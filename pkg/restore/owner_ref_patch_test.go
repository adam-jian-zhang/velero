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
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

func TestMergeOwnerReferences(t *testing.T) {
	isControllerTrue := true
	isControllerFalse := false

	live := []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "rs-1",
			UID:        "live-rs-uid",
			Controller: &isControllerTrue,
		},
		{
			APIVersion: "custom.io/v1",
			Kind:       "CustomManager",
			Name:       "cm-1",
			UID:        "live-cm-uid",
			Controller: &isControllerFalse,
		},
	}

	remapped := []metav1.OwnerReference{
		// Should update rs-1 UID and flags
		{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "rs-1",
			UID:        "new-rs-uid",
			Controller: &isControllerTrue,
		},
		// Should append new-parent
		{
			APIVersion: "cluster.x-k8s.io/v1beta1",
			Kind:       "MachineSet",
			Name:       "ms-1",
			UID:        "new-ms-uid",
			Controller: &isControllerFalse,
		},
	}

	merged := mergeOwnerReferences(live, remapped)
	require.Len(t, merged, 3)

	// Verify rs-1 updated
	assert.Equal(t, types.UID("new-rs-uid"), merged[0].UID)
	// Verify live-cm-uid preserved (unmanaged live ref)
	assert.Equal(t, types.UID("live-cm-uid"), merged[1].UID)
	// Verify new parent appended
	assert.Equal(t, types.UID("new-ms-uid"), merged[2].UID)

	// Test single controller invariant: appending a new controller clears prior controller flags
	newControllerParent := []metav1.OwnerReference{
		{
			APIVersion: "cluster.x-k8s.io/v1beta1",
			Kind:       "MachineDeployment",
			Name:       "md-1",
			UID:        "md-uid",
			Controller: &isControllerTrue,
		},
	}
	mergedWithNewController := mergeOwnerReferences(merged, newControllerParent)
	require.Len(t, mergedWithNewController, 4)
	assert.Nil(t, mergedWithNewController[0].Controller, "prior controller flag on rs-1 should be cleared")
	require.NotNil(t, mergedWithNewController[3].Controller)
	assert.True(t, *mergedWithNewController[3].Controller, "new controller md-1 should have Controller: true")

	// Test cross-version group matching (e.g. v1alpha4 backed up vs v1beta1 live)
	crossVersionRemap := []metav1.OwnerReference{
		{
			APIVersion: "cluster.x-k8s.io/v1alpha4",
			Kind:       "MachineSet",
			Name:       "ms-1",
			UID:        "updated-ms-uid",
		},
	}
	mergedCrossVersion := mergeOwnerReferences(merged, crossVersionRemap)
	require.Len(t, mergedCrossVersion, 3, "should match on Group/Kind/Name and not create duplicate entry")
	assert.Equal(t, types.UID("updated-ms-uid"), mergedCrossVersion[2].UID)
}

func TestGenerateOwnerRefMergePatch(t *testing.T) {
	refs := []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "rs-1",
			UID:        "uid-1",
		},
	}
	patchBytes, err := generateOwnerRefMergePatch("42", refs)
	require.NoError(t, err)
	patchStr := string(patchBytes)
	assert.Contains(t, patchStr, `"resourceVersion":"42"`)
	assert.Contains(t, patchStr, `"ownerReferences"`)
}

func TestApplyOwnerRefRemapping_NamespaceSplit(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	state.RegisterUIDMapping("old-parent-uid", "new-parent-uid")

	req := ownerref.OwnerPatchRequest{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Resource:  "machines",
		Namespace: "target-ns-1",
		Name:      "machine-1",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "cluster.x-k8s.io/v1beta1",
				Kind:       "MachineSet",
				Name:       "ms-1",
				UID:        "old-parent-uid",
			},
		},
		// Owner source namespace is src-ns-parent
		OwnerRefSourceNS: []string{"src-ns-parent"},
	}
	state.EnqueueOwnerPatch(req)

	// Restore mapping maps src-ns-parent to target-ns-2, but child is in target-ns-1!
	restore := &velerov1api.Restore{
		Spec: velerov1api.RestoreSpec{
			NamespaceMapping: map[string]string{
				"src-ns-parent": "target-ns-2",
			},
		},
	}

	warnings := ApplyOwnerRefRemapping(context.Background(), logrus.StandardLogger(), fakeClient, restore, state)
	assert.True(t, warnings.IsEmpty())
	// Machine should have been skipped because namespace mapping split owner and child
	// and since it cannot be patched, queue should be empty (no parents remaining to resolve)
}

func TestApplyOwnerRefRemapping_MultiParentAndTwoPass(t *testing.T) {
	scheme := runtime.NewScheme()
	childObj := &unstructured.Unstructured{}
	childObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Machine",
	})
	childObj.SetName("worker-1")
	childObj.SetNamespace("default")

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true

	// Parent 1 resolved in Pass 1, Parent 2 NOT resolved yet:
	state.RegisterUIDMapping("parent-1-old", "parent-1-new")

	isController := true
	req := ownerref.OwnerPatchRequest{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Resource:  "machines",
		Namespace: "default",
		Name:      "worker-1",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "cluster.x-k8s.io/v1beta1",
				Kind:       "MachineSet",
				Name:       "ms-1",
				UID:        "parent-1-old",
				Controller: &isController,
			},
			{
				APIVersion: "cluster.x-k8s.io/v1beta1",
				Kind:       "Cluster",
				Name:       "cluster-1",
				UID:        "parent-2-old",
			},
		},
		OwnerRefSourceNS: []string{"default", "default"},
	}
	state.EnqueueOwnerPatch(req)

	restore := &velerov1api.Restore{}

	// Pass 1:
	warnings := ApplyOwnerRefRemapping(context.Background(), logrus.StandardLogger(), fakeClient, restore, state)
	assert.True(t, warnings.IsEmpty())

	// Because Parent 2 was unmapped, req remains in OwnerPatchQueue for Pass 2:
	require.Len(t, state.GetOwnerPatchQueue(), 1)

	// Check child on fakeClient: should have Parent 1 patched!
	liveChild := &unstructured.Unstructured{}
	liveChild.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "worker-1"}, liveChild)
	require.NoError(t, err)
	require.Len(t, liveChild.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("parent-1-new"), liveChild.GetOwnerReferences()[0].UID)

	// Now simulate Pass 2: Parent 2 becomes resolved:
	state.RegisterUIDMapping("parent-2-old", "parent-2-new")
	warningsPass2 := ApplyOwnerRefRemapping(context.Background(), logrus.StandardLogger(), fakeClient, restore, state)
	assert.True(t, warningsPass2.IsEmpty())

	// Queue should now be empty!
	assert.Empty(t, state.GetOwnerPatchQueue())

	// Check child on fakeClient: should have BOTH parents via additive merge!
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "worker-1"}, liveChild)
	require.NoError(t, err)
	require.Len(t, liveChild.GetOwnerReferences(), 2)
	assert.Equal(t, types.UID("parent-1-new"), liveChild.GetOwnerReferences()[0].UID)
	assert.Equal(t, types.UID("parent-2-new"), liveChild.GetOwnerReferences()[1].UID)
}
