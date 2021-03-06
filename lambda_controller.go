/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/metacontroller/apis/metacontroller/v1alpha1"
	k8s "k8s.io/metacontroller/third_party/kubernetes"
)

func syncAllLambdaControllers(clientset *dynamicClientset) error {
	lcClient, err := clientset.Resource(v1alpha1.SchemeGroupVersion.String(), "lambdacontrollers", "")
	if err != nil {
		return err
	}
	obj, err := lcClient.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("can't list LambdaControllers: %v", err)
	}
	lcList := obj.(*unstructured.UnstructuredList)

	for i := range lcList.Items {
		data, err := json.Marshal(&lcList.Items[i])
		if err != nil {
			glog.Errorf("can't marshal LambdaController: %v")
			continue
		}
		lc := &v1alpha1.LambdaController{}
		if err := json.Unmarshal(data, lc); err != nil {
			glog.Errorf("can't unmarshal LambdaController: %v", err)
			continue
		}
		if err := syncLambdaController(clientset, lc); err != nil {
			glog.Errorf("syncLambdaController: %v", err)
			continue
		}
	}
	return nil
}

func syncLambdaController(clientset *dynamicClientset, lc *v1alpha1.LambdaController) error {
	// Sync all objects of the parent type, in all namespaces.
	parentClient, err := clientset.Resource(lc.Spec.ParentResource.APIVersion, lc.Spec.ParentResource.Resource, "")
	if err != nil {
		return err
	}
	obj, err := parentClient.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("can't list %vs: %v", parentClient.Kind(), err)
	}
	list := obj.(*unstructured.UnstructuredList)
	for i := range list.Items {
		parent := &list.Items[i]
		if err := syncParentResource(clientset, lc, parentClient.APIResource(), parent); err != nil {
			glog.Errorf("can't sync %v %v/%v: %v", parentClient.Kind(), parent.GetNamespace(), parent.GetName(), err)
			continue
		}
	}

	return nil
}

func syncParentResource(clientset *dynamicClientset, lc *v1alpha1.LambdaController, parentResource *APIResource, parent *unstructured.Unstructured) error {
	// Get the parent's LabelSelector.
	labelSelector := &metav1.LabelSelector{}
	if err := k8s.GetNestedFieldInto(&labelSelector, parent.UnstructuredContent(), "spec", "selector"); err != nil {
		return fmt.Errorf("can't get label selector from %v %v/%v", parentResource.Kind, parent.GetNamespace(), parent.GetName())
	}

	// Claim all matching child resources, including orphan/adopt as necessary.
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return fmt.Errorf("can't convert label selector (%#v): %v", labelSelector, err)
	}
	children, err := claimChildren(clientset, lc, parentResource, parent, selector)
	if err != nil {
		return fmt.Errorf("can't claim children: %v", err)
	}

	// Call the sync hook for this parent.
	syncRequest := &syncHookRequest{
		Parent:   parent,
		Children: children,
	}
	syncResult, err := callSyncHook(lc, syncRequest)
	if err != nil {
		return fmt.Errorf("sync hook failed for %v %v/%v: %v", parentResource.Kind, parent.GetNamespace(), parent.GetName(), err)
	}

	// Remember manage error, but continue to update status regardless.
	var manageErr error
	if parent.GetDeletionTimestamp() == nil {
		// Reconcile children.
		if err := manageChildren(clientset, lc, parent, children, makeChildMap(syncResult.Children)); err != nil {
			manageErr = fmt.Errorf("can't reconcile children for %v %v/%v: %v", parentResource.Kind, parent.GetNamespace(), parent.GetName(), err)
		}
	}

	// Update parent status.
	// We'll want to make sure this happens after manageChildren once we support observedGeneration.
	if err := updateParentStatus(clientset, lc, parentResource, parent, syncResult.Status); err != nil {
		return fmt.Errorf("can't update status for %v %v/%v: %v", parentResource.Kind, parent.GetNamespace(), parent.GetName(), err)
	}

	return manageErr
}

func claimChildren(clientset *dynamicClientset, lc *v1alpha1.LambdaController, parentResource *APIResource, parent *unstructured.Unstructured, selector labels.Selector) (childMap, error) {
	// Set up values common to all child types.
	parentGVK := parentResource.GroupVersionKind()
	parentClient, err := clientset.Resource(parentResource.APIVersion, parentResource.Name, parent.GetNamespace())
	if err != nil {
		return nil, err
	}
	canAdoptFunc := k8s.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		// Make sure this is always an uncached read.
		fresh, err := parentClient.Get(parent.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.GetUID() != parent.GetUID() {
			return nil, fmt.Errorf("original %v %v/%v is gone: got uid %v, wanted %v", parentResource.Kind, parent.GetNamespace(), parent.GetName(), fresh.GetUID(), parent.GetUID())
		}
		return fresh, nil
	})

	// Claim all child types.
	groups := make(childMap)
	for _, group := range lc.Spec.ChildResources {
		// Within each group/version, there can be multiple resources requested.
		for _, resourceName := range group.Resources {
			// List all objects of the child kind in the parent object's namespace.
			childClient, err := clientset.Resource(group.APIVersion, resourceName, parent.GetNamespace())
			if err != nil {
				return nil, err
			}
			obj, err := childClient.List(metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("can't list %v children: %v", childClient.Kind(), err)
			}
			childList := obj.(*unstructured.UnstructuredList)

			// Handle orphan/adopt and filter by owner+selector.
			crm := newDynamicControllerRefManager(childClient, parent, selector, parentGVK, childClient.GroupVersionKind(), canAdoptFunc)
			children, err := crm.claimChildren(childList.Items)
			if err != nil {
				return nil, fmt.Errorf("can't claim %v children: %v", childClient.Kind(), err)
			}

			// Add children to map by name.
			// Note that we limit each parent to only working within its own namespace.
			groupMap := make(map[string]*unstructured.Unstructured)
			for _, child := range children {
				groupMap[child.GetName()] = child
			}
			groups[fmt.Sprintf("%s.%s", childClient.Kind(), group.APIVersion)] = groupMap
		}
	}
	return groups, nil
}

func updateParentStatus(clientset *dynamicClientset, lc *v1alpha1.LambdaController, parentResource *APIResource, parent *unstructured.Unstructured, status map[string]interface{}) error {
	parentClient, err := clientset.Resource(parentResource.APIVersion, parentResource.Name, parent.GetNamespace())
	if err != nil {
		return err
	}
	// Overwrite .status field of parent object without touching other parts.
	// We can't use Patch() because we need to ensure that the UID matches.
	// TODO(enisoc): Use /status subresource when that exists.
	// TODO(enisoc): Update status.observedGeneration when spec.generation starts working.
	return parentClient.UpdateWithRetries(parent, func(obj *unstructured.Unstructured) bool {
		oldStatus := k8s.GetNestedField(obj.UnstructuredContent(), "status")
		if reflect.DeepEqual(oldStatus, status) {
			// Nothing to do.
			return false
		}
		k8s.SetNestedField(obj.UnstructuredContent(), status, "status")
		return true
	})
}

func deleteChildren(client *dynamicResourceClient, parent *unstructured.Unstructured, observed, desired map[string]*unstructured.Unstructured) error {
	var errs []error
	for name, obj := range observed {
		if obj.GetDeletionTimestamp() != nil {
			// Skip objects that are already pending deletion.
			continue
		}
		if desired == nil || desired[name] == nil {
			// This observed object wasn't listed as desired.
			glog.Infof("%v %v/%v: deleting %v %v", parent.GetKind(), parent.GetNamespace(), parent.GetName(), obj.GetKind(), obj.GetName())
			uid := obj.GetUID()
			err := client.Delete(name, &metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("can't delete %v %v/%v: %v", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err))
				continue
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func updateChildren(client *dynamicResourceClient, parent *unstructured.Unstructured, observed, desired map[string]*unstructured.Unstructured) error {
	var errs []error
	for name, obj := range desired {
		if oldObj := observed[name]; oldObj != nil {
			// Update
			if !reflect.DeepEqual(obj.UnstructuredContent(), oldObj.UnstructuredContent()) {
				glog.Infof("%v %v/%v: updating %v %v", parent.GetKind(), parent.GetNamespace(), parent.GetName(), obj.GetKind(), obj.GetName())
				if _, err := client.Update(obj); err != nil {
					errs = append(errs, err)
					continue
				}
			}
		} else {
			// Create
			glog.Infof("%v %v/%v: creating %v %v", parent.GetKind(), parent.GetNamespace(), parent.GetName(), obj.GetKind(), obj.GetName())
			controllerRef := map[string]interface{}{
				"apiVersion":         parent.GetAPIVersion(),
				"kind":               parent.GetKind(),
				"name":               parent.GetName(),
				"uid":                string(parent.GetUID()),
				"controller":         true,
				"blockOwnerDeletion": true,
			}
			ownerRefs, _ := k8s.GetNestedField(obj.UnstructuredContent(), "metadata", "ownerReferences").([]interface{})
			ownerRefs = append(ownerRefs, controllerRef)
			k8s.SetNestedField(obj.UnstructuredContent(), "metadata", "ownerReferences")
			if _, err := client.Create(obj); err != nil {
				errs = append(errs, err)
				continue
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func manageChildren(clientset *dynamicClientset, lc *v1alpha1.LambdaController, parent *unstructured.Unstructured, observedChildren childMap, desiredChildren childMap) error {
	// If some operations fail, keep trying others so, for example,
	// we don't block recovery (create new Pod) on a failed delete.
	var errs []error

	// Delete observed, owned objects that are not desired.
	for key, objects := range observedChildren {
		apiVersion, kind := parseChildMapKey(key)
		client, err := clientset.Kind(apiVersion, kind, parent.GetNamespace())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := deleteChildren(client, parent, objects, desiredChildren[key]); err != nil {
			errs = append(errs, err)
			continue
		}
	}

	// Create or update desired objects.
	for key, objects := range desiredChildren {
		apiVersion, kind := parseChildMapKey(key)
		client, err := clientset.Kind(apiVersion, kind, parent.GetNamespace())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := updateChildren(client, parent, observedChildren[key], objects); err != nil {
			errs = append(errs, err)
			continue
		}
	}

	return utilerrors.NewAggregate(errs)
}

func makeChildMap(list []*unstructured.Unstructured) childMap {
	children := make(childMap)
	for _, child := range list {
		apiVersion := k8s.GetNestedString(child.UnstructuredContent(), "apiVersion")
		kind := k8s.GetNestedString(child.UnstructuredContent(), "kind")
		key := fmt.Sprintf("%s.%s", kind, apiVersion)

		if children[key] == nil {
			children[key] = make(map[string]*unstructured.Unstructured)
		}
		children[key][child.GetName()] = child
	}
	return children
}

func parseChildMapKey(key string) (apiVersion, kind string) {
	parts := strings.SplitN(key, ".", 2)
	return parts[1], parts[0]
}
