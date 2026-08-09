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

package dag

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestAccumulatorRecordItem(t *testing.T) {
	acc := NewAccumulator()
	parentUID := types.UID("parent-1")
	childUID := types.UID("child-1")

	child := &unstructured.Unstructured{}
	child.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	child.SetKind("Machine")
	child.SetNamespace("default")
	child.SetName("m-1")
	child.SetUID(childUID)
	child.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "cluster.x-k8s.io/v1beta1",
		Kind:       "MachineSet",
		Name:       "ms-1",
		UID:        parentUID,
	}})

	acc.RecordItem(child)
	snapshot := acc.Snapshot()

	require.Equal(t, ResourceDAGVersion, snapshot.Version)
	require.Contains(t, snapshot.Nodes, childUID)
	assert.Equal(t, "Machine", snapshot.Nodes[childUID].Kind)
	require.Len(t, snapshot.Edges, 1)
	assert.Equal(t, parentUID, snapshot.Edges[0].ParentUID)
	assert.Equal(t, childUID, snapshot.Edges[0].ChildUID)
}

func TestAccumulatorMalformedOwnerRef(t *testing.T) {
	acc := NewAccumulator()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	obj.SetKind("Machine")
	obj.SetNamespace("default")
	obj.SetName("m-1")
	obj.SetUID("child-1")
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		Kind: "MachineSet",
		Name: "ms-1",
		UID:  "",
	}})

	acc.RecordItem(obj)
	snapshot := acc.Snapshot()
	assert.Empty(t, snapshot.Edges)
	require.NotEmpty(t, snapshot.Warnings)
	assert.Contains(t, snapshot.Warnings[0], "malformed ownerRef")
}

func TestAccumulatorMerge(t *testing.T) {
	acc := NewAccumulator()
	existing := &ResourceDAG{
		Version: ResourceDAGVersion,
		Nodes: map[types.UID]ResourceNode{
			"a": {Kind: "Cluster", Name: "c1", Namespace: "default", APIVersion: "cluster.x-k8s.io/v1beta1"},
		},
		Edges: []ResourceEdge{{ParentUID: "a", ChildUID: "b"}},
	}
	acc.Merge(existing)

	child := &unstructured.Unstructured{}
	child.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	child.SetKind("Machine")
	child.SetName("m1")
	child.SetNamespace("default")
	child.SetUID("b")
	acc.RecordItem(child)

	snapshot := acc.Snapshot()
	assert.Contains(t, snapshot.Nodes, types.UID("a"))
	assert.Contains(t, snapshot.Nodes, types.UID("b"))
	assert.Len(t, snapshot.Edges, 1)
}

func TestDetectCycles(t *testing.T) {
	d := &ResourceDAG{
		Version: ResourceDAGVersion,
		Nodes: map[types.UID]ResourceNode{
			"a": {Name: "a"},
			"b": {Name: "b"},
		},
		Edges: []ResourceEdge{
			{ParentUID: "a", ChildUID: "b"},
			{ParentUID: "b", ChildUID: "a"},
		},
	}
	warnings := DetectCycles(d)
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0], "cycle detected")

	// Snapshot should still serialize with cycle warnings appended
	acc := NewAccumulator()
	acc.Merge(d)
	snapshot := acc.Snapshot()
	require.NotEmpty(t, snapshot.Warnings)
	assert.Len(t, snapshot.Edges, 2)
}

func TestScopeDenyListWins(t *testing.T) {
	scope := NewScope()
	scope.LoadFromConfigMap(&corev1api.ConfigMap{
		Data: map[string]string{
			ConfigMapInScopeKey: `
- group: apps
  kind: Deployment
`,
		},
	}, logrus.New())

	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}))
	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}))
}

func TestScopeSeedAndConfigMap(t *testing.T) {
	scope := NewScope()
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"}))
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}))
	assert.False(t, scope.IsInScope(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "MyApp"}))

	scope.LoadFromConfigMap(&corev1api.ConfigMap{
		Data: map[string]string{
			ConfigMapInScopeKey: `
- group: example.io
  version: v1
  kind: MyApp
`,
			ConfigMapSpecRefPathsKey: `
- group: example.io
  version: v1
  kind: MyApp
  jsonPaths:
    - spec.ref
`,
		},
	}, logrus.New())

	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "MyApp"}))
	paths := scope.SpecRefPathsFor(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "MyApp"})
	assert.Equal(t, []string{"spec.ref"}, paths)
	// Built-in seed still present
	assert.NotEmpty(t, scope.SpecRefPathsFor(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}))
}

func TestScopeInvalidConfigMapEntries(t *testing.T) {
	scope := NewScope()
	scope.LoadFromConfigMap(&corev1api.ConfigMap{
		Data: map[string]string{
			ConfigMapInScopeKey: `
- group: ""
  kind: Broken
- group: cert-manager.io
`,
			ConfigMapSpecRefPathsKey: `
- group: example.io
  kind: ""
  jsonPaths: []
`,
		},
	}, logrus.New())
	assert.True(t, scope.IsInScope(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}))
}
