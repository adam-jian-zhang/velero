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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
)

func TestOwnerRefUnavailableParentsRemainPending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		missing   bool
		uid       types.UID
		deleting  bool
		otherNS   bool
		wantError string
	}{
		{name: "missing", missing: true, wantError: "not found"},
		{name: "deleting", uid: "parent-new", deleting: true, wantError: "deleting"},
		{name: "parent in another namespace", uid: "parent-new", otherNS: true, wantError: "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := newOwnerRefChild()
			before := child.DeepCopy()
			parent := newOwnerRefParent()
			parent.SetUID(tc.uid)
			if tc.deleting {
				now := metav1.Now()
				parent.SetDeletionTimestamp(&now)
				parent.SetFinalizers([]string{"example.io/hold"})
			}
			if tc.otherNS {
				parent.SetNamespace("other")
			}
			objects := []client.Object{child}
			if !tc.missing {
				objects = append(objects, parent)
			}
			base := newOwnerRefRestoreClient(t, objects...)
			c := &ownerRefInterceptClient{Client: base, patch: func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("must not patch child with an invalid parent")
				return nil
			}}
			// Replay the persisted payload, as a finalizer after a restart would.
			r := newOwnerRefRestore()
			storageErrors := SavePendingPatchesHybrid(t.Context(), base, r, []velerov1.PendingPatchRef{newOwnerRefPending(0)})
			require.True(t, storageErrors.IsEmpty(), "%+v", storageErrors)
			pending, err := LoadPendingPatchesHybrid(t.Context(), base, r)
			require.NoError(t, err)
			warnings, errs, remaining := RetryPendingPatches(t.Context(), c, r, pending)
			assert.True(t, errs.IsEmpty(), "%+v", errs)
			assert.Contains(t, fmt.Sprint(warnings), tc.wantError)
			assert.Equal(t, pending, remaining)
			assert.Zero(t, r.Status.OwnerRefsRelinked)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
			assert.Equal(t, before.GetOwnerReferences(), child.GetOwnerReferences())
		})
	}
}

func TestOwnerRefValidatesNamespacedAndClusterParents(t *testing.T) {
	for _, clusterChild := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster child %t", clusterChild), func(t *testing.T) {
			child := newOwnerRefChild()
			req := newOwnerRefPending(0)
			if clusterChild {
				child.SetNamespace("")
				req.Namespace = ""
			}
			parent := &corev1api.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "cluster-parent"}}
			req.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: parent.Name, UID: parent.UID}}
			objects := []client.Object{child, parent}
			if !clusterChild {
				objects = append(objects, newOwnerRefParent())
				req.OwnerReferences = append(req.OwnerReferences, newOwnerRefPending(0).OwnerReferences...)
			}
			c := newOwnerRefRestoreClient(t, objects...)
			r := newOwnerRefRestore()
			_, errs, remaining := RetryPendingPatches(t.Context(), c, r, []velerov1.PendingPatchRef{req})
			require.True(t, errs.IsEmpty(), "%+v", errs)
			assert.Empty(t, remaining)
			assert.Equal(t, 1, r.Status.OwnerRefsRelinked)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(child), child))
			assert.Equal(t, req.OwnerReferences, child.GetOwnerReferences())
		})
	}
}

func TestOwnerRefValidatesParentThroughServedVersion(t *testing.T) {
	child, parent := newOwnerRefChild(), newOwnerRefParent()
	parent.SetAPIVersion("example.io/v2")
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.io", Version: "v2"}})
	mapper.Add(parent.GroupVersionKind(), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithRESTMapper(mapper).WithObjects(child, parent).Build()
	r := newOwnerRefRestore()
	_, errs, remaining := RetryPendingPatches(t.Context(), c, r, []velerov1.PendingPatchRef{newOwnerRefPending(0)})
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Empty(t, remaining)
	assert.Equal(t, 1, r.Status.OwnerRefsRelinked)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(child), child))
	require.Len(t, child.GetOwnerReferences(), 1)
	assert.Equal(t, parent.GetUID(), child.GetOwnerReferences()[0].UID)
	assert.Equal(t, "example.io/v2", child.GetOwnerReferences()[0].APIVersion)
}

func TestOwnerRefParentReadFailures(t *testing.T) {
	for _, transient := range []bool{false, true} {
		t.Run(fmt.Sprintf("transient %t", transient), func(t *testing.T) {
			base := newOwnerRefRestoreClient(t, newOwnerRefChild(), newOwnerRefParent())
			attempts := 0
			c := &ownerRefInterceptClient{Client: base}
			c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Parent" {
					attempts++
					if transient {
						return apierrors.NewTooManyRequests("busy", 0)
					}
					return apierrors.NewForbidden(schema.GroupResource{Group: "example.io", Resource: "parents"}, key.Name, fmt.Errorf("read denied"))
				}
				return base.Get(ctx, key, obj, opts...)
			}
			c.patch = func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("parent read failure must prevent patching")
				return nil
			}
			req := newOwnerRefPending(0)
			block := true
			req.OwnerReferences[0].BlockOwnerDeletion = &block
			r := newOwnerRefRestore()
			warnings, errs, remaining := RetryPendingPatches(t.Context(), c, r, []velerov1.PendingPatchRef{req})
			assert.Zero(t, r.Status.OwnerRefsRelinked)
			assert.NotContains(t, fmt.Sprint(warnings), "retrying without incoming blockOwnerDeletion")
			if transient {
				require.True(t, errs.IsEmpty(), "%+v", errs)
				require.Equal(t, []velerov1.PendingPatchRef{req}, remaining)
				assert.Equal(t, 1, attempts, "lookup failures are cached for this execution")
				_, errs, remaining = RetryPendingPatches(t.Context(), base, r, remaining)
				assert.True(t, errs.IsEmpty(), "%+v", errs)
				assert.Empty(t, remaining)
				assert.Equal(t, 1, r.Status.OwnerRefsRelinked)
			} else {
				assert.Contains(t, fmt.Sprint(errs), "read denied")
				assert.Empty(t, remaining)
				assert.Equal(t, 1, attempts)
			}
		})
	}
}

func TestOwnerRefPass1FrozenPass2RefreshesOnlyPending(t *testing.T) {
	children := []*unstructured.Unstructured{newOwnerRefChild(), newOwnerRefChild()}
	children[1].SetName("child-1")
	parent := newOwnerRefParent()
	base := newOwnerRefRestoreClient(t, children[0], children[1], parent)
	state := ownerref.NewState(ownerref.NewScope())
	state.Register("backup-parent", ownerref.UIDMappingRecord{LiveUID: parent.GetUID(), GroupKind: parent.GroupVersionKind().GroupKind(), APIVersion: parent.GetAPIVersion(), Name: parent.GetName(), Namespace: "work", Namespaced: true, ScopeKnown: true})
	for _, child := range children {
		state.Enqueue(ownerref.OwnerPatchRequest{Group: "example.io", Version: "v1", Kind: "Child", Namespace: "work", Name: child.GetName(), RestoredUID: child.GetUID(), Namespaced: true, ScopeKnown: true, OwnerReferences: []metav1.OwnerReference{newOwnerRef("backup-parent", "parent"), newOwnerRef("excluded", "excluded")}})
	}
	c := &ownerRefInterceptClient{Client: base}
	c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		require.Equal(t, "Child", obj.GetObjectKind().GroupVersionKind().Kind, "Pass 1 must not GET parents")
		return base.Get(ctx, key, obj, opts...)
	}
	c.patch = func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if obj.GetName() == "child-1" {
			return apierrors.NewTooManyRequests("busy", 0)
		}
		return base.Patch(ctx, obj, patch, opts...)
	}
	r := newOwnerRefRestore()
	_, errs, pending := ApplyOwnerRefRelinking(t.Context(), c, r, state)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	require.Len(t, pending, 1)
	require.Len(t, pending[0].OwnerReferences, 1, "excluded parent must remain omitted")
	assert.Equal(t, 1, r.Status.OwnerRefsRelinked)
	require.NoError(t, base.Delete(t.Context(), parent))
	replacement := newOwnerRefParent()
	replacement.SetUID("replacement")
	require.NoError(t, base.Create(t.Context(), replacement))
	parentGets := 0
	c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Parent" {
			parentGets++
			assert.Equal(t, "parent", key.Name)
		} else {
			assert.Equal(t, "child-1", key.Name, "Pass 1 success must not be revisited")
		}
		return base.Get(ctx, key, obj, opts...)
	}
	c.patch = nil
	warnings, errs, remaining := RetryPendingPatches(t.Context(), c, r, pending)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Contains(t, fmt.Sprint(warnings), "previous UID parent-new, current UID replacement")
	assert.Empty(t, remaining)
	assert.Equal(t, 1, parentGets)
	assert.Equal(t, 2, r.Status.OwnerRefsRelinked)
	assert.Equal(t, types.UID("parent-new"), pending[0].OwnerReferences[0].UID, "durable input must remain unchanged until commit")
	for i, child := range children {
		require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
		require.Len(t, child.GetOwnerReferences(), 1)
		assert.Equal(t, []types.UID{"parent-new", "replacement"}[i], child.GetOwnerReferences()[0].UID)
	}
}

func TestOwnerRefPass2SharesParentSnapshotAcrossChildrenAndRetries(t *testing.T) {
	for _, forbidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("forbidden %t", forbidden), func(t *testing.T) {
			parent := newOwnerRefParent()
			parent.SetUID("replacement")
			objects := []client.Object{parent}
			pending := make([]velerov1.PendingPatchRef, 100)
			for i := range pending {
				pending[i] = newOwnerRefPending(i)
				block := true
				pending[i].OwnerReferences[0].BlockOwnerDeletion = &block
				child := newOwnerRefChild()
				child.SetName(pending[i].Name)
				objects = append(objects, child)
			}
			base := newOwnerRefRestoreClient(t, objects...)
			c := &ownerRefInterceptClient{Client: base}
			gets, patches := 0, 0
			c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Parent" {
					gets++
					assert.Zero(t, patches, "discovery completes before child patching")
				}
				return base.Get(ctx, key, obj, opts...)
			}
			c.patch = func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches == 1 {
					// A subsequent incarnation is outside this execution's snapshot.
					require.NoError(t, base.Delete(ctx, parent))
					next := newOwnerRefParent()
					next.SetUID("next-replacement")
					require.NoError(t, base.Create(ctx, next))
					if forbidden {
						return apierrors.NewForbidden(schema.GroupResource{}, obj.GetName(), fmt.Errorf("blockOwnerDeletion denied"))
					}
					return apierrors.NewConflict(schema.GroupResource{}, obj.GetName(), fmt.Errorf("changed"))
				}
				return base.Patch(ctx, obj, patch, opts...)
			}
			r := newOwnerRefRestore()
			_, errs, remaining := RetryPendingPatches(t.Context(), c, r, pending)
			require.True(t, errs.IsEmpty(), "%+v", errs)
			assert.Empty(t, remaining)
			assert.Equal(t, 1, gets)
			assert.Equal(t, 101, patches)
			assert.Equal(t, 100, r.Status.OwnerRefsRelinked)
			for _, obj := range objects[1:] {
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(obj), obj))
				assert.Equal(t, types.UID("replacement"), obj.GetOwnerReferences()[0].UID)
			}
		})
	}
}

func TestOwnerRefPass2PersistsRefreshedUIDAndRediscoversOnReplay(t *testing.T) {
	parent := newOwnerRefParent()
	parent.SetUID("replacement")
	base := newOwnerRefRestoreClient(t, newOwnerRefChild(), parent)
	c := &ownerRefInterceptClient{Client: base, patch: func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
		return apierrors.NewTooManyRequests("busy", 0)
	}}
	r := newOwnerRefRestore()
	_, errs, pending := RetryPendingPatches(t.Context(), c, r, []velerov1.PendingPatchRef{newOwnerRefPending(0)})
	require.True(t, errs.IsEmpty(), "%+v", errs)
	require.Len(t, pending, 1)
	assert.Equal(t, types.UID("replacement"), pending[0].OwnerReferences[0].UID)
	storageErrors := SavePendingPatchesHybrid(t.Context(), base, r, pending)
	require.True(t, storageErrors.IsEmpty(), "%+v", storageErrors)
	loaded, err := LoadPendingPatchesHybrid(t.Context(), base, r)
	require.NoError(t, err)
	require.Equal(t, pending, loaded)
	require.NoError(t, base.Delete(t.Context(), parent))
	next := newOwnerRefParent()
	next.SetUID("next-replacement")
	require.NoError(t, base.Create(t.Context(), next))
	_, errs, remaining := RetryPendingPatches(t.Context(), base, r, loaded)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Empty(t, remaining)
	child := newOwnerRefChild()
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
	assert.Equal(t, types.UID("next-replacement"), child.GetOwnerReferences()[0].UID)
}

func TestOwnerRefPass2CacheUsesParentScopeAndNamespace(t *testing.T) {
	root := &corev1api.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "root", UID: "root-new"}}
	objects := []client.Object{root}
	var pending []velerov1.PendingPatchRef
	for _, ns := range []string{"work", "other"} {
		child, parent := newOwnerRefChild(), newOwnerRefParent()
		child.SetNamespace(ns)
		parent.SetNamespace(ns)
		parent.SetUID(types.UID(ns + "-parent"))
		objects = append(objects, child, parent)
		req := newOwnerRefPending(0)
		req.Namespace = ns
		req.OwnerReferences = append(req.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "Namespace", Name: root.Name, UID: "root-old"})
		pending = append(pending, req)
	}
	base := newOwnerRefRestoreClient(t, objects...)
	gets := map[client.ObjectKey]int{}
	c := &ownerRefInterceptClient{Client: base, get: func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind != "Child" {
			gets[key]++
		}
		return base.Get(ctx, key, obj, opts...)
	}}
	_, errs, remaining := RetryPendingPatches(t.Context(), c, newOwnerRefRestore(), pending)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Empty(t, remaining)
	assert.Equal(t, map[client.ObjectKey]int{{Name: "root"}: 1, {Namespace: "work", Name: "parent"}: 1, {Namespace: "other", Name: "parent"}: 1}, gets)
	for _, ns := range []string{"work", "other"} {
		child := newOwnerRefChild()
		require.NoError(t, base.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: child.GetName()}, child))
		assert.Equal(t, types.UID(ns+"-parent"), child.GetOwnerReferences()[0].UID)
		assert.Equal(t, root.UID, child.GetOwnerReferences()[1].UID)
	}
}

func TestOwnerRefPass2CachesUnavailableParentForAllChildren(t *testing.T) {
	base := newOwnerRefRestoreClient(t)
	gets := 0
	c := &ownerRefInterceptClient{Client: base, get: func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		require.Equal(t, "Parent", obj.GetObjectKind().GroupVersionKind().Kind, "unavailable parents must prevent child GETs")
		return base.Get(ctx, key, obj, opts...)
	}}
	pending := []velerov1.PendingPatchRef{newOwnerRefPending(0), newOwnerRefPending(1)}
	_, errs, remaining := RetryPendingPatches(t.Context(), c, newOwnerRefRestore(), pending)
	assert.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Equal(t, pending, remaining)
	assert.Equal(t, 1, gets)
}

func TestOwnerRefPass2DeadlineRetainsRefreshedAndUnprocessedReferences(t *testing.T) {
	parent := newOwnerRefParent()
	parent.SetUID("replacement")
	base := newOwnerRefRestoreClient(t, parent)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gets := 0
	c := &ownerRefInterceptClient{Client: base, get: func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		require.Equal(t, "Parent", obj.GetObjectKind().GroupVersionKind().Kind)
		err := base.Get(ctx, key, obj, opts...)
		cancel()
		return err
	}}
	pending := []velerov1.PendingPatchRef{newOwnerRefPending(0), newOwnerRefPending(1)}
	pending[1].OwnerReferences[0].Name = "unprocessed-parent"
	_, errs, remaining := RetryPendingPatches(ctx, c, newOwnerRefRestore(), pending)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	require.Len(t, remaining, 2)
	assert.Equal(t, types.UID("replacement"), remaining[0].OwnerReferences[0].UID)
	assert.Equal(t, pending[1], remaining[1])
	assert.Equal(t, 1, gets)
}
