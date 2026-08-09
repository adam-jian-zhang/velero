/*
Copyright The Velero Contributors.

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

package restore

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/dag"
	"github.com/vmware-tanzu/velero/pkg/features"
)

func TestRegisterAndEnqueueInScope(t *testing.T) {
	features.NewFeatureFlagSet(velerov1api.OwnerReferenceDAGFeatureFlag)
	defer features.NewFeatureFlagSet()

	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = true
	state.SetScope(dag.NewScope())

	backup := &unstructured.Unstructured{}
	backup.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	backup.SetKind("Machine")
	backup.SetNamespace("default")
	backup.SetName("m1")
	backup.SetUID("old-child")
	controller := true
	backup.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "cluster.x-k8s.io/v1beta1",
		Kind:       "MachineSet",
		Name:       "ms1",
		UID:        "old-parent",
		Controller: &controller,
	}})

	live := backup.DeepCopy()
	live.SetUID("new-child")

	state.registerAndMaybeEnqueue(backup, live, schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "machines"},
		copyOwnerReferences(backup.GetOwnerReferences()), []string{"default"})

	assert.Equal(t, types.UID("new-child"), state.UIDMap["old-child"])
	require.Len(t, state.OwnerPatchQueue, 1)
	assert.Equal(t, "Machine", state.OwnerPatchQueue[0].Kind)
	assert.True(t, *state.OwnerPatchQueue[0].OriginalOwnerRefs[0].Controller)
}

func TestRegisterDoesNotEnqueueDeniedWorkload(t *testing.T) {
	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = true
	state.SetScope(dag.NewScope())

	backup := &unstructured.Unstructured{}
	backup.SetAPIVersion("apps/v1")
	backup.SetKind("Deployment")
	backup.SetNamespace("default")
	backup.SetName("d1")
	backup.SetUID("old")
	backup.SetOwnerReferences([]metav1.OwnerReference{{UID: "owner", Kind: "Something", Name: "x"}})
	live := backup.DeepCopy()
	live.SetUID("new")

	state.registerAndMaybeEnqueue(backup, live, schema.GroupResource{Group: "apps", Resource: "deployments"},
		copyOwnerReferences(backup.GetOwnerReferences()), []string{"default"})

	assert.Equal(t, types.UID("new"), state.UIDMap["old"])
	assert.Empty(t, state.OwnerPatchQueue)
}

func TestOwnerPatchQueueConcurrentEnqueue(t *testing.T) {
	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = true
	state.SetScope(dag.NewScope())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			backup := &unstructured.Unstructured{}
			backup.SetAPIVersion("cluster.x-k8s.io/v1beta1")
			backup.SetKind("Machine")
			backup.SetNamespace("default")
			backup.SetName("m")
			backup.SetUID(types.UID(fmt.Sprintf("old-%d", i)))
			backup.SetOwnerReferences([]metav1.OwnerReference{{UID: "p", Kind: "MachineSet", Name: "ms"}})
			live := backup.DeepCopy()
			live.SetUID(types.UID(fmt.Sprintf("new-%d", i)))
			state.registerAndMaybeEnqueue(backup, live, schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "machines"},
				copyOwnerReferences(backup.GetOwnerReferences()), []string{"default"})
		}(i)
	}
	wg.Wait()
	assert.Len(t, state.OwnerPatchQueue, 50)
}

func TestPatchOwnerReferencesRemapsUID(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, velerov1api.AddToScheme(scheme))

	controller := true
	block := true
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	live.SetNamespace("default")
	live.SetName("m1")
	live.SetUID("live-uid")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()

	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = true
	state.UIDMap["old-parent"] = "new-parent"

	req := OwnerPatchRequest{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Resource:  "machines",
		Namespace: "default",
		Name:      "m1",
		OriginalOwnerRefs: []metav1.OwnerReference{{
			APIVersion:         "cluster.x-k8s.io/v1beta1",
			Kind:               "MachineSet",
			Name:               "ms1",
			UID:                "old-parent",
			Controller:         &controller,
			BlockOwnerDeletion: &block,
		}},
		OwnerRefSourceNS: []string{"default"},
	}

	err := patchOwnerReferences(context.Background(), logrus.New(), cl, state, req, nil)
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(live.GroupVersionKind())
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "m1"}, got))
	refs := got.GetOwnerReferences()
	require.Len(t, refs, 1)
	assert.Equal(t, types.UID("new-parent"), refs[0].UID)
	assert.True(t, *refs[0].Controller)
	assert.True(t, *refs[0].BlockOwnerDeletion)
}

func TestPatchOwnerReferencesSkipsNamespaceSplit(t *testing.T) {
	scheme := runtime.NewScheme()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	// req.Namespace is already the live/target namespace after Phase 1A remapping.
	live.SetNamespace("target-b")
	live.SetName("m1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()

	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = true
	state.UIDMap["old-parent"] = "new-parent"

	req := OwnerPatchRequest{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Namespace: "target-b",
		Name:      "m1",
		OriginalOwnerRefs: []metav1.OwnerReference{{
			Kind: "MachineSet",
			Name: "ms1",
			UID:  "old-parent",
		}},
		OwnerRefSourceNS: []string{"ns-a"},
	}

	err := patchOwnerReferences(context.Background(), logrus.New(), cl, state, req, map[string]string{
		"ns-a": "target-a",
		"ns-b": "target-b",
	})
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(live.GroupVersionKind())
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "target-b", Name: "m1"}, got))
	assert.Empty(t, got.GetOwnerReferences())
}

func TestPatchOwnerReferencesSameMappedNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	live.SetNamespace("target")
	live.SetName("m1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()

	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = true
	state.UIDMap["old-parent"] = "new-parent"

	req := OwnerPatchRequest{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Namespace: "target",
		Name:      "m1",
		OriginalOwnerRefs: []metav1.OwnerReference{{
			Kind: "MachineSet",
			Name: "ms1",
			UID:  "old-parent",
		}},
		OwnerRefSourceNS: []string{"src"},
	}

	err := patchOwnerReferences(context.Background(), logrus.New(), cl, state, req, map[string]string{
		"src": "target",
	})
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(live.GroupVersionKind())
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "target", Name: "m1"}, got))
	require.Len(t, got.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("new-parent"), got.GetOwnerReferences()[0].UID)
}

func TestRemapSpecRefFields(t *testing.T) {
	state := NewOwnerRefRemapState()
	state.UIDMap["old-uid"] = "new-uid"

	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"infrastructureRef": map[string]any{
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
				"kind":       "VSphereMachine",
				"name":       "vm",
				"namespace":  "src",
				"uid":        "old-uid",
			},
		},
	}}
	obj.SetName("cluster")
	obj.SetNamespace("src")

	changed, patchObj, err := remapSpecRefFields(obj, []string{"spec.infrastructureRef"}, state, map[string]string{"src": "dst"}, logrus.New())
	require.NoError(t, err)
	assert.True(t, changed)

	ref, _, _ := unstructured.NestedMap(obj.Object, "spec", "infrastructureRef")
	assert.Equal(t, "new-uid", ref["uid"])
	assert.Equal(t, "dst", ref["namespace"])

	patchRef, ok := patchObj["spec"].(map[string]any)["infrastructureRef"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new-uid", patchRef["uid"])
	assert.Equal(t, "dst", patchRef["namespace"])
}

func TestApplyOwnerRefRemappingNoopWhenDisabled(t *testing.T) {
	warnings := ApplyOwnerRefRemapping(context.Background(), logrus.New(), nil, nil, nil)
	assert.True(t, warnings.IsEmpty())

	state := NewOwnerRefRemapState()
	state.Enabled = true
	state.ResourceDAGPresent = false
	warnings = ApplyOwnerRefRemapping(context.Background(), logrus.New(), nil, &velerov1api.Restore{}, state)
	assert.True(t, warnings.IsEmpty())
}
