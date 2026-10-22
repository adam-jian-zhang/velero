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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	discoverymocks "github.com/velero-io/velero/pkg/discovery/mocks"
)

func TestOwnerRefEmitsServedVersionInBothPasses(t *testing.T) {
	for _, tc := range []struct {
		name, liveVersion, want string
		serveV1                 bool
	}{
		{name: "removed backup version", want: "v2"},
		{name: "removed live version", liveVersion: "v1", want: "v2"},
		{name: "served backup version despite preferred v2", serveV1: true, want: "v1"},
		{name: "served live version takes precedence", liveVersion: "v2", serveV1: true, want: "v2"},
	} {
		for _, pass2 := range []bool{false, true} {
			pass := "pass1"
			if pass2 {
				pass = "pass2"
			}
			t.Run(tc.name+"/"+pass, func(t *testing.T) {
				child, parent := newOwnerRefChild(), newOwnerRefParent()
				parent.SetAPIVersion("example.io/v2")
				if tc.liveVersion != "" {
					live := newOwnerRef("old-live-uid", parent.GetName())
					live.APIVersion = "example.io/" + tc.liveVersion
					child.SetOwnerReferences([]metav1.OwnerReference{live})
				}
				mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.io", Version: "v2"}, {Group: "example.io", Version: "v1"}})
				mapper.Add(parent.GroupVersionKind(), meta.RESTScopeNamespace)
				if tc.serveV1 {
					mapper.Add(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Parent"}, meta.RESTScopeNamespace)
				}
				base := fake.NewClientBuilder().WithRESTMapper(mapper).WithObjects(child).Build()
				parentGets := 0
				c := &ownerRefInterceptClient{Client: base}
				c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind().Kind == "Parent" {
						parentGets++
						require.True(t, pass2, "Pass 1 must not GET parents")
						// Model API conversion: both served versions identify the same parent.
						obj.SetUID(parent.GetUID())
						return nil
					}
					return base.Get(ctx, key, obj, opts...)
				}
				r := newOwnerRefRestore()
				if pass2 {
					_, errs, remaining := RetryPendingPatches(t.Context(), c, r, []velerov1.PendingPatchRef{newOwnerRefPending(0)})
					require.True(t, errs.IsEmpty(), "%+v", errs)
					require.Empty(t, remaining)
					assert.Equal(t, 1, parentGets)
				} else {
					state := ownerref.NewState(ownerref.NewScope())
					discovery := &discoverymocks.Helper{}
					discovery.On("KindFor", parent.GroupVersionKind()).Return(schema.GroupVersionResource{Group: "example.io", Version: "v2", Resource: "parents"}, metav1.APIResource{Namespaced: true}, nil)
					registration := &restoreContext{ownerRefRelink: state, kbClient: c, discoveryHelper: discovery}
					backup := parent.DeepCopy()
					backup.SetUID("backup-parent")
					registration.registerOwnerRefObject(backup, parent, parent.GroupVersionKind(), false)
					state.Enqueue(ownerref.OwnerPatchRequest{Group: "example.io", Version: "v1", Kind: "Child", Namespace: "work", Name: child.GetName(), RestoredUID: child.GetUID(), Namespaced: true, ScopeKnown: true, OwnerReferences: []metav1.OwnerReference{newOwnerRef("backup-parent", "parent")}})
					_, errs, remaining := ApplyOwnerRefRelinking(t.Context(), c, r, state)
					require.True(t, errs.IsEmpty(), "%+v", errs)
					require.Empty(t, remaining)
					assert.Zero(t, parentGets)
				}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
				require.Len(t, child.GetOwnerReferences(), 1)
				assert.Equal(t, "example.io/"+tc.want, child.GetOwnerReferences()[0].APIVersion)
				assert.Equal(t, parent.GetUID(), child.GetOwnerReferences()[0].UID)
			})
		}
	}
}

func TestOwnerRefVersionSurvivesPendingPersistence(t *testing.T) {
	child, parent := newOwnerRefChild(), newOwnerRefParent()
	parent.SetAPIVersion("example.io/v2")
	base := newOwnerRefRestoreClient(t, child, parent)
	base.RESTMapper().(*meta.DefaultRESTMapper).Add(parent.GroupVersionKind(), meta.RESTScopeNamespace)
	state := ownerref.NewState(ownerref.NewScope())
	state.Register("backup-parent", ownerref.UIDMappingRecord{LiveUID: parent.GetUID(), GroupKind: parent.GroupVersionKind().GroupKind(), APIVersion: parent.GetAPIVersion(), Name: parent.GetName(), Namespace: "work", Namespaced: true, ScopeKnown: true})
	state.Enqueue(ownerref.OwnerPatchRequest{Group: "example.io", Version: "v1", Kind: "Child", Namespace: "work", Name: child.GetName(), RestoredUID: child.GetUID(), Namespaced: true, ScopeKnown: true, OwnerReferences: []metav1.OwnerReference{newOwnerRef("backup-parent", "parent")}})
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	r := newOwnerRefRestore()
	_, errs, pending := ApplyOwnerRefRelinking(expired, base, r, state)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	require.Len(t, pending, 1)
	assert.Equal(t, "example.io/v2", pending[0].OwnerReferences[0].APIVersion)
	require.NoError(t, ReconcilePendingPatchesConfigMap(t.Context(), base, r, pending))
	loaded, err := LoadPendingPatches(t.Context(), base, r)
	require.NoError(t, err)
	_, errs, remaining := RetryPendingPatches(t.Context(), base, r, loaded)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	require.Empty(t, remaining)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
	assert.Equal(t, "example.io/v2", child.GetOwnerReferences()[0].APIVersion)
	assert.Equal(t, parent.GetUID(), child.GetOwnerReferences()[0].UID)
}

func TestOwnerRefPass2SharesUIDAcrossVersionsAndRetainsVersionCorrection(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.io", Version: "v2"}, {Group: "example.io", Version: "v1"}})
	for _, version := range []string{"v2", "v1"} {
		mapper.Add(schema.GroupVersionKind{Group: "example.io", Version: version, Kind: "Parent"}, meta.RESTScopeNamespace)
	}
	base := fake.NewClientBuilder().WithRESTMapper(mapper).Build()
	var pending []velerov1.PendingPatchRef
	for i, version := range []string{"removed", "v1", "v2"} {
		req := newOwnerRefPending(i)
		req.OwnerReferences[0].APIVersion = "example.io/" + version
		pending = append(pending, req)
		child := newOwnerRefChild()
		child.SetName(req.Name)
		require.NoError(t, base.Create(t.Context(), child))
	}
	gets := 0
	c := &ownerRefInterceptClient{Client: base}
	c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Parent" {
			gets++
			obj.SetUID("replacement-parent")
			return nil
		}
		return base.Get(ctx, key, obj, opts...)
	}
	c.patch = func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if obj.GetName() == pending[0].Name {
			return apierrors.NewServiceUnavailable("retry later")
		}
		return base.Patch(ctx, obj, patch, opts...)
	}
	r := newOwnerRefRestore()
	_, errs, remaining := RetryPendingPatches(t.Context(), c, r, pending)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Equal(t, 1, gets, "versions must not split the parent UID cache")
	require.Len(t, remaining, 1)
	assert.Equal(t, "example.io/v2", remaining[0].OwnerReferences[0].APIVersion)
	assert.Equal(t, "replacement-parent", string(remaining[0].OwnerReferences[0].UID))
	assert.Equal(t, "example.io/removed", pending[0].OwnerReferences[0].APIVersion, "old durable input must remain untouched")
	require.NoError(t, ReconcilePendingPatchesConfigMap(t.Context(), base, r, remaining))
	loaded, err := LoadPendingPatches(t.Context(), base, r)
	require.NoError(t, err)
	assert.Equal(t, remaining, loaded)
	for _, req := range pending[1:] {
		child := newOwnerRefChild()
		require.NoError(t, base.Get(t.Context(), client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, child))
		assert.Equal(t, req.OwnerReferences[0].APIVersion, child.GetOwnerReferences()[0].APIVersion)
		assert.Equal(t, "replacement-parent", string(child.GetOwnerReferences()[0].UID))
	}
}
