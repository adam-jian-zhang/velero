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

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/label"
)

func newOwnerRefTestUnstructured(gvk schema.GroupVersionKind, namespace, name string, annotations, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	if labels != nil {
		u.SetLabels(labels)
	}
	return u
}

func TestApplyOwnerRefRelinking(t *testing.T) {
	ctx := context.Background()
	log := logrus.New()
	scheme := runtime.NewScheme()

	childGVK := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerCluster",
	}

	t.Run("successfully patches child with live parent UID", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-parent-uuid-111")
		newParentUID := types.UID("new-parent-uuid-222")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "target-ns")

		ctrl := true
		block := true
		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion:         "cluster.x-k8s.io/v1beta1",
					Kind:               "Cluster",
					Name:               "my-cluster",
					UID:                oldParentUID,
					Controller:         &ctrl,
					BlockOwnerDeletion: &block,
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		// Verify child has patched ownerReferences
		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)

		ownerRefs := updatedChild.GetOwnerReferences()
		require.Len(t, ownerRefs, 1)
		assert.Equal(t, newParentUID, ownerRefs[0].UID)
		assert.Equal(t, "my-cluster", ownerRefs[0].Name)
		assert.Equal(t, "Cluster", ownerRefs[0].Kind)
		require.NotNil(t, ownerRefs[0].Controller)
		assert.True(t, *ownerRefs[0].Controller)
		require.NotNil(t, ownerRefs[0].BlockOwnerDeletion)
		assert.True(t, *ownerRefs[0].BlockOwnerDeletion)
	})

	t.Run("skips ownerRef if namespace mapping splits owner and child", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns-child", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-parent-uuid-111")
		newParentUID := types.UID("new-parent-uuid-222")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "target-ns-parent")

		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns-child",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "my-cluster",
					UID:        oldParentUID,
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		// Child should not have ownerReferences added because of split
		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns-child", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)
		assert.Empty(t, updatedChild.GetOwnerReferences())
	})

	t.Run("skips unmapped ownerReference when parent UID was not tracked", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		// No mapping registered in UIDMap

		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "my-cluster",
					UID:        "unknown-parent-uid",
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)
		assert.Empty(t, updatedChild.GetOwnerReferences())
	})

	t.Run("skips ownerReference if parent is on deny list", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-pod-uid")
		newParentUID := types.UID("new-pod-uid")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "target-ns")

		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "my-parent-pod",
					UID:        oldParentUID,
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)
		assert.Empty(t, updatedChild.GetOwnerReferences())
	})
	t.Run("skips ownerReference with malformed APIVersion", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-parent-uid")
		newParentUID := types.UID("new-parent-uid")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "target-ns")

		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion: "///invalid-gv",
					Kind:       "Cluster",
					Name:       "my-parent",
					UID:        oldParentUID,
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)
		assert.Empty(t, updatedChild.GetOwnerReferences())
	})

	t.Run("skips duplicate parent ownerReference mapping to the same new UID", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldUID1 := types.UID("old-parent-1")
		oldUID2 := types.UID("old-parent-2")
		newUID := types.UID("new-parent-common")
		state.RegisterUIDMapping(oldUID1, newUID, "target-ns")
		state.RegisterUIDMapping(oldUID2, newUID, "target-ns")

		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "my-cluster",
					UID:        oldUID1,
				},
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "my-cluster",
					UID:        oldUID2,
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)
		require.Len(t, updatedChild.GetOwnerReferences(), 1)
		assert.Equal(t, newUID, updatedChild.GetOwnerReferences()[0].UID)
	})

	t.Run("patches ownerRef when the parent is cluster-scoped", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-cluster-scoped")
		newParentUID := types.UID("new-cluster-scoped")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "")

		state.EnqueueOwnerPatch(
			childGVK,
			"dockerclusters",
			"target-ns",
			"my-cluster",
			[]metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "my-cluster",
					UID:        oldParentUID,
				},
			},
		)

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild)
		require.NoError(t, err)
		require.Len(t, updatedChild.GetOwnerReferences(), 1)
		assert.Equal(t, newParentUID, updatedChild.GetOwnerReferences()[0].UID)
	})

	t.Run("keeps a second non-controlling owner", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldCtrl := types.UID("old-ctrl")
		newCtrl := types.UID("new-ctrl")
		oldOther := types.UID("old-other")
		newOther := types.UID("new-other")
		state.RegisterUIDMapping(oldCtrl, newCtrl, "target-ns")
		state.RegisterUIDMapping(oldOther, newOther, "target-ns")
		ctrl := true

		state.EnqueueOwnerPatch(childGVK, "dockerclusters", "target-ns", "my-cluster", []metav1.OwnerReference{
			{APIVersion: "cluster.x-k8s.io/v1beta1", Kind: "Cluster", Name: "my-cluster", UID: oldCtrl, Controller: &ctrl},
			{APIVersion: "cluster.x-k8s.io/v1beta1", Kind: "Machine", Name: "cp-0", UID: oldOther},
		})

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild))
		refs := updatedChild.GetOwnerReferences()
		require.Len(t, refs, 2)
		assert.Equal(t, newCtrl, refs[0].UID)
		require.NotNil(t, refs[0].Controller)
		assert.True(t, *refs[0].Controller)
		assert.Equal(t, newOther, refs[1].UID)
		assert.Nil(t, refs[1].Controller)
	})

	t.Run("demotes restored controller when live object already has one", func(t *testing.T) {
		liveCtrl := true
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		childObj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "example.com/v1",
			Kind:       "Operator",
			Name:       "live-owner",
			UID:        "live-uid",
			Controller: &liveCtrl,
		}})
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-parent")
		newParentUID := types.UID("new-parent")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "target-ns")
		restoredCtrl := true
		state.EnqueueOwnerPatch(childGVK, "dockerclusters", "target-ns", "my-cluster", []metav1.OwnerReference{{
			APIVersion: "cluster.x-k8s.io/v1beta1",
			Kind:       "Cluster",
			Name:       "my-cluster",
			UID:        oldParentUID,
			Controller: &restoredCtrl,
		}})

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild))
		refs := updatedChild.GetOwnerReferences()
		require.Len(t, refs, 2)
		assert.Equal(t, "Operator", refs[0].Kind)
		require.NotNil(t, refs[0].Controller)
		assert.True(t, *refs[0].Controller)
		assert.Equal(t, newParentUID, refs[1].UID)
		assert.Nil(t, refs[1].Controller)
	})

	t.Run("retries once without blockOwnerDeletion after HTTP 403", func(t *testing.T) {
		childObj := newOwnerRefTestUnstructured(childGVK, "target-ns", "my-cluster", nil, nil)
		base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childObj).Build()
		fakeClient := &forbiddenOnceClient{Client: base}

		state := NewOwnerRefRemapState(true, &ScopeConfig{})
		oldParentUID := types.UID("old-parent")
		newParentUID := types.UID("new-parent")
		state.RegisterUIDMapping(oldParentUID, newParentUID, "target-ns")
		block := true
		state.EnqueueOwnerPatch(childGVK, "dockerclusters", "target-ns", "my-cluster", []metav1.OwnerReference{{
			APIVersion:         "cluster.x-k8s.io/v1beta1",
			Kind:               "Cluster",
			Name:               "my-cluster",
			UID:                oldParentUID,
			BlockOwnerDeletion: &block,
		}})

		warnings := ApplyOwnerRefRelinking(ctx, log, fakeClient, state)
		assert.Empty(t, warnings.Namespaces)
		assert.Equal(t, 2, fakeClient.patches)

		updatedChild := &unstructured.Unstructured{}
		updatedChild.SetGroupVersionKind(childGVK)
		require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "target-ns", Name: "my-cluster"}, updatedChild))
		refs := updatedChild.GetOwnerReferences()
		require.Len(t, refs, 1)
		assert.Equal(t, newParentUID, refs[0].UID)
		assert.Nil(t, refs[0].BlockOwnerDeletion)
	})
}

type forbiddenOnceClient struct {
	client.Client
	patches int
}

func (c *forbiddenOnceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	if c.patches == 1 {
		return apierrors.NewForbidden(schema.GroupResource{Group: "infrastructure.cluster.x-k8s.io", Resource: "dockerclusters"}, obj.GetName(), fmt.Errorf("missing delete permission"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type emptyVersionListClient struct {
	client.Client
}

func (c *emptyVersionListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if uList, ok := list.(*unstructured.UnstructuredList); ok {
		for i := range uList.Items {
			// Simulate live API server omitting apiVersion in list items
			uList.Items[i].SetAPIVersion("")
		}
	}
	return nil
}

func TestUnquiesceObjects(t *testing.T) {
	ctx := context.Background()
	log := logrus.New()
	scheme := runtime.NewScheme()

	clusterGVK := schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Cluster",
	}

	quiescedCluster1 := newOwnerRefTestUnstructured(
		clusterGVK,
		"ns-1",
		"cluster-1",
		map[string]string{
			"cluster.x-k8s.io/paused":         "",
			velerov1api.QuiescedKeyAnnotation: "cluster.x-k8s.io/paused",
		},
		map[string]string{velerov1api.QuiescedByRestoreLabel: "restore-test"},
	)
	quiescedClusterOtherRestore := newOwnerRefTestUnstructured(
		clusterGVK,
		"ns-2",
		"cluster-2",
		map[string]string{"cluster.x-k8s.io/paused": ""},
		map[string]string{velerov1api.QuiescedByRestoreLabel: "restore-other"},
	)

	t.Run("returns early without modifying anything when rules are empty", func(t *testing.T) {
		c1 := quiescedCluster1.DeepCopy()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c1).Build()

		warnings := UnquiesceObjects(ctx, log, fakeClient, "restore-test")
		assert.Empty(t, warnings.Namespaces)

		updated1 := &unstructured.Unstructured{}
		updated1.SetGroupVersionKind(clusterGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: "cluster-1"}, updated1)
		require.NoError(t, err)
		assert.Contains(t, updated1.GetAnnotations(), "cluster.x-k8s.io/paused")
	})

	t.Run("unquiesces objects matching rule and restore label", func(t *testing.T) {
		c1 := quiescedCluster1.DeepCopy()
		c2 := quiescedClusterOtherRestore.DeepCopy()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c1, c2).Build()

		rule := QuiesceRule{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		}
		warnings := UnquiesceObjects(ctx, log, fakeClient, "restore-test", rule)
		assert.Empty(t, warnings.Namespaces)

		// Verify cluster-1 has annotation and label removed
		updated1 := &unstructured.Unstructured{}
		updated1.SetGroupVersionKind(clusterGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: "cluster-1"}, updated1)
		require.NoError(t, err)
		assert.NotContains(t, updated1.GetAnnotations(), "cluster.x-k8s.io/paused")
		assert.NotContains(t, updated1.GetAnnotations(), velerov1api.QuiescedKeyAnnotation)
		assert.NotContains(t, updated1.GetLabels(), velerov1api.QuiescedByRestoreLabel)

		// Verify cluster-2 from other restore was NOT modified
		updated2 := &unstructured.Unstructured{}
		updated2.SetGroupVersionKind(clusterGVK)
		err = fakeClient.Get(ctx, client.ObjectKey{Namespace: "ns-2", Name: "cluster-2"}, updated2)
		require.NoError(t, err)
		assert.Contains(t, updated2.GetAnnotations(), "cluster.x-k8s.io/paused")
		assert.Equal(t, "restore-other", updated2.GetLabels()[velerov1api.QuiescedByRestoreLabel])
	})

	t.Run("unquiesces objects when list items have empty version in unstructured", func(t *testing.T) {
		c1 := quiescedCluster1.DeepCopy()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c1).Build()
		wrappedClient := &emptyVersionListClient{Client: fakeClient}

		rule := QuiesceRule{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		}
		warnings := UnquiesceObjects(ctx, log, wrappedClient, "restore-test", rule)
		assert.Empty(t, warnings.Namespaces)

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(clusterGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: "cluster-1"}, updated)
		require.NoError(t, err)
		assert.NotContains(t, updated.GetAnnotations(), "cluster.x-k8s.io/paused")
		assert.NotContains(t, updated.GetLabels(), velerov1api.QuiescedByRestoreLabel)
	})

	t.Run("skips non-existent CRD gracefully without warning", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		nonExistentRule := QuiesceRule{
			Group:         "nonexistent.example.com",
			Version:       "v1",
			Kind:          "NonExistentResource",
			AnnotationKey: "example.com/paused",
		}
		warnings := UnquiesceObjects(ctx, log, fakeClient, "restore-test", nonExistentRule)
		assert.Empty(t, warnings.Namespaces)
	})

	t.Run("unquiesces using a valid label value for a long restore name", func(t *testing.T) {
		longName := strings.Repeat("a", 70)
		labelValue := label.GetValidName(longName)
		require.NotEqual(t, longName, labelValue)

		cluster := newOwnerRefTestUnstructured(
			clusterGVK,
			"ns-1",
			"cluster-long",
			map[string]string{"cluster.x-k8s.io/paused": ""},
			map[string]string{velerov1api.QuiescedByRestoreLabel: labelValue},
		)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

		rule := QuiesceRule{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		}
		warnings := UnquiesceObjects(ctx, log, fakeClient, longName, rule)
		assert.Empty(t, warnings.Cluster)
		assert.Empty(t, warnings.Namespaces)

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(clusterGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: "cluster-long"}, updated)
		require.NoError(t, err)
		assert.NotContains(t, updated.GetAnnotations(), "cluster.x-k8s.io/paused")
		assert.NotContains(t, updated.GetLabels(), velerov1api.QuiescedByRestoreLabel)
	})

	t.Run("skips rule when version cannot be resolved", func(t *testing.T) {
		cluster := quiescedCluster1.DeepCopy()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

		rule := QuiesceRule{
			Group:         "cluster.x-k8s.io",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		}
		warnings := UnquiesceObjects(ctx, log, fakeClient, "restore-test", rule)
		require.NotEmpty(t, warnings.Cluster)

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(clusterGVK)
		err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: "cluster-1"}, updated)
		require.NoError(t, err)
		assert.Contains(t, updated.GetAnnotations(), "cluster.x-k8s.io/paused")
		assert.Equal(t, "restore-test", updated.GetLabels()[velerov1api.QuiescedByRestoreLabel])
	})
}
