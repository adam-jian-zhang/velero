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
)

func TestScopeDenyList(t *testing.T) {
	scope := NewScope()

	deniedGVKs := []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "Pod"},
		{Group: "", Version: "v1", Kind: "ReplicationController"},
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"},
		{Group: "batch", Version: "v1", Kind: "Job"},
		{Group: "batch", Version: "v1", Kind: "CronJob"},
	}

	for _, gvk := range deniedGVKs {
		assert.False(t, scope.IsInScope(gvk), "GVK %v should be denied", gvk)
	}

	// Kind name collision: custom CRD sharing a kind name (e.g. sparkoperator.k8s.io/Pod) is NOT denied
	sparkPodGVK := schema.GroupVersionKind{Group: "sparkoperator.k8s.io", Version: "v1", Kind: "Pod"}
	// It's not in built-in seeds, so false unless user config added:
	assert.False(t, scope.IsInScope(sparkPodGVK))

	// Add sparkoperator.k8s.io to user config:
	scope.userEntries = append(scope.userEntries, ScopeEntry{Group: "sparkoperator.k8s.io", Kind: "Pod"})
	assert.True(t, scope.IsInScope(sparkPodGVK), "Custom CRD with Kind Pod should be allowed when in userEntries")

	// Even if user adds core Pod to userEntries, deny list must win!
	scope.userEntries = append(scope.userEntries, ScopeEntry{Group: "", Kind: "Pod"})
	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}), "Core Pod must remain denied even if listed in userEntries")
}

func TestScopeBuiltInSeeds(t *testing.T) {
	scope := NewScope()

	allowedGVKs := []schema.GroupVersionKind{
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"},
		{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta1", Kind: "KubeadmControlPlane"},
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "VSphereCluster"},
		{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"},
		{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"},
		{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"},
	}

	for _, gvk := range allowedGVKs {
		assert.True(t, scope.IsInScope(gvk), "GVK %v should be allowed by built-in seed", gvk)
	}

	unrelatedGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "MyCRD"}
	assert.False(t, scope.IsInScope(unrelatedGVK))
}

func TestScopeSpecRefPathsAndQuiesce(t *testing.T) {
	scope := NewScope()

	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}
	assert.True(t, scope.HasSpecRefPaths(clusterGVK))
	paths := scope.SpecRefPathsFor(clusterGVK)
	assert.Contains(t, paths, "spec.infrastructureRef")
	assert.Contains(t, paths, "spec.controlPlaneRef")

	vmGVK := schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
	assert.True(t, scope.HasSpecRefPaths(vmGVK))
	vmPaths := scope.SpecRefPathsFor(vmGVK)
	assert.Contains(t, vmPaths, "spec.template.spec.volumes[*].dataVolume")

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
			"inScope": `
- group: cert-manager.io
- group: custom.io
  kind: App
`,
			"specRefPaths": `
- group: custom.io
  kind: App
  jsonPaths:
    - spec.targetRef
`,
			"quiesceOnRestore": `
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

	// Built-in seeds preserved:
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}))

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

func TestSpecRefPathsFor_Deduplication(t *testing.T) {
	scope := NewScope()
	// Add an entry with duplicate paths, some already present in built-in
	scope.SpecRefPaths = append(scope.SpecRefPaths, SpecRefPathEntry{
		Group: "cluster.x-k8s.io",
		Kind:  "Cluster",
		JSONPaths: []string{
			"spec.infrastructureRef", // duplicate of built-in
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

	scope, err := LoadScopeFromConfigMap(cm)
	require.NoError(t, err)

	// Core resources should not be in scope because empty entries are ignored
	coreGVKs := []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "ConfigMap"},
		{Group: "", Version: "v1", Kind: "Secret"},
		{Group: "", Version: "v1", Kind: "Service"},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
	}

	for _, gvk := range coreGVKs {
		assert.False(t, scope.IsInScope(gvk), "Core resource %v must NOT match empty inScope entry", gvk)
	}

	// Also test manual userEntry with empty group and empty kind
	scope.userEntries = append(scope.userEntries, ScopeEntry{Group: "", Kind: ""})
	for _, gvk := range coreGVKs {
		assert.False(t, scope.IsInScope(gvk), "Core resource %v must NOT match even if empty ScopeEntry is in userEntries", gvk)
	}
}
