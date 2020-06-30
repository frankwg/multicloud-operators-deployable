// Copyright 2019 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployable

import (
	"context"

	appv1alpha1 "github.com/open-cluster-management/multicloud-operators-deployable/pkg/apis/apps/v1"
	"github.com/open-cluster-management/multicloud-operators-deployable/pkg/utils"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *ReconcileDeployable) handleDeployable(instance *appv1alpha1.Deployable) error {
	if klog.V(utils.QuiteLogLel) {
		fnName := utils.GetFnName()
		klog.Infof("Entering: %v()", fnName)

		defer klog.Infof("Exiting: %v()", fnName)
	}

	// propagate subscription-pause label to its subscription template
	err := utils.SetPauseLabelDplSubTpl(instance, instance)
	if err != nil {
		klog.Info("Failed to propagate pause label to new local deployable subscription template. err:", err)
		return err
	}

	// try to find children
	children, err := r.getDeployableFamily(instance)

	if err != nil {
		klog.Error("Failed to get children deployable with err:", err)
	}

	// actively delete children deployables when change from hub to local only
	if len(instance.GetFinalizers()) > 0 || instance.Spec.Placement == nil {
		for _, dpl := range children {
			dplkey := types.NamespacedName{Namespace: dpl.GetNamespace(), Name: dpl.GetName()}

			klog.V(5).Info("As hub, deleting ", dpl.GetNamespace(), "/", dpl.GetName())

			if dpl.Namespace != instance.Namespace {
				err = r.Delete(context.TODO(), dpl)

				addtionalMsg := "Delete propogated Deployable " + dplkey.String()
				r.eventRecorder.RecordEvent(instance, "Delete", addtionalMsg, err)
			}
		}

		instance.Status.PropagatedStatus = nil

		return nil
	}

	// hub reconcile always has in cluster status
	if instance.Status.PropagatedStatus == nil {
		instance.Status.PropagatedStatus = make(map[string]*appv1alpha1.ResourceUnitStatus)
	}

	// prepare map to delete expired children
	expireddeployablemap := make(map[string]*appv1alpha1.Deployable)

	for _, dpl := range children {
		expireddeployablemap[getDeployableTrueKey(dpl)] = dpl

		if utils.GetClusterFromResourceObject(dpl).Name != "" {
			instance.Status.PropagatedStatus[utils.GetClusterFromResourceObject(dpl).Name] = dpl.Status.ResourceUnitStatus.DeepCopy()
			klog.V(5).Infof("child dpl cluster name: %v, unit status: %#v", utils.GetClusterFromResourceObject(dpl).Name, dpl.Status.ResourceUnitStatus.DeepCopy())
		}
	}
	// instance itself does not expire anyway
	delete(expireddeployablemap, getDeployableTrueKey(instance))
	klog.V(1).Info("Existing deployables to check expiration:", expireddeployablemap)

	err = r.rollingUpdate(instance)

	if err != nil {
		klog.Error("Error in rolling update:", err)
		return err
	}

	// // Generate deployable for managed clusters
	clusters, err := r.getClustersByPlacement(instance)

	if err != nil {
		klog.Error("Error in getting clusters:", err)
		return err
	}

	// propagate template
	expireddeployablemap, err = r.propagateDeployables(clusters, instance, expireddeployablemap)
	if err != nil {
		klog.Error("Error in propagating to clusters:", err)
		return err
	}

	// delete expired deployables
	klog.V(5).Info("Expired deployables map:", expireddeployablemap)

	for _, dpl := range expireddeployablemap {
		delete(instance.Status.PropagatedStatus, utils.GetClusterFromResourceObject(dpl).Name)
		dplanno := dpl.GetAnnotations()

		if dplanno == nil && dplanno[appv1alpha1.AnnotationShared] == "true" {
			continue
		}

		dplkey := types.NamespacedName{Namespace: dpl.GetNamespace(), Name: dpl.GetName()}
		err = r.Delete(context.TODO(), dpl)

		addtionalMsg := "Delete Expired Deployable " + dplkey.String()
		r.eventRecorder.RecordEvent(instance, "Delete", addtionalMsg, err)

		if err != nil {
			klog.Error("Failed to delete local deployable ", dpl.GetNamespace(), "/", dpl.GetName(), ":", err, "skipping")
		}
	}

	//remove expired clusters from instance status targetClusters list
	clusterStatusMap := instance.Status.PropagatedStatus
	for clusterName := range clusterStatusMap {
		if !utils.ContainsName(clusters, clusterName) {
			delete(instance.Status.PropagatedStatus, clusterName)
		}
	}

	// delete invalid overrides generated by rolling update
	r.validateOverridesForRollingUpdate(instance)

	instance.Status.Phase = appv1alpha1.DeployablePropagated
	instance.Status.Reason = ""

	klog.V(5).Infof("Exit hub func with err: %v, and instance status: %#v", err, instance.Status)

	return err
}

func (r *ReconcileDeployable) getDeployableFamily(instance *appv1alpha1.Deployable) ([]*appv1alpha1.Deployable, error) {
	if klog.V(utils.QuiteLogLel) {
		fnName := utils.GetFnName()
		klog.Infof("Entering: %v()", fnName)

		defer klog.Infof("Exiting: %v()", fnName)
	}
	// get all existing deployables
	exlist := &appv1alpha1.DeployableList{}
	exlabel := make(map[string]string)
	// Label does not support "/" for NamespacedName, let get by name first and filter by annotation later
	exlabel[appv1alpha1.PropertyHostingDeployableName] = instance.GetName()
	err := r.List(context.TODO(), exlist, client.MatchingLabels(exlabel))

	if err != nil && !errors.IsNotFound(err) {
		klog.Error("Trying to list existing deployabe ", instance.GetNamespace(), "/", instance.GetName(), " with error:", err)
		return nil, err
	}

	var dpllist []*appv1alpha1.Deployable

	hosting := (types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()}).String()

	for _, dpl := range exlist.Items {
		dplanno := dpl.GetAnnotations()

		if dplanno == nil {
			continue
		}

		if dplanno[appv1alpha1.AnnotationHosting] == hosting {
			dpllist = append(dpllist, dpl.DeepCopy())
		}
	}

	return dpllist, nil
}

func getDeployableTrueKey(dpl *appv1alpha1.Deployable) string {
	if klog.V(utils.QuiteLogLel) {
		fnName := utils.GetFnName()
		klog.Infof("Entering: %v()", fnName)

		defer klog.Infof("Exiting: %v()", fnName)
	}

	objkey := types.NamespacedName{Name: dpl.Name, Namespace: dpl.Namespace}

	if dpl.GetGenerateName() != "" {
		objkey.Name = dpl.GetGenerateName()
	}

	return objkey.String()
}

// validateDeployables validate parent deployable exist or not. The deployables with empty parent will be removed.
func (r *ReconcileDeployable) validateDeployables() error {
	deployablelist := &appv1alpha1.DeployableList{}
	listopts := &client.ListOptions{}
	err := r.List(context.TODO(), deployablelist, listopts)

	if err != nil {
		klog.Error("Failed to obtain deployable list")
		return err
	}

	// construct a map to make things easier
	deployableMap := make(map[string]*appv1alpha1.Deployable)
	for _, dpl := range deployablelist.Items {
		deployableMap[(types.NamespacedName{Name: dpl.GetName(), Namespace: dpl.GetNamespace()}).String()] = dpl.DeepCopy()
		klog.V(5).Info("validateDeployables() dpl: ", dpl.GetNamespace(), " ", dpl.GetName())
	}

	// check each deployable for parents
	for k, v := range deployableMap {
		obj := v.DeepCopy()

		annotations := obj.GetAnnotations()
		if annotations == nil {
			// newly added not processed yet break this loop
			break
		}

		hostDpl := obj

		for hostDpl != nil {
			host := utils.GetHostDeployableFromObject(hostDpl)
			if host == nil {
				break
			}

			klog.V(5).Infof("obj: %#v, hosting deployable: %#v", obj.GetNamespace()+"/"+obj.GetName(), host)

			ok := false
			hostDpl, ok = deployableMap[host.String()]

			if ok {
				if host.Namespace == obj.GetNamespace() && host.Name == obj.GetName() {
					break
				}
			} else {
				// parent is gone, delete the deployable from map and from kube
				delete(deployableMap, k)

				err = r.Delete(context.TODO(), obj)
				klog.V(5).Infof("parent is gone, delete the deployable from map and from kube: host: %#v, k: %#v, v: %#v, err: %#v", host, k, v, err)

				break
			}
		}
	}

	return nil
}
