/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package drain

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes"
)

// CordonHelper wraps functionality to cordon/uncordon nodes
type CordonHelper struct {
	object runtime.Object
	node   *corev1.Node

	oldData []byte
}

// NewCordonHelper returns a new CordonHelper, or an error if given object is not a
// node or cannot be encoded as JSON
func NewCordonHelper(nodeObject runtime.Object, scheme *runtime.Scheme, gvk schema.GroupVersionKind) (*CordonHelper, error) {
	nodeObject, err := scheme.ConvertToVersion(nodeObject, gvk.GroupVersion())
	if err != nil {
		return nil, err
	}

	// serialize original data to JSON for use to create a patch later
	oldData, err := json.Marshal(nodeObject)
	if err != nil {
		return nil, err
	}

	node, ok := nodeObject.(*corev1.Node)
	if !ok {
		return nil, fmt.Errorf("unexpected Type%T, expected Node\n", nodeObject)
	}

	c := &CordonHelper{
		node:    node,
		object:  nodeObject,
		oldData: oldData,
	}

	return c, nil
}

// SetUnschedulableIfNeeded sets node.Spec.Unschedulable = desired and returns true,
// or false when change isn't needed
func (c *CordonHelper) SetUnschedulableIfNeeded(desired bool) bool {
	if c.node.Spec.Unschedulable == desired {
		return false
	}
	c.node.Spec.Unschedulable = desired
	return true
}

// PatchOrReplace uses given clientset to update the node status, either by patching or
// updating the given node object; it may return error if the object cannot be encoded as
// JSON, or if either patch or update calls fail; it will also return a second error
// whenever creating a patch has failed
func (c *CordonHelper) PatchOrReplace(clientset kubernetes.Interface) (error, error) {
	client := clientset.Core().Nodes()

	newData, err := json.Marshal(c.object)
	if err != nil {
		return err, nil
	}

	patchBytes, patchErr := strategicpatch.CreateTwoWayMergePatch(c.oldData, newData, c.object)
	if patchErr == nil {
		_, err = client.Patch(c.node.Name, types.StrategicMergePatchType, patchBytes)
	} else {
		_, err = client.Update(c.node)
	}
	return err, patchErr
}
