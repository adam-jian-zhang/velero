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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	velerotest "github.com/vmware-tanzu/velero/pkg/test"
)

func TestScopeDenyList(t *testing.T) {
	scope := NewScope()

	deniedGVKs := []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "Pod"},
		{Group: "", Version: "v1", Kind: "ReplicationController"},
		{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		{Group: "batch", Version: "v1", Kind: "Job"},
	}

	for _, gvk := range deniedGVKs {
		assert.False(t, scope.IsInScope(gvk), "GVK %v should be denied", gvk)
	}

	// Top-level workloads are NOT denied, but are also not in-scope by default in a bare scope
	topLevelWorkloadGVKs := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"},
		{Group: "batch", Version: "v1", Kind: "CronJob"},
	}
	for _, gvk := range topLevelWorkloadGVKs {
		assert.False(t, scope.IsInScope(gvk), "Top-level workload %v should not be in-scope by default", gvk)
	}

	// When added via userEntries / ConfigMap inScope, top-level workloads become in-scope!
	scopeWithTopLevel := NewScope()
	for _, gvk := range topLevelWorkloadGVKs {
		scopeWithTopLevel.userEntries = append(scopeWithTopLevel.userEntries, ScopeEntry{Group: gvk.Group, Kind: gvk.Kind})
	}
	for _, gvk := range topLevelWorkloadGVKs {
		assert.True(t, scopeWithTopLevel.IsInScope(gvk), "Top-level workload %v should be allowed when in userEntries", gvk)
	}

	// Leaf workloads must remain denied even if explicitly listed in userEntries!
	scopeWithDeniedUserEntries := NewScope()
	for _, gvk := range deniedGVKs {
		scopeWithDeniedUserEntries.userEntries = append(scopeWithDeniedUserEntries.userEntries, ScopeEntry{Group: gvk.Group, Kind: gvk.Kind})
	}
	for _, gvk := range deniedGVKs {
		assert.False(t, scopeWithDeniedUserEntries.IsInScope(gvk), "Leaf workload %v must remain denied even if listed in userEntries", gvk)
	}

	// Kind name collision: custom CRD sharing a kind name (e.g. sparkoperator.k8s.io/Pod) is NOT denied
	sparkPodGVK := schema.GroupVersionKind{Group: "sparkoperator.k8s.io", Version: "v1", Kind: "Pod"}
	assert.False(t, scope.IsInScope(sparkPodGVK))

	// Add sparkoperator.k8s.io to user config:
	scope.userEntries = append(scope.userEntries, ScopeEntry{Group: "sparkoperator.k8s.io", Kind: "Pod"})
	assert.True(t, scope.IsInScope(sparkPodGVK), "Custom CRD with Kind Pod should be allowed when in userEntries")

	// Even if user adds core Pod to userEntries, deny list must win!
	scope.userEntries = append(scope.userEntries, ScopeEntry{Group: "", Kind: "Pod"})
	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}), "Core Pod must remain denied even if listed in userEntries")
}

func TestDefaultBaselineConfigMap(t *testing.T) {
	scope := NewScope()

	capiGVKs := []schema.GroupVersionKind{
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"},
		{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta1", Kind: "KubeadmControlPlane"},
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "VSphereCluster"},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
	}

	// In a bare scope without baseline ConfigMap, nothing is in scope by default
	for _, gvk := range capiGVKs {
		assert.False(t, scope.IsInScope(gvk), "Bare scope should not have GVK %v in scope before baseline ConfigMap is merged", gvk)
	}

	// Merge the curated baseline ConfigMap loaded from examples
	baselineCM, err := velerotest.LoadBaselineOwnerRefConfigMapFromExample("velero")
	require.NoError(t, err)
	require.NoError(t, scope.MergeConfigMap(baselineCM))

	// Now all CAPI GVKs and PersistentVolumeClaim are in scope
	for _, gvk := range capiGVKs {
		assert.True(t, scope.IsInScope(gvk), "GVK %v should be allowed after baseline ConfigMap is merged", gvk)
	}

	// KubeVirt is not in baseline ConfigMap
	kubevirtGVK := schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
	assert.False(t, scope.IsInScope(kubevirtGVK), "KubeVirt should not be in baseline ConfigMap")

	unrelatedGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "MyCRD"}
	assert.False(t, scope.IsInScope(unrelatedGVK))

	// Verify specRefPaths are populated from baseline
	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}
	assert.True(t, scope.HasSpecRefPaths(clusterGVK))
	paths := scope.SpecRefPathsFor(clusterGVK)
	assert.Contains(t, paths, "spec.infrastructureRef")
	assert.Contains(t, paths, "spec.controlPlaneRef")

	machineGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"}
	assert.True(t, scope.HasSpecRefPaths(machineGVK))
	mPaths := scope.SpecRefPathsFor(machineGVK)
	assert.Contains(t, mPaths, "spec.infrastructureRef")
	assert.Contains(t, mPaths, "spec.bootstrap.configRef")

	// Verify CAPI Cluster pause rule is populated from baseline
	rule, ok := scope.MatchesQuiesceRule(clusterGVK)
	assert.True(t, ok)
	assert.Equal(t, "cluster.x-k8s.io/paused", rule.AnnotationKey)
}

func TestScopeSpecRefPathsAndQuiesce(t *testing.T) {
	scope := NewScope()
	baselineCM, err := velerotest.LoadBaselineOwnerRefConfigMapFromExample("velero")
	require.NoError(t, err)
	require.NoError(t, scope.MergeConfigMap(baselineCM))

	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}
	assert.True(t, scope.HasSpecRefPaths(clusterGVK))
	paths := scope.SpecRefPathsFor(clusterGVK)
	assert.Contains(t, paths, "spec.infrastructureRef")
	assert.Contains(t, paths, "spec.controlPlaneRef")

	rule, ok := scope.MatchesQuiesceRule(clusterGVK)
	assert.True(t, ok)
	assert.Equal(t, "cluster.x-k8s.io/paused", rule.AnnotationKey)

	_, ok = scope.MatchesQuiesceRule(schema.GroupVersionKind{Group: "other.io", Version: "v1", Kind: "Foo"})
	assert.False(t, ok)
}

func TestLoadScopeFromConfigMap(t *testing.T) {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "velero",
		},
		Data: map[string]string{
			ConfigMapKeyInScope: `
- group: cert-manager.io
- group: custom.io
  kind: App
`,
			ConfigMapKeySpecRefPaths: `
- group: custom.io
  kind: App
  paths:
    - spec.targetRef
`,
			ConfigMapKeyQuiesceOnRestore: `
- group: custom.io
  kind: App
  annotationKey: custom.io/pause
  annotationValue: "true"
`,
		},
	}

	scope, err := LoadScopeFromConfigMap(cm)
	require.NoError(t, err)

	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}))
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "App"}))
	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "Other"}))

	// Custom specRefPath:
	appGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "App"}
	assert.True(t, scope.HasSpecRefPaths(appGVK))
	assert.Equal(t, []string{"spec.targetRef"}, scope.SpecRefPathsFor(appGVK))

	// Custom quiesce:
	qRule, ok := scope.MatchesQuiesceRule(appGVK)
	assert.True(t, ok)
	assert.Equal(t, "custom.io/pause", qRule.AnnotationKey)
	assert.Equal(t, "true", qRule.AnnotationValue)
}

func TestMergeConfigMap_2TierUnionAndQuiesceOverride(t *testing.T) {
	scope := NewScope()

	// Tier 1: Baseline ConfigMap (CAPI + PVC)
	baselineCM, err := velerotest.LoadBaselineOwnerRefConfigMapFromExample("velero")
	require.NoError(t, err)
	require.NoError(t, scope.MergeConfigMap(baselineCM))

	// Verify Baseline is in scope
	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}
	assert.True(t, scope.IsInScope(clusterGVK))
	rule, ok := scope.MatchesQuiesceRule(clusterGVK)
	require.True(t, ok)
	assert.Equal(t, "cluster.x-k8s.io/paused", rule.AnnotationKey)

	// Tier 2: Restore ConfigMap with additions and an override for Cluster quiesce rule
	restoreCM := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-custom-cm",
			Namespace: "velero",
		},
		Data: map[string]string{
			ConfigMapKeyInScope: `
- group: database.example.io
- group: custom.io
  kind: App
`,
			ConfigMapKeySpecRefPaths: `
- group: cluster.x-k8s.io
  kind: Cluster
  paths:
    - spec.infrastructureRef
    - spec.customExtensionRef
- group: custom.io
  kind: App
  paths:
    - spec.dbRef
`,
			ConfigMapKeyQuiesceOnRestore: `
- group: cluster.x-k8s.io
  kind: Cluster
  annotationKey: custom-cluster.io/paused
  annotationValue: "override-value"
- group: custom.io
  kind: App
  annotationKey: custom.io/paused
  annotationValue: "app-paused"
`,
		},
	}
	require.NoError(t, scope.MergeConfigMap(restoreCM))

	// 1. inScope Additive Set Union: both CAPI and database.example.io are in scope
	assert.True(t, scope.IsInScope(clusterGVK), "CAPI Cluster should remain in scope")
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"}), "PVC should remain in scope")
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "database.example.io", Version: "v1", Kind: "PostgresCluster"}), "New group database.example.io should be in scope")
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "App"}), "custom.io/App should be in scope")

	// 2. specRefPaths Deduplicated Union:
	cPaths := scope.SpecRefPathsFor(clusterGVK)
	assert.Contains(t, cPaths, "spec.infrastructureRef")
	assert.Contains(t, cPaths, "spec.controlPlaneRef")
	assert.Contains(t, cPaths, "spec.customExtensionRef")
	assert.Len(t, cPaths, 3, "spec.infrastructureRef should be deduplicated")

	appGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "App"}
	assert.Equal(t, []string{"spec.dbRef"}, scope.SpecRefPathsFor(appGVK))

	// 3. quiesceOnRestore Override: restore rule replaces baseline rule for (cluster.x-k8s.io, Cluster)
	overriddenRule, ok := scope.MatchesQuiesceRule(clusterGVK)
	require.True(t, ok)
	assert.Equal(t, "custom-cluster.io/paused", overriddenRule.AnnotationKey, "Restore rule should override baseline rule for Cluster")
	assert.Equal(t, "override-value", overriddenRule.AnnotationValue)

	// New quiesce rule for App is present
	appRule, ok := scope.MatchesQuiesceRule(appGVK)
	require.True(t, ok)
	assert.Equal(t, "custom.io/paused", appRule.AnnotationKey)
}

func TestSpecRefPathsFor_Deduplication(t *testing.T) {
	scope := NewScope()
	baselineCM, err := velerotest.LoadBaselineOwnerRefConfigMapFromExample("velero")
	require.NoError(t, err)
	require.NoError(t, scope.MergeConfigMap(baselineCM))

	// Add an entry with duplicate paths, some already present in baseline
	scope.SpecRefPaths = append(scope.SpecRefPaths, SpecRefPathEntry{
		Group: "cluster.x-k8s.io",
		Kind:  "Cluster",
		Paths: []string{
			"spec.infrastructureRef", // duplicate of baseline
			"spec.customRef",
			"spec.customRef", // duplicate within entry
		},
	})

	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}
	paths := scope.SpecRefPathsFor(clusterGVK)

	assert.Equal(t, []string{
		"spec.infrastructureRef",
		"spec.controlPlaneRef",
		"spec.customRef",
	}, paths)
}

func TestScopeEmptyEntryDoesNotMatchCoreResources(t *testing.T) {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-empty-config",
			Namespace: "velero",
		},
		Data: map[string]string{
			"inScope": `
- group: ""
  kind: ""
- {}
- group: "   "
`,
		},
	}

	_, err := LoadScopeFromConfigMap(cm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty group and kind")

	scope := NewScope()
	coreGVKs := []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "ConfigMap"},
		{Group: "", Version: "v1", Kind: "Secret"},
		{Group: "", Version: "v1", Kind: "Service"},
		{Group: "", Version: "v1", Kind: "ServiceAccount"},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
	}

	for _, gvk := range coreGVKs {
		assert.False(t, scope.IsInScope(gvk), "Core resource %v must NOT match empty inScope entry or be in bare scope", gvk)
	}

	// Once baseline ConfigMap is loaded, PersistentVolumeClaim is in scope, but not other core resources
	baselineCM, err := velerotest.LoadBaselineOwnerRefConfigMapFromExample("velero")
	require.NoError(t, err)
	require.NoError(t, scope.MergeConfigMap(baselineCM))
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"}))
	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}))

	scope.userEntries = append(scope.userEntries, ScopeEntry{Group: "", Kind: ""})
	for _, gvk := range coreGVKs[:4] {
		assert.False(t, scope.IsInScope(gvk), "Core resource %v must NOT match even if empty ScopeEntry is in userEntries", gvk)
	}
}

func TestLoadScopeFromConfigMapRejectsDenyList(t *testing.T) {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "deny", Namespace: "velero"},
		Data: map[string]string{
			ConfigMapKeyInScope: `
- group: apps
  kind: ReplicaSet
`,
		},
	}
	_, err := LoadScopeFromConfigMap(cm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deny list")
}

func TestLoadScopeFromConfigMapRejectsJSONPath(t *testing.T) {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "jsonpath", Namespace: "velero"},
		Data: map[string]string{
			ConfigMapKeySpecRefPaths: `
- group: custom.io
  kind: App
  paths:
    - $.spec.targetRef
`,
		},
	}
	_, err := LoadScopeFromConfigMap(cm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSONPath syntax is not supported")
}

func TestLoadScopeFromConfigMapRejectsSpecFieldPath(t *testing.T) {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "specfield", Namespace: "velero"},
		Data: map[string]string{
			ConfigMapKeyQuiesceOnRestore: `
- group: custom.io
  kind: App
  specFieldPath: spec.paused
`,
		},
	}
	_, err := LoadScopeFromConfigMap(cm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "specFieldPath is not supported")
}

func TestIsDeniedOwner(t *testing.T) {
	scope := NewScope()
	assert.True(t, scope.IsDeniedOwner("v1", "Pod"))
	assert.True(t, scope.IsDeniedOwner("apps/v1", "ReplicaSet"))
	assert.True(t, scope.IsDeniedOwner("batch/v1", "Job"))
	assert.True(t, scope.IsDeniedOwner("v1", "ReplicationController"))
	assert.False(t, scope.IsDeniedOwner("database.example.io/v1", "DatabaseCluster"))
	assert.False(t, scope.IsDeniedOwner("cluster.x-k8s.io/v1beta1", "Cluster"))
	assert.False(t, (*Scope)(nil).IsDeniedOwner("v1", "Pod"))
}

func TestDenyListGroupKinds_Immutability(t *testing.T) {
	list1 := DenyListGroupKinds()
	require.NotEmpty(t, list1)
	// Mutate list1
	list1[0] = schema.GroupKind{Group: "malicious", Kind: "Hacked"}

	list2 := DenyListGroupKinds()
	assert.NotEqual(t, "malicious", list2[0].Group)
	assert.Equal(t, "Pod", list2[0].Kind)

	scope := NewScope()
	assert.True(t, scope.IsDeniedOwner("v1", "Pod"))
}

func TestMergeConfigMap_WhitespaceTrimmed(t *testing.T) {
	scope := NewScope()
	cm := &corev1api.ConfigMap{
		Data: map[string]string{
			ConfigMapKeyInScope: `
- group: "  custom.io  "
  kind: "  App  "
`,
			ConfigMapKeySpecRefPaths: `
- group: "  custom.io  "
  kind: "  App  "
  paths:
    - "  spec.targetRef  "
`,
			ConfigMapKeyQuiesceOnRestore: `
- group: "  custom.io  "
  kind: "  App  "
  annotationKey: "  custom.io/paused  "
  annotationValue: "  true  "
`,
		},
	}
	require.NoError(t, scope.MergeConfigMap(cm))
	appGVK := schema.GroupVersionKind{Group: "custom.io", Version: "v1", Kind: "App"}
	assert.True(t, scope.IsInScope(appGVK))
	assert.Equal(t, []string{"spec.targetRef"}, scope.SpecRefPathsFor(appGVK))
	rule, ok := scope.MatchesQuiesceRule(appGVK)
	require.True(t, ok)
	assert.Equal(t, "custom.io", rule.Group)
	assert.Equal(t, "App", rule.Kind)
	assert.Equal(t, "custom.io/paused", rule.AnnotationKey)
	assert.Equal(t, "true", rule.AnnotationValue)
}

func TestValidateDottedSpecPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "invalid bare spec", path: "spec", wantErr: true},
		{name: "invalid bare spec with dot", path: "spec.", wantErr: true},
		{name: "invalid trailing dot", path: "spec.ref.", wantErr: true},
		{name: "valid simple spec path", path: "spec.ref", wantErr: false},
		{name: "valid nested spec path", path: "spec.infrastructureRef", wantErr: false},
		{name: "valid wildcard path", path: "spec.template.spec.volumes[*].dataVolume", wantErr: false},
		{name: "valid wildcard path at end", path: "spec.clusterRefs[*]", wantErr: false},
		{name: "empty path", path: "", wantErr: true},
		{name: "jsonpath prefix $", path: "$.spec.infrastructureRef", wantErr: true},
		{name: "jsonpath recursive descent ..", path: "spec..volumes", wantErr: true},
		{name: "not starting with spec", path: "status.ref", wantErr: true},
		{name: "array index [0]", path: "spec.volumes[0].persistentVolumeClaim", wantErr: true},
		{name: "array index [123]", path: "spec.volumes[123]", wantErr: true},
		{name: "unmatched bracket [", path: "spec.volumes[", wantErr: true},
		{name: "unmatched bracket ]", path: "spec.volumes]", wantErr: true},
		{name: "empty bracket []", path: "spec.volumes[]", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDottedSpecPath(tt.path)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
