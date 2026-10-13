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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/velero-io/velero/internal/ownerref"
	discoverymocks "github.com/velero-io/velero/pkg/discovery/mocks"
)

func TestOwnerRefEmitsDestinationServedVersion(t *testing.T) {
	for _, tc := range []struct {
		name, liveVersion, want string
		serveV1                 bool
	}{
		{name: "removed backup version", want: "v2"},
		{name: "removed live version", liveVersion: "v1", want: "v2"},
		{name: "served backup version despite preferred v2", serveV1: true, want: "v1"},
		{name: "served live version takes precedence", liveVersion: "v2", serveV1: true, want: "v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child, parent := newOwnerRefChild(), newOwnerRefParent()
			parent.SetAPIVersion("example.io/v2")
			if tc.liveVersion != "" {
				live := newOwnerRef("stale-parent", parent.GetName())
				live.APIVersion = "example.io/" + tc.liveVersion
				child.SetOwnerReferences([]metav1.OwnerReference{live})
			}
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.io", Version: "v2"}, {Group: "example.io", Version: "v1"}})
			mapper.Add(parent.GroupVersionKind(), meta.RESTScopeNamespace)
			if tc.serveV1 {
				mapper.Add(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Parent"}, meta.RESTScopeNamespace)
			}
			base := fake.NewClientBuilder().WithRESTMapper(mapper).WithObjects(child).Build()
			c := &ownerRefInterceptClient{Client: base}
			c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				require.Equal(t, "Child", obj.GetObjectKind().GroupVersionKind().Kind, "relinking must not GET parents")
				return base.Get(ctx, key, obj, opts...)
			}
			state := ownerref.NewState(ownerref.NewScope())
			discovery := &discoverymocks.Helper{}
			discovery.On("KindFor", parent.GroupVersionKind()).Return(schema.GroupVersionResource{Group: "example.io", Version: "v2", Resource: "parents"}, metav1.APIResource{Namespaced: true}, nil)
			registration := &restoreContext{ownerRefRelink: state, kbClient: c, discoveryHelper: discovery}
			backup := parent.DeepCopy()
			backup.SetUID("parent-old")
			registration.registerOwnerRefObject(backup, parent, parent.GroupVersionKind(), false)
			state.Enqueue(newOwnerRefRequest())
			_, errs := ApplyOwnerRefRelinking(t.Context(), c, newOwnerRefRestore(), state)
			require.True(t, errs.IsEmpty(), "%+v", errs)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
			require.Len(t, child.GetOwnerReferences(), 1)
			assert.Equal(t, "example.io/"+tc.want, child.GetOwnerReferences()[0].APIVersion)
			assert.Equal(t, parent.GetUID(), child.GetOwnerReferences()[0].UID)
		})
	}
}
