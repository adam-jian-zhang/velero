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

package ownerref

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestScopeValidationAndSourcePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, scope       string
		baseline, invalid bool
	}{
		{"group", "- group: apps", true, false},
		{"kind", "- group: example.io\n  kind: Parent", false, false},
		{"empty", "- {}", false, true},
		{"malformed", "[", false, true},
		{"unknown field", "- group: apps\n  version: v1", false, true},
		{"pod denied", "- kind: Pod", false, true},
		{"job denied", "- group: batch\n  kind: Job", false, true},
		{"pvc baseline denied", "- kind: PersistentVolumeClaim", true, true},
		{"pvc delta", "- kind: PersistentVolumeClaim", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScope()
			err := s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{InScopeConfigKey: tc.scope}}, tc.baseline)
			if tc.invalid {
				require.Error(t, err)
				assert.Empty(t, s.entries)
			} else {
				require.NoError(t, err)
			}
			assert.False(t, s.IsInScope(schema.GroupVersionKind{Group: "apps", Kind: "ReplicaSet"}))
		})
	}
	s := NewScope()
	require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{InScopeConfigKey: "- group: example.io"}}, true))
	assert.True(t, s.IsInScope(schema.GroupVersionKind{Group: "example.io", Version: "v9", Kind: "Anything"}))
	for _, section := range []string{"specRefPaths", "quiesceOnRestore", "unknown"} {
		require.Error(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{section: "[]"}}, false))
	}
}

func TestLoadScopeOptionalBaselineRequiredDelta(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	baseline := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DefaultConfigMap, Namespace: "velero"}, Data: map[string]string{InScopeConfigKey: "- group: example.io"}}
	delta := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "delta", Namespace: "velero"}, Data: map[string]string{InScopeConfigKey: "- kind: PersistentVolumeClaim"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(baseline, delta).Build()
	ref := &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: delta.Name}
	s, warnings, err := LoadScope(context.Background(), c, "velero", "", ref)
	require.NoError(t, err)
	assert.True(t, warnings.IsEmpty())
	assert.True(t, s.IsInScope(schema.GroupVersionKind{Group: "example.io", Kind: "Child"}))
	assert.True(t, s.IsInScope(schema.GroupVersionKind{Kind: "PersistentVolumeClaim"}))
	_, warnings, err = LoadScope(context.Background(), c, "velero", "absent", ref)
	require.NoError(t, err)
	assert.False(t, warnings.IsEmpty())
	ref.Name = "absent"
	_, _, err = LoadScope(context.Background(), c, "velero", "", ref)
	require.Error(t, err)
	ref.Name = delta.Name
	baseline.Data[InScopeConfigKey] = "- kind: PersistentVolumeClaim"
	require.NoError(t, c.Update(context.Background(), baseline))
	_, _, err = LoadScope(context.Background(), c, "velero", "", ref)
	require.ErrorContains(t, err, "baseline")
	ref.Kind = "Secret"
	_, _, err = LoadScope(context.Background(), c, "velero", "", ref)
	require.ErrorContains(t, err, "reference")
}

func TestRegistryFreezeAndConcurrentRegistration(t *testing.T) {
	s := NewState(NewScope())
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			uid := types.UID(strconv.Itoa(i))
			s.Register(uid, UIDMappingRecord{LiveUID: uid, Name: "parent", ScopeKnown: true})
			s.Enqueue(OwnerPatchRequest{Name: string(uid)})
		})
	}
	wg.Wait()
	records, queue := s.Freeze()
	require.Len(t, records, 100)
	require.Len(t, queue, 100)
	s.Register("late", UIDMappingRecord{LiveUID: "later"})
	s.Enqueue(OwnerPatchRequest{Name: "late"})
	delete(records, "0")
	records, queue = s.Freeze()
	assert.Len(t, records, 100)
	assert.Len(t, queue, 100)
}
