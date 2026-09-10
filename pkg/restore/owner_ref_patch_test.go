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
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	merged := mergeOwnerReferences(live, remapped, logrus.StandardLogger(), "default", "pod-1")
	require.Len(t, merged, 3)

	// Verify rs-1 updated
	assert.Equal(t, types.UID("new-rs-uid"), merged[0].UID)
	// Verify live-cm-uid preserved (unmanaged live ref)
	assert.Equal(t, types.UID("live-cm-uid"), merged[1].UID)
	// Verify new parent appended
	assert.Equal(t, types.UID("new-ms-uid"), merged[2].UID)

	// Test Live Precedence: when live object already has a controlling owner (rs-1),
	// incoming new controller parent (md-1) must be demoted to Controller: nil to preserve API invariant.
	newControllerParent := []metav1.OwnerReference{
		{
			APIVersion: "cluster.x-k8s.io/v1beta1",
			Kind:       "MachineDeployment",
			Name:       "md-1",
			UID:        "md-uid",
			Controller: &isControllerTrue,
		},
	}
	mergedWithNewController := mergeOwnerReferences(merged, newControllerParent, logrus.StandardLogger(), "default", "pod-1")
	require.Len(t, mergedWithNewController, 4)
	// Live controller rs-1 must retain Controller: true
	require.NotNil(t, mergedWithNewController[0].Controller)
	assert.True(t, *mergedWithNewController[0].Controller, "live controller rs-1 must retain Controller: true")
	// Incoming controller md-1 must be demoted to nil
	assert.Nil(t, mergedWithNewController[3].Controller, "incoming controller md-1 should be demoted to nil due to Live Precedence")

	// Test Live Precedence for existing ref promotion:
	// ms-1 exists with Controller: false. If remapped incoming ms-1 has Controller: true,
	// it must NOT be promoted because rs-1 is already the live controller.
	promoteExisting := []metav1.OwnerReference{
		{
			APIVersion: "cluster.x-k8s.io/v1beta1",
			Kind:       "MachineSet",
			Name:       "ms-1",
			UID:        "new-ms-uid",
			Controller: &isControllerTrue,
		},
	}
	mergedPromote := mergeOwnerReferences(merged, promoteExisting, logrus.StandardLogger(), "default", "pod-1")
	require.Len(t, mergedPromote, 3)
	assert.True(t, *mergedPromote[0].Controller, "rs-1 remains controller")
	assert.False(t, *mergedPromote[2].Controller, "ms-1 must not be promoted to controller")

	// Test cross-version group matching (e.g. v1alpha4 backed up vs v1beta1 live)
	crossVersionRemap := []metav1.OwnerReference{
		{
			APIVersion: "cluster.x-k8s.io/v1alpha4",
			Kind:       "MachineSet",
			Name:       "ms-1",
			UID:        "updated-ms-uid",
		},
	}
	mergedCrossVersion := mergeOwnerReferences(merged, crossVersionRemap, logrus.StandardLogger(), "default", "pod-1")
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

	warnings, pending := ApplyOwnerRefRemapping(context.Background(), logrus.StandardLogger(), fakeClient, restore, state)
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, pending)
	// Machine should have been skipped because namespace mapping split owner and child
	assert.Empty(t, state.GetOwnerPatchQueue())
}

func TestApplyOwnerRefRemapping_MultiParentAndMissingParentOmitted(t *testing.T) {
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

	// Parent 1 resolved in Pass 1, Parent 2 is permanently missing (excluded from restore):
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
				Name:       "cluster-excluded",
				UID:        "parent-2-old",
			},
		},
		OwnerRefSourceNS: []string{"default", "default"},
	}
	state.EnqueueOwnerPatch(req)

	restore := &velerov1api.Restore{}

	warnings, pending := ApplyOwnerRefRemapping(context.Background(), logrus.StandardLogger(), fakeClient, restore, state)
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, pending)

	// Because Parent 2 was missing/excluded, it was recognized as a permanent omission and omitted.
	// Queue should now be empty (not retained in queue!):
	assert.Empty(t, state.GetOwnerPatchQueue())

	// Check child on fakeClient: should have Parent 1 patched!
	liveChild := &unstructured.Unstructured{}
	liveChild.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "worker-1"}, liveChild)
	require.NoError(t, err)
	require.Len(t, liveChild.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("parent-1-new"), liveChild.GetOwnerReferences()[0].UID)
}

func TestRetryPendingPatches(t *testing.T) {
	scheme := runtime.NewScheme()
	machineObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Machine",
			"metadata": map[string]any{
				"name":      "worker-retry",
				"namespace": "default",
			},
			"spec": map[string]any{},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineObj).Build()

	pending := []velerov1api.PendingPatchRef{
		{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Machine",
			Namespace: "default",
			Name:      "worker-retry",
			PatchType: "ownerRef",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "cluster-1",
					UID:        "new-cluster-uid",
				},
			},
		},
		{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Machine",
			Namespace:     "default",
			Name:          "worker-retry",
			PatchType:     "specRef",
			SpecPatchJSON: `{"metadata":{"resourceVersion":"stale-1"},"spec":{"infrastructureRef":{"name":"vsphere-vm-1"}}}`,
		},
	}

	warnings, remaining := RetryPendingPatches(context.Background(), logrus.StandardLogger(), fakeClient, nil, pending)
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, remaining)

	liveMachine := &unstructured.Unstructured{}
	liveMachine.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "worker-retry"}, liveMachine)
	require.NoError(t, err)
	require.Len(t, liveMachine.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("new-cluster-uid"), liveMachine.GetOwnerReferences()[0].UID)
	specInfra, ok := liveMachine.Object["spec"].(map[string]any)["infrastructureRef"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "vsphere-vm-1", specInfra["name"])
}

type forbiddenOnBlockOwnerDeletionClient struct {
	client.Client
	forbiddenCount int
}

func (c *forbiddenOnBlockOwnerDeletionClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), `"blockOwnerDeletion":true`) {
		c.forbiddenCount++
		return apierrors.NewForbidden(schema.GroupResource{Group: "test.example.io", Resource: "childapps"}, obj.GetName(), fmt.Errorf("cannot set blockOwnerDeletion: user does not have delete permission on owner"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestPatchObjectOwnerRefs_RBACForbiddenFallback(t *testing.T) {
	scheme := runtime.NewScheme()
	childObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.example.io/v1",
			"kind":       "ChildApp",
			"metadata": map[string]any{
				"name":            "child-rbac",
				"namespace":       "default",
				"resourceVersion": "100",
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()
	clientWrapper := &forbiddenOnBlockOwnerDeletionClient{Client: fakeClient}

	blockOwnerDeletionTrue := true
	remapped := []metav1.OwnerReference{
		{
			APIVersion:         "test.example.io/v1",
			Kind:               "ParentApp",
			Name:               "parent-1",
			UID:                types.UID("new-parent-uid"),
			BlockOwnerDeletion: &blockOwnerDeletionTrue,
		},
	}

	req := ownerref.OwnerPatchRequest{
		Group:     "test.example.io",
		Version:   "v1",
		Kind:      "ChildApp",
		Namespace: "default",
		Name:      "child-rbac",
	}

	err := patchObjectOwnerRefs(context.Background(), clientWrapper, req, remapped, logrus.StandardLogger())
	require.NoError(t, err, "patchObjectOwnerRefs should succeed after stripping blockOwnerDeletion upon 403 Forbidden")
	assert.Equal(t, 1, clientWrapper.forbiddenCount, "forbidden error should have been encountered once and handled")

	liveChild := &unstructured.Unstructured{}
	liveChild.SetGroupVersionKind(schema.GroupVersionKind{Group: "test.example.io", Version: "v1", Kind: "ChildApp"})
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "child-rbac"}, liveChild)
	require.NoError(t, err)
	require.Len(t, liveChild.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("new-parent-uid"), liveChild.GetOwnerReferences()[0].UID)
	assert.Nil(t, liveChild.GetOwnerReferences()[0].BlockOwnerDeletion, "blockOwnerDeletion should be stripped to nil on fallback")
}

func TestApplyOwnerRefRemapping_PersistentVolumeClaim(t *testing.T) {
	scheme := runtime.NewScheme()
	pvcObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":            "data-pvc",
				"namespace":       "default",
				"resourceVersion": "50",
			},
			"spec": map[string]any{
				"accessModes": []any{"ReadWriteOnce"},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvcObj).Build()

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	// Built-in seed allowlist includes PersistentVolumeClaim
	scope := ownerref.NewScope()
	state.SetScope(scope)
	state.RegisterUIDMapping("old-operator-parent-uid", "new-operator-parent-uid")

	isController := true
	req := ownerref.OwnerPatchRequest{
		Group:     "",
		Version:   "v1",
		Kind:      "PersistentVolumeClaim",
		Resource:  "persistentvolumeclaims",
		Namespace: "default",
		Name:      "data-pvc",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "database.example.io/v1",
				Kind:       "DatabaseCluster",
				Name:       "db-cluster-1",
				UID:        "old-operator-parent-uid",
				Controller: &isController,
			},
		},
		OwnerRefSourceNS: []string{"default"},
	}
	state.EnqueueOwnerPatch(req)

	restore := &velerov1api.Restore{}
	warnings, pending := ApplyOwnerRefRemapping(context.Background(), logrus.StandardLogger(), fakeClient, restore, state)
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, pending)
	assert.Empty(t, state.GetOwnerPatchQueue())

	livePVC := &unstructured.Unstructured{}
	livePVC.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "data-pvc"}, livePVC)
	require.NoError(t, err)
	require.Len(t, livePVC.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("new-operator-parent-uid"), livePVC.GetOwnerReferences()[0].UID)
	assert.Equal(t, "DatabaseCluster", livePVC.GetOwnerReferences()[0].Kind)
	assert.Equal(t, "db-cluster-1", livePVC.GetOwnerReferences()[0].Name)
	require.NotNil(t, livePVC.GetOwnerReferences()[0].Controller)
	assert.True(t, *livePVC.GetOwnerReferences()[0].Controller)
}

func TestRegisterAndMaybeEnqueue_Durability(t *testing.T) {
	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group:     "cluster.x-k8s.io",
		Kind:      "Cluster",
		JSONPaths: []string{"spec.infrastructureRef"},
	})

	ctx := &restoreContext{
		ownerRefRemap: state,
		ownerRefScope: scope,
	}

	backupObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-1",
				"namespace": "default",
				"uid":       "backup-cluster-uid",
			},
		},
	}

	liveObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-1",
				"namespace": "default",
				"uid":       "live-cluster-uid",
			},
		},
	}

	origOwnerRefs := []metav1.OwnerReference{
		{
			APIVersion: "tanzu.vmware.com/v1",
			Kind:       "ManagementCluster",
			Name:       "mgmt-1",
			UID:        "mgmt-uid-1",
		},
	}

	ctx.registerAndMaybeEnqueue(
		backupObj,
		liveObj,
		schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "clusters"},
		origOwnerRefs,
		[]string{"default"},
	)

	// 1. UID mapping must be registered immediately
	newUID, found := state.GetNewUID("backup-cluster-uid")
	assert.True(t, found)
	assert.Equal(t, types.UID("live-cluster-uid"), newUID)

	// 2. OwnerPatchQueue must contain the enqueue request
	ownerQueue := state.GetOwnerPatchQueue()
	require.Len(t, ownerQueue, 1)
	assert.Equal(t, "cluster-1", ownerQueue[0].Name)
	assert.Equal(t, "Cluster", ownerQueue[0].Kind)
	require.Len(t, ownerQueue[0].OriginalOwnerRefs, 1)
	assert.Equal(t, types.UID("mgmt-uid-1"), ownerQueue[0].OriginalOwnerRefs[0].UID)

	// 3. SpecPatchQueue must contain the request because scope.HasSpecRefPaths is true
	specQueue := state.GetSpecPatchQueue()
	require.Len(t, specQueue, 1)
	assert.Equal(t, "cluster-1", specQueue[0].Name)
	assert.Equal(t, "Cluster", specQueue[0].Kind)
}

