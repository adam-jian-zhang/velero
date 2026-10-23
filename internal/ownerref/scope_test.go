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
	"maps"
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
		{"namespace baseline denied", "- kind: Namespace", true, true},
		{"namespace per-restore denied", "- group: ''\n  kind: Namespace", false, true},
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
			assert.False(t, s.IsInScope(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}))
		})
	}
	s := NewScope()
	require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{InScopeConfigKey: "- group: example.io"}}, true))
	assert.True(t, s.IsInScope(schema.GroupVersionKind{Group: "example.io", Version: "v9", Kind: "Anything"}))
	for _, section := range []string{"specRefPaths", "quiesceOnRestore", "unknown"} {
		require.Error(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{section: "[]"}}, false))
	}
}

func TestScopeNamespaceIsDeniedOnlyAsChild(t *testing.T) {
	namespace := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	s := NewScope()
	require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{OutOfScopeConfigKey: "- kind: Namespace"}}, false))
	assert.False(t, s.IsInScope(namespace))
	// The child boundary holds even if an internal caller bypasses validation.
	s.entries[ScopeEntry{Kind: "Namespace"}] = struct{}{}
	s.excludedEntries = nil
	assert.False(t, s.IsInScope(namespace))
	assert.True(t, IsDeniedChildGroupKind(namespace.GroupKind()))
	assert.False(t, IsDeniedGroupKind(namespace.GroupKind()), "Namespace must remain eligible as a PVC parent")
	for _, workload := range DenyListGroupKinds {
		assert.True(t, IsDeniedChildGroupKind(workload))
		assert.True(t, IsDeniedGroupKind(workload))
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

func TestScopeExclusionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, baseline, included, excluded string
		gvk                                schema.GroupVersionKind
		want                               bool
	}{
		{"kind overrides baseline group", "- group: example.io", "", "- group: example.io\n  kind: Child", schema.GroupVersionKind{Group: "example.io", Version: "v2", Kind: "Child"}, false},
		{"kind overrides baseline kind", "- group: example.io\n  kind: Child", "", "- group: example.io\n  kind: Child", schema.GroupVersionKind{Group: "example.io", Kind: "Child"}, false},
		{"kind overrides restore group", "", "- group: example.io", "- group: example.io\n  kind: Child", schema.GroupVersionKind{Group: "example.io", Kind: "Child"}, false},
		{"group overrides both exact inclusions", "- group: example.io\n  kind: Child", "- group: example.io\n  kind: Child", "- group: example.io", schema.GroupVersionKind{Group: "example.io", Version: "v9", Kind: "Child"}, false},
		{"unexcluded sibling", "- group: example.io", "", "- group: example.io\n  kind: Child", schema.GroupVersionKind{Group: "example.io", Kind: "Sibling"}, true},
		{"unexcluded group", "- group: another.io", "", "- group: example.io", schema.GroupVersionKind{Group: "another.io", Kind: "Child"}, true},
		{"PVC exclusion overrides opt in", "", "- kind: PersistentVolumeClaim", "- kind: PersistentVolumeClaim", schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, false},
		{"denied exclusion allowed", "- group: apps", "", "- group: apps\n  kind: ReplicaSet", schema.GroupVersionKind{Group: "apps", Kind: "ReplicaSet"}, false},
		{"deny list still wins", "- group: apps", "", "[]", schema.GroupVersionKind{Group: "apps", Kind: "ReplicaSet"}, false},
		{"empty list preserves baseline", "- group: example.io", "", "[]", schema.GroupVersionKind{Group: "example.io", Kind: "Child"}, true},
		{"empty value preserves baseline", "- group: example.io", "", "", schema.GroupVersionKind{Group: "example.io", Kind: "Child"}, true},
		{"exclusion does not include", "", "", "- group: other.io", schema.GroupVersionKind{Group: "example.io", Kind: "Child"}, false},
		{"duplicate exclusions", "- group: example.io", "", "- group: example.io\n- group: example.io", schema.GroupVersionKind{Group: "example.io", Kind: "Child"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScope()
			require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{InScopeConfigKey: tc.baseline}}, true))
			data := map[string]string{OutOfScopeConfigKey: tc.excluded}
			if tc.included != "" {
				data[InScopeConfigKey] = tc.included
			}
			require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: data}, false))
			assert.Equal(t, tc.want, s.IsInScope(tc.gvk))
			// Reapplying a configuration is harmless and cannot restore excluded kinds.
			require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: data}, false))
			assert.Equal(t, tc.want, s.IsInScope(tc.gvk))
		})
	}
}

func TestScopeExclusionValidationIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		data      map[string]string
		baseline  bool
		wantError string
	}{
		{"baseline exclusions", map[string]string{OutOfScopeConfigKey: "- group: example.io"}, true, "server baseline"},
		{"baseline empty list", map[string]string{OutOfScopeConfigKey: "[]"}, true, "server baseline"},
		{"baseline empty value", map[string]string{OutOfScopeConfigKey: ""}, true, "server baseline"},
		{"malformed exclusions", map[string]string{InScopeConfigKey: "- group: added.io", OutOfScopeConfigKey: "["}, false, "outOfScope"},
		{"unknown field", map[string]string{OutOfScopeConfigKey: "- group: example.io\n  version: v1"}, false, "outOfScope"},
		{"empty entry after valid entry", map[string]string{InScopeConfigKey: "- group: added.io", OutOfScopeConfigKey: "- group: example.io\n- {}"}, false, "outOfScope"},
		{"core group needs kind", map[string]string{OutOfScopeConfigKey: "- group: ''"}, false, "outOfScope"},
		{"mapping instead of list", map[string]string{OutOfScopeConfigKey: "group: example.io"}, false, "outOfScope"},
		{"invalid inclusion with valid exclusions", map[string]string{InScopeConfigKey: "[", OutOfScopeConfigKey: "- group: example.io"}, false, "inScope"},
		{"exclusion cannot hide denied inclusion", map[string]string{InScopeConfigKey: "- kind: Pod", OutOfScopeConfigKey: "- kind: Pod"}, false, "deny list"},
		{"unknown section", map[string]string{OutOfScopeConfigKey: "- group: example.io", "deScope": "[]"}, false, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScope()
			require.NoError(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{
				InScopeConfigKey: "- group: example.io", OutOfScopeConfigKey: "- group: example.io\n  kind: ExistingExclusion",
			}}, false))
			beforeIncluded, beforeExcluded := maps.Clone(s.entries), maps.Clone(s.excludedEntries)
			require.ErrorContains(t, s.MergeConfigMap(&corev1api.ConfigMap{Data: tc.data}, tc.baseline), tc.wantError)
			assert.Equal(t, beforeIncluded, s.entries)
			assert.Equal(t, beforeExcluded, s.excludedEntries)
		})
	}
}

func TestLoadScopeExclusionOnlyConfigMap(t *testing.T) {
	for _, baselineName := range []string{"", "custom-baseline"} {
		t.Run(baselineName, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1api.AddToScheme(scheme))
			name := baselineName
			if name == "" {
				name = DefaultConfigMap
			}
			baseline := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "velero"}, Data: map[string]string{InScopeConfigKey: "- group: example.io"}}
			delta := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "delta", Namespace: "velero"}, Data: map[string]string{OutOfScopeConfigKey: "- group: example.io\n  kind: Child"}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(baseline, delta).Build()
			ref := &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: delta.Name}
			s, warnings, err := LoadScope(t.Context(), c, "velero", baselineName, ref)
			require.NoError(t, err)
			assert.True(t, warnings.IsEmpty())
			assert.False(t, s.IsInScope(schema.GroupVersionKind{Group: "example.io", Kind: "Child"}))
			assert.True(t, s.IsInScope(schema.GroupVersionKind{Group: "example.io", Kind: "Sibling"}))
			// Per-restore exclusions cannot hide invalid baseline inclusions.
			baseline.Data[InScopeConfigKey] = "- kind: PersistentVolumeClaim"
			require.NoError(t, c.Update(t.Context(), baseline))
			delta.Data[OutOfScopeConfigKey] = "- kind: PersistentVolumeClaim"
			require.NoError(t, c.Update(t.Context(), delta))
			_, _, err = LoadScope(t.Context(), c, "velero", baselineName, ref)
			require.ErrorContains(t, err, "baseline")
		})
	}
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
