/*
Copyright the Velero contributors.

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

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/velero-io/velero/internal/restoreorder"
	api "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/builder"
	velerotest "github.com/velero-io/velero/pkg/test"
)

func TestResolveRestoreOrder(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, data string
		forbidden             bool
		transient             bool
		want                  string
	}{
		{name: "combined", namespace: "velero", data: "version: v1\nresourceRestoreOrder:\n- before: betas\n  after: gammas"},
		{name: "other namespace", namespace: "other", data: "version: v1\nresourceRestoreOrder: []", want: "not found"},
		{name: "forbidden", namespace: "velero", forbidden: true, want: "forbidden"},
		{name: "transient", namespace: "velero", transient: true, want: "api server restarting"},
		{name: "bad document", namespace: "velero", data: "version: v1\nresourceRestoreOrder: null", want: "require version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: tc.namespace, Name: "order"}, Data: map[string]string{restoreorder.ConfigMapKey: tc.data}}
			reads := 0
			c := velerotest.NewFakeControllerRuntimeClientBuilder(t).WithObjects(cm).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					require.Equal(t, "velero", key.Namespace)
					if tc.forbidden {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
					}
					if tc.transient {
						return apierrors.NewServiceUnavailable("api server restarting")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			r := &restoreReconciler{kbClient: c, globalCrClient: c, logger: velerotest.NewLogger()}
			restore := builder.ForRestore("velero", "restore").Result()
			restore.Spec.ResourceRestoreOrder = []api.ResourceRestoreOrder{{Before: "alphas", After: "betas"}}
			restore.Spec.ResourceRestoreOrderRef = &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: "order"}
			rules, err := r.resolveRestoreOrder(t.Context(), restore)
			require.Equal(t, 1, reads)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				var transient *transientRestoreOrderError
				// Only server-side read failures are retried; every other failure is a validation error.
				require.Equal(t, tc.transient, errors.As(err, &transient))
				return
			}
			require.NoError(t, err)
			require.Len(t, rules, 2)
			require.Empty(t, rules[0].Source)
			require.Contains(t, rules[1].Source, `ConfigMap velero/order data["restore-order"].resourceRestoreOrder[0]`)
			plan, err := restoreorder.Merge([]string{"alphas", "betas", "gammas"}, nil, nil, rules)
			require.NoError(t, err)
			require.Equal(t, []string{"alphas", "betas", "gammas"}, plan.Order)
			require.Len(t, restore.Spec.ResourceRestoreOrder, 1)
		})
	}
}

func TestResolveRestoreOrderBypassesConfigMapCache(t *testing.T) {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "order"},
		Data:       map[string]string{restoreorder.ConfigMapKey: "version: v1\nresourceRestoreOrder:\n- before: alphas\n  after: betas"},
	}
	// The ConfigMap exists on the API server but has not reached the cache.
	cached := velerotest.NewFakeControllerRuntimeClientBuilder(t).Build()
	uncached := velerotest.NewFakeControllerRuntimeClientBuilder(t).WithObjects(cm).Build()
	require.True(t, apierrors.IsNotFound(cached.Get(t.Context(), client.ObjectKeyFromObject(cm), new(corev1api.ConfigMap))))
	r := &restoreReconciler{kbClient: cached, globalCrClient: uncached, logger: velerotest.NewLogger()}
	restore := builder.ForRestore("velero", "restore").Result()
	restore.Spec.ResourceRestoreOrderRef = &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: "order"}

	rules, err := r.resolveRestoreOrder(t.Context(), restore)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, "alphas", rules[0].Before)
	require.Equal(t, "betas", rules[0].After)
}
