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
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

func TestScopeMatching(t *testing.T) {
	cfg := &ScopeConfig{
		InScope: []InScopeEntry{
			{Group: "cluster.x-k8s.io"},
			{Group: "infrastructure.cluster.x-k8s.io", Kind: "DockerCluster"},
			{Group: "custom.io", Version: "v1", Kind: "MyCRD"},
		},
		QuiesceOnRestore: []QuiesceRule{
			{
				Group:           "cluster.x-k8s.io",
				Kind:            "Cluster",
				AnnotationKey:   "cluster.x-k8s.io/paused",
				AnnotationValue: "",
			},
		},
	}

	// 1. Group-only wildcard match
	assert.True(t, cfg.IsInScope(schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Machine",
	}))

	// 2. Group and Kind match
	assert.True(t, cfg.IsInScope(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerCluster",
	}))

	// 3. Different Kind in same group should not match if Kind is restricted
	assert.False(t, cfg.IsInScope(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachine",
	}))

	// 4. Exact GVK match
	assert.True(t, cfg.IsInScope(schema.GroupVersionKind{
		Group:   "custom.io",
		Version: "v1",
		Kind:    "MyCRD",
	}))
	assert.False(t, cfg.IsInScope(schema.GroupVersionKind{
		Group:   "custom.io",
		Version: "v2",
		Kind:    "MyCRD",
	}))

	// 5. Out of scope resource (e.g. apps/v1 Deployment)
	assert.False(t, cfg.IsInScope(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	}))

	// 6. Deny list resources should NEVER be in scope even if wildcard group matches
	cfgWithWildcard := &ScopeConfig{
		InScope: []InScopeEntry{
			{Group: ""},
			{Group: "apps"},
			{Group: "batch"},
		},
	}
	assert.False(t, cfgWithWildcard.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}))
	assert.False(t, cfgWithWildcard.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ReplicationController"}))
	assert.False(t, cfgWithWildcard.IsInScope(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}))
	assert.False(t, cfgWithWildcard.IsInScope(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}))
	// Non-denied in same group should be in scope
	assert.True(t, cfgWithWildcard.IsInScope(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}))
	assert.True(t, cfgWithWildcard.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}))

	// 7. Core group match: empty group matches core resources
	cfgCore := &ScopeConfig{
		InScope: []InScopeEntry{
			{Group: "", Kind: "PersistentVolumeClaim"},
		},
		QuiesceOnRestore: []QuiesceRule{
			{
				Group:           "",
				Kind:            "ConfigMap",
				AnnotationKey:   "quiesce.test/paused",
				AnnotationValue: "true",
			},
		},
	}
	assert.True(t, cfgCore.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"}))
	pvcQuiesce := cfgCore.ShouldQuiesce(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NotNil(t, pvcQuiesce)
	assert.Equal(t, "quiesce.test/paused", pvcQuiesce.AnnotationKey)

	// 8. Quiesce rule matching
	quiesceRule := cfg.ShouldQuiesce(schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Cluster",
	})
	require.NotNil(t, quiesceRule)
	assert.Equal(t, "cluster.x-k8s.io/paused", quiesceRule.AnnotationKey)

	// Should not quiesce other kinds
	assert.Nil(t, cfg.ShouldQuiesce(schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Machine",
	}))

	// 9. Quiesce rule with specific version
	cfgWithVersion := &ScopeConfig{
		QuiesceOnRestore: []QuiesceRule{
			{
				Group:         "custom.io",
				Version:       "v1",
				Kind:          "MyResource",
				AnnotationKey: "custom.io/paused",
			},
		},
	}
	assert.NotNil(t, cfgWithVersion.ShouldQuiesce(schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "MyResource"}))
	assert.Nil(t, cfgWithVersion.ShouldQuiesce(schema.GroupVersionKind{Group: "custom.io", Version: "v2", Kind: "MyResource"}))
}

func TestMergeScopeConfig(t *testing.T) {
	baseline := &ScopeConfig{
		InScope: []InScopeEntry{
			{Group: "cluster.x-k8s.io"},
			{Group: "controlplane.cluster.x-k8s.io"},
		},
		QuiesceOnRestore: []QuiesceRule{
			{
				Group:           "cluster.x-k8s.io",
				Kind:            "Cluster",
				AnnotationKey:   "cluster.x-k8s.io/paused",
				AnnotationValue: "",
			},
			{
				Group:           "foo.io",
				Kind:            "FooRoot",
				AnnotationKey:   "foo.io/paused",
				AnnotationValue: "true",
			},
		},
	}

	delta := &ScopeConfig{
		InScope: []InScopeEntry{
			{Group: "cluster.x-k8s.io"}, // duplicate
			{Group: "postgres-operator.crunchydata.com"},
		},
		QuiesceOnRestore: []QuiesceRule{
			{
				Group:           "cluster.x-k8s.io",
				Kind:            "Cluster",
				AnnotationKey:   "custom.cluster.x-k8s.io/paused", // delta override
				AnnotationValue: "custom-val",
			},
			{
				Group:           "bar.io",
				Kind:            "BarRoot",
				AnnotationKey:   "bar.io/paused",
				AnnotationValue: "true",
			},
		},
	}

	merged := MergeScopeConfig(baseline, delta)
	require.NotNil(t, merged)

	// Check deduplicated inScope
	assert.Len(t, merged.InScope, 3)
	assert.Contains(t, merged.InScope, InScopeEntry{Group: "cluster.x-k8s.io"})
	assert.Contains(t, merged.InScope, InScopeEntry{Group: "controlplane.cluster.x-k8s.io"})
	assert.Contains(t, merged.InScope, InScopeEntry{Group: "postgres-operator.crunchydata.com"})

	// Check quiesce rules: cluster.x-k8s.io/Cluster overridden, foo.io retained, bar.io added
	assert.Len(t, merged.QuiesceOnRestore, 3)
	clusterRule := merged.ShouldQuiesce(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Kind: "Cluster"})
	require.NotNil(t, clusterRule)
	assert.Equal(t, "custom.cluster.x-k8s.io/paused", clusterRule.AnnotationKey)
	assert.Equal(t, "custom-val", clusterRule.AnnotationValue)

	fooRule := merged.ShouldQuiesce(schema.GroupVersionKind{Group: "foo.io", Kind: "FooRoot"})
	require.NotNil(t, fooRule)
	assert.Equal(t, "foo.io/paused", fooRule.AnnotationKey)

	barRule := merged.ShouldQuiesce(schema.GroupVersionKind{Group: "bar.io", Kind: "BarRoot"})
	require.NotNil(t, barRule)
	assert.Equal(t, "bar.io/paused", barRule.AnnotationKey)

	// Verify deterministic slice order: Cluster (overridden in-place), FooRoot, BarRoot
	assert.Equal(t, "cluster.x-k8s.io", merged.QuiesceOnRestore[0].Group)
	assert.Equal(t, "Cluster", merged.QuiesceOnRestore[0].Kind)
	assert.Equal(t, "foo.io", merged.QuiesceOnRestore[1].Group)
	assert.Equal(t, "FooRoot", merged.QuiesceOnRestore[1].Kind)
	assert.Equal(t, "bar.io", merged.QuiesceOnRestore[2].Group)
	assert.Equal(t, "BarRoot", merged.QuiesceOnRestore[2].Kind)
}

func TestLoadScopeConfig(t *testing.T) {
	ctx := context.Background()
	log := logrus.New()

	t.Run("default baseline conventional fallback not found: returns empty scope cleanly", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().Build()
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "", nil, log)
		require.NoError(t, err)
		assert.Nil(t, scope)
	})

	t.Run("explicit baseline missing: logs warning and returns empty scope", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().Build()
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "custom-baseline", nil, log)
		require.NoError(t, err)
		assert.Nil(t, scope)
	})

	t.Run("baseline exists and loaded", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "velero-ownerref-config",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: cluster.x-k8s.io
`,
				QuiesceOnRestoreConfigKey: `
- group: cluster.x-k8s.io
  kind: Cluster
  annotationKey: "cluster.x-k8s.io/paused"
  annotationValue: ""
`,
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "", nil, log)
		require.NoError(t, err)
		require.NotNil(t, scope)
		assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Kind: "Cluster"}))
		rule := scope.ShouldQuiesce(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Kind: "Cluster"})
		require.NotNil(t, rule)
		assert.Equal(t, "cluster.x-k8s.io/paused", rule.AnnotationKey)
	})

	t.Run("per-restore ConfigMap specified but missing: returns error for fail-fast", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().Build()
		restoreRef := &corev1api.TypedLocalObjectReference{
			Kind: velerov1api.OwnerRefConfigMapKind,
			Name: "missing-restore-cm",
		}
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "", restoreRef, log)
		require.Error(t, err)
		assert.Nil(t, scope)
		assert.Contains(t, err.Error(), "failed to load per-restore ConfigMap")
	})

	t.Run("per-restore ConfigMap specified with invalid YAML: returns error", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "invalid-cm",
			},
			Data: map[string]string{
				InScopeConfigKey: "[unclosed bracket",
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		restoreRef := &corev1api.TypedLocalObjectReference{
			Kind: velerov1api.OwnerRefConfigMapKind,
			Name: "invalid-cm",
		}
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "", restoreRef, log)
		require.Error(t, err)
		assert.Nil(t, scope)
		assert.Contains(t, err.Error(), "failed to unmarshal")
	})

	t.Run("per-restore ConfigMap unions onto baseline", func(t *testing.T) {
		baselineCM := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "velero-ownerref-config",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: cluster.x-k8s.io
`,
			},
		}
		deltaCM := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "delta-cm",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: kafka.strimzi.io
`,
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(baselineCM, deltaCM).Build()
		restoreRef := &corev1api.TypedLocalObjectReference{
			Kind: velerov1api.OwnerRefConfigMapKind,
			Name: "delta-cm",
		}
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "", restoreRef, log)
		require.NoError(t, err)
		require.NotNil(t, scope)
		assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Kind: "Cluster"}))
		assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "kafka.strimzi.io", Kind: "Kafka"}))
	})

	t.Run("per-restore ConfigMap specified with deny-listed kind: returns validation error", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "deny-cm",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: ""
  kind: Pod
`,
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		restoreRef := &corev1api.TypedLocalObjectReference{
			Kind: velerov1api.OwnerRefConfigMapKind,
			Name: "deny-cm",
		}
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "", restoreRef, log)
		require.Error(t, err)
		assert.Nil(t, scope)
		assert.Contains(t, err.Error(), "forbidden by the built-in deny list")
	})

	t.Run("quiesce rule without corresponding inScope entries logs warning", func(t *testing.T) {
		testLogger, hook := logrustest.NewNullLogger()
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "velero-ownerref-config",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: other.io
`,
				QuiesceOnRestoreConfigKey: `
- group: cluster.x-k8s.io
  kind: Cluster
  annotationKey: "cluster.x-k8s.io/paused"
`,
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		scope, err := LoadScopeConfig(ctx, fakeClient, "velero", "velero-ownerref-config", nil, testLogger)
		require.NoError(t, err)
		require.NotNil(t, scope)

		foundWarn := false
		for _, entry := range hook.AllEntries() {
			if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "quiesceOnRestore rule for cluster.x-k8s.io/Cluster has no corresponding inScope entries") {
				foundWarn = true
				break
			}
		}
		assert.True(t, foundWarn, "expected warning about quiesceOnRestore without inScope entries")
	})
}

func TestLoadSingleConfigMap(t *testing.T) {
	ctx := context.Background()

	t.Run("nil client returns error", func(t *testing.T) {
		cfg, err := LoadSingleConfigMap(ctx, nil, "velero", "cm")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "client is nil")
	})

	t.Run("missing configmap returns not found error", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "missing")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("configmap with nil data returns error", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "nil-data",
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "nil-data")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "contains neither 'inScope' nor 'quiesceOnRestore' key")
	})

	t.Run("configmap with neither inScope nor quiesceOnRestore returns error", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "other-keys",
			},
			Data: map[string]string{
				"other": "data",
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "other-keys")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), fmt.Sprintf("contains neither '%s' nor '%s' key", InScopeConfigKey, QuiesceOnRestoreConfigKey))
	})

	t.Run("configmap with inScope only", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "inscope-only",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: cluster.x-k8s.io
`,
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "inscope-only")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Len(t, cfg.InScope, 1)
		assert.Empty(t, cfg.QuiesceOnRestore)
	})

	t.Run("configmap with quiesceOnRestore only (annotationKey/annotationValue)", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "quiesce-only",
			},
			Data: map[string]string{
				QuiesceOnRestoreConfigKey: `
- group: cluster.x-k8s.io
  kind: Cluster
  annotationKey: cluster.x-k8s.io/paused
  annotationValue: "true"
`,
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "quiesce-only")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Empty(t, cfg.InScope)
		require.Len(t, cfg.QuiesceOnRestore, 1)
		assert.Equal(t, "Cluster", cfg.QuiesceOnRestore[0].Kind)
		assert.Equal(t, "cluster.x-k8s.io/paused", cfg.QuiesceOnRestore[0].AnnotationKey)
		assert.Equal(t, "true", cfg.QuiesceOnRestore[0].AnnotationValue)
	})

	t.Run("configmap with empty string quiesceOnRestore", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "empty-quiesce",
			},
			Data: map[string]string{
				InScopeConfigKey: `
- group: cluster.x-k8s.io
`,
				QuiesceOnRestoreConfigKey: "",
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "empty-quiesce")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Len(t, cfg.InScope, 1)
		assert.Empty(t, cfg.QuiesceOnRestore)
	})

	t.Run("configmap with invalid YAML in inScope", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "bad-inscope",
			},
			Data: map[string]string{
				InScopeConfigKey: "[unclosed bracket",
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "bad-inscope")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), fmt.Sprintf("failed to unmarshal '%s'", InScopeConfigKey))
	})

	t.Run("configmap with invalid YAML in quiesceOnRestore", func(t *testing.T) {
		cm := &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "velero",
				Name:      "bad-quiesce",
			},
			Data: map[string]string{
				QuiesceOnRestoreConfigKey: "[unclosed bracket",
			},
		}
		fakeClient := fake.NewClientBuilder().WithObjects(cm).Build()
		cfg, err := LoadSingleConfigMap(ctx, fakeClient, "velero", "bad-quiesce")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), fmt.Sprintf("failed to unmarshal '%s'", QuiesceOnRestoreConfigKey))
	})
}

func TestIsDeniedGroupKind(t *testing.T) {
	// Denied kinds
	assert.True(t, IsDeniedGroupKind(schema.GroupKind{Group: "", Kind: "Pod"}))
	assert.True(t, IsDeniedGroupKind(schema.GroupKind{Group: "", Kind: "ReplicationController"}))
	assert.True(t, IsDeniedGroupKind(schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}))
	assert.True(t, IsDeniedGroupKind(schema.GroupKind{Group: "batch", Kind: "Job"}))

	// Non-denied kinds
	assert.False(t, IsDeniedGroupKind(schema.GroupKind{Group: "apps", Kind: "Deployment"}))
	assert.False(t, IsDeniedGroupKind(schema.GroupKind{Group: "apps", Kind: "StatefulSet"}))
	assert.False(t, IsDeniedGroupKind(schema.GroupKind{Group: "batch", Kind: "CronJob"}))
	assert.False(t, IsDeniedGroupKind(schema.GroupKind{Group: "cluster.x-k8s.io", Kind: "Cluster"}))
	assert.False(t, IsDeniedGroupKind(schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"}))
	assert.False(t, IsDeniedGroupKind(schema.GroupKind{Group: "", Kind: "ConfigMap"}))
}

func TestValidateScopeConfig(t *testing.T) {
	t.Run("nil config is valid", func(t *testing.T) {
		assert.NoError(t, ValidateScopeConfig(nil))
	})

	t.Run("valid config with groups and kinds passes", func(t *testing.T) {
		cfg := &ScopeConfig{
			InScope: []InScopeEntry{
				{Group: "cluster.x-k8s.io"},
				{Group: "", Kind: "PersistentVolumeClaim"},
				{Group: "apps", Kind: "Deployment"},
			},
		}
		assert.NoError(t, ValidateScopeConfig(cfg))
	})

	t.Run("entry with both group and kind empty fails", func(t *testing.T) {
		cfg := &ScopeConfig{
			InScope: []InScopeEntry{
				{Group: "", Kind: ""},
			},
		}
		err := ValidateScopeConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must specify at least group or kind")
	})

	t.Run("deny listed kind fails", func(t *testing.T) {
		testCases := []struct {
			group string
			kind  string
		}{
			{"", "Pod"},
			{"", "ReplicationController"},
			{"apps", "ReplicaSet"},
			{"", "ReplicaSet"},
			{"batch", "Job"},
			{"", "Job"},
		}
		for _, tc := range testCases {
			cfg := &ScopeConfig{
				InScope: []InScopeEntry{
					{Group: tc.group, Kind: tc.kind},
				},
			}
			err := ValidateScopeConfig(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "forbidden by the built-in deny list")
		}
	})

	t.Run("quiesce rule without kind fails", func(t *testing.T) {
		cfg := &ScopeConfig{
			QuiesceOnRestore: []QuiesceRule{
				{Group: "cluster.x-k8s.io", AnnotationKey: "cluster.x-k8s.io/paused"},
			},
		}
		err := ValidateScopeConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "quiesceOnRestore rule must specify a kind")
	})

	t.Run("quiesce rule without annotationKey fails", func(t *testing.T) {
		cfg := &ScopeConfig{
			QuiesceOnRestore: []QuiesceRule{
				{Group: "cluster.x-k8s.io", Kind: "Cluster"},
			},
		}
		err := ValidateScopeConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must specify an annotationKey")
	})

	t.Run("valid config with quiesce rules passes", func(t *testing.T) {
		cfg := &ScopeConfig{
			InScope: []InScopeEntry{
				{Group: "cluster.x-k8s.io"},
			},
			QuiesceOnRestore: []QuiesceRule{
				{
					Group:           "cluster.x-k8s.io",
					Kind:            "Cluster",
					AnnotationKey:   "cluster.x-k8s.io/paused",
					AnnotationValue: "",
				},
			},
		}
		assert.NoError(t, ValidateScopeConfig(cfg))
	})

	t.Run("valid config with annotationKey and annotationValue passes", func(t *testing.T) {
		cfg := &ScopeConfig{
			QuiesceOnRestore: []QuiesceRule{
				{
					Group:           "cluster.x-k8s.io",
					Kind:            "Cluster",
					AnnotationKey:   "cluster.x-k8s.io/paused",
					AnnotationValue: "test-val",
				},
			},
		}
		require.NoError(t, ValidateScopeConfig(cfg))
		assert.Equal(t, "cluster.x-k8s.io/paused", cfg.QuiesceOnRestore[0].AnnotationKey)
		assert.Equal(t, "test-val", cfg.QuiesceOnRestore[0].AnnotationValue)
	})
}

func TestFilterDeniedOwnerRefs(t *testing.T) {
	ctrl := true
	block := false
	refs := []metav1.OwnerReference{
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Name:               "parent-pod",
			UID:                "uid-1",
			Controller:         &ctrl,
			BlockOwnerDeletion: &block,
		},
		{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "parent-rs",
			UID:        "uid-2",
		},
		{
			APIVersion: "batch/v1",
			Kind:       "Job",
			Name:       "parent-job",
			UID:        "uid-3",
		},
		{
			APIVersion: "cluster.x-k8s.io/v1beta1",
			Kind:       "Cluster",
			Name:       "parent-cluster",
			UID:        "uid-4",
		},
		{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       "parent-deploy",
			UID:        "uid-5",
		},
	}

	filtered := FilterDeniedOwnerRefs(refs)
	require.Len(t, filtered, 2)
	assert.Equal(t, "Cluster", filtered[0].Kind)
	assert.Equal(t, "parent-cluster", filtered[0].Name)
	assert.Equal(t, "Deployment", filtered[1].Kind)
	assert.Equal(t, "parent-deploy", filtered[1].Name)

	// Empty input
	assert.Nil(t, FilterDeniedOwnerRefs(nil))
	assert.Nil(t, FilterDeniedOwnerRefs([]metav1.OwnerReference{}))
}

func TestFilterDeniedOwnerRefs_PVCParent(t *testing.T) {
	pvcRefs := []metav1.OwnerReference{
		{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       "ephemeral-pod",
			UID:        "pod-uid",
		},
		{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "ephemeral-rs",
			UID:        "rs-uid",
		},
		{
			APIVersion: "batch/v1",
			Kind:       "Job",
			Name:       "ephemeral-job",
			UID:        "job-uid",
		},
		{
			APIVersion: "postgresql.cnpg.io/v1",
			Kind:       "Cluster",
			Name:       "pg-cluster",
			UID:        "pg-uid",
		},
	}

	filtered := FilterDeniedOwnerRefs(pvcRefs)
	require.Len(t, filtered, 1)
	assert.Equal(t, "Cluster", filtered[0].Kind)
	assert.Equal(t, "postgresql.cnpg.io/v1", filtered[0].APIVersion)
	assert.Equal(t, "pg-cluster", filtered[0].Name)
}

func TestOwnerRefRemapState(t *testing.T) {
	state := NewOwnerRefRemapState(true, &ScopeConfig{})
	require.NotNil(t, state)

	oldUID := types.UID("old-uid-123")
	newUID := types.UID("new-uid-456")

	state.RegisterUIDMapping(oldUID, newUID, "workload-ns")
	state.uidMapLock.RLock()
	assert.Equal(t, newUID, state.UIDMap[oldUID].UID)
	assert.Equal(t, "workload-ns", state.UIDMap[oldUID].Namespace)
	state.uidMapLock.RUnlock()

	ctrl := true
	block := true
	origRefs := []metav1.OwnerReference{
		{
			APIVersion:         "cluster.x-k8s.io/v1beta1",
			Kind:               "Cluster",
			Name:               "my-cluster",
			UID:                oldUID,
			Controller:         &ctrl,
			BlockOwnerDeletion: &block,
		},
	}

	state.EnqueueOwnerPatch(
		schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "DockerCluster"},
		"dockerclusters",
		"workload-ns",
		"my-cluster",
		origRefs,
	)

	state.ownerPatchQueueMu.Lock()
	require.Len(t, state.OwnerPatchQueue, 1)
	req := state.OwnerPatchQueue[0]
	assert.Equal(t, "my-cluster", req.Name)
	assert.Equal(t, "workload-ns", req.Namespace)
	require.Len(t, req.OriginalOwnerRefs, 1)
	assert.Equal(t, oldUID, req.OriginalOwnerRefs[0].UID)
	state.ownerPatchQueueMu.Unlock()
}

func TestCopyOwnerReferences(t *testing.T) {
	ctrl := true
	block := false
	refs := []metav1.OwnerReference{
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Name:               "test-pod",
			UID:                "uid-1",
			Controller:         &ctrl,
			BlockOwnerDeletion: &block,
		},
	}

	copied := copyOwnerReferences(refs)
	require.Len(t, copied, 1)
	assert.Equal(t, refs[0].Name, copied[0].Name)
	assert.Equal(t, *refs[0].Controller, *copied[0].Controller)
	assert.Equal(t, *refs[0].BlockOwnerDeletion, *copied[0].BlockOwnerDeletion)

	// Verify deep clone of pointers
	*refs[0].Controller = false
	assert.True(t, *copied[0].Controller)
}

func TestBackupAlreadyPaused(t *testing.T) {
	assert.False(t, backupAlreadyPaused(nil, "cluster.x-k8s.io/paused"))
	assert.False(t, backupAlreadyPaused(map[string]string{}, "cluster.x-k8s.io/paused"))
	assert.False(t, backupAlreadyPaused(map[string]string{"other": ""}, "cluster.x-k8s.io/paused"))
	assert.False(t, backupAlreadyPaused(map[string]string{"cluster.x-k8s.io/paused": ""}, ""))
	assert.True(t, backupAlreadyPaused(map[string]string{"cluster.x-k8s.io/paused": ""}, "cluster.x-k8s.io/paused"))
	assert.True(t, backupAlreadyPaused(map[string]string{"cluster.x-k8s.io/paused": "true"}, "cluster.x-k8s.io/paused"))
}
