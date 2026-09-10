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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

func TestResolveQuiescedRoots_DirectParent(t *testing.T) {
	state := NewOwnerRefRelinkState()
	clusterRec := velerov1api.QuiescedObjectRef{
		Group:         "cluster.x-k8s.io",
		Version:       "v1beta1",
		Kind:          "Cluster",
		Namespace:     "default",
		Name:          "cluster-1",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}
	state.RecordQuiescedObjectWithUID("uid-cluster-1", clusterRec)

	req := OwnerPatchRequest{
		OldUID:    "uid-md-1",
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "MachineDeployment",
		Namespace: "default",
		Name:      "md-1",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "cluster.x-k8s.io/v1beta1",
				Kind:       "Cluster",
				Name:       "cluster-1",
				UID:        "uid-cluster-1",
			},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	require.Len(t, roots, 1)
	assert.Equal(t, "cluster.x-k8s.io", roots[0].Group)
	assert.Equal(t, "Cluster", roots[0].Kind)
	assert.Equal(t, "default", roots[0].Namespace)
	assert.Equal(t, "cluster-1", roots[0].Name)
}

func TestResolveQuiescedRoots_MultiHopChain(t *testing.T) {
	// Hierarchy:
	// Cluster (quiesced, uid-cluster) <- MachineDeployment (uid-md) <- MachineSet (uid-ms) <- Machine (uid-m)
	state := NewOwnerRefRelinkState()
	clusterRec := velerov1api.QuiescedObjectRef{
		Group:         "cluster.x-k8s.io",
		Version:       "v1beta1",
		Kind:          "Cluster",
		Namespace:     "prod-ns",
		Name:          "cluster-prod",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}
	state.RecordQuiescedObjectWithUID("uid-cluster", clusterRec)

	// Register parent linkages
	state.RegisterParentUID("uid-md", "uid-cluster")
	state.RegisterParentUID("uid-ms", "uid-md")
	state.RegisterParentUID("uid-m", "uid-ms")

	req := OwnerPatchRequest{
		OldUID:    "uid-m",
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Namespace: "prod-ns",
		Name:      "machine-1",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "cluster.x-k8s.io/v1beta1",
				Kind:       "MachineSet",
				Name:       "ms-1",
				UID:        "uid-ms",
			},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	require.Len(t, roots, 1, "Must find quiesced Cluster 3 hops away")
	assert.Equal(t, "cluster.x-k8s.io", roots[0].Group)
	assert.Equal(t, "Cluster", roots[0].Kind)
	assert.Equal(t, "prod-ns", roots[0].Namespace)
	assert.Equal(t, "cluster-prod", roots[0].Name)
}

func TestResolveQuiescedRoots_DiamondDAG(t *testing.T) {
	// Diamond DAG:
	// Cluster (quiesced, uid-cluster)
	//   <- NodeA (uid-a)
	//   <- NodeB (uid-b)
	// Both own Leaf (uid-leaf)
	state := NewOwnerRefRelinkState()
	clusterRec := velerov1api.QuiescedObjectRef{
		Group:         "example.io",
		Kind:          "RootApp",
		Namespace:     "default",
		Name:          "app-root",
		AnnotationKey: "example.io/paused",
	}
	state.RecordQuiescedObjectWithUID("uid-cluster", clusterRec)

	state.RegisterParentUID("uid-a", "uid-cluster")
	state.RegisterParentUID("uid-b", "uid-cluster")
	state.RegisterParentUID("uid-leaf", "uid-a")
	state.RegisterParentUID("uid-leaf", "uid-b")

	req := OwnerPatchRequest{
		OldUID:    "uid-leaf",
		Group:     "example.io",
		Kind:      "Leaf",
		Namespace: "default",
		Name:      "leaf-1",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{Kind: "NodeA", Name: "node-a", UID: "uid-a"},
			{Kind: "NodeB", Name: "node-b", UID: "uid-b"},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	require.Len(t, roots, 1, "Must deduplicate multiple paths to same quiesced root")
	assert.Equal(t, "RootApp", roots[0].Kind)
	assert.Equal(t, "app-root", roots[0].Name)
}

func TestResolveQuiescedRoots_CycleDetection(t *testing.T) {
	// Circular reference: A -> B -> A
	state := NewOwnerRefRelinkState()
	state.RegisterParentUID("uid-a", "uid-b")
	state.RegisterParentUID("uid-b", "uid-a")

	req := OwnerPatchRequest{
		OldUID: "uid-a",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{Kind: "B", Name: "b", UID: "uid-b"},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	assert.Empty(t, roots, "Must safely terminate and return empty on cycle with no quiesced root")
}

func TestResolveQuiescedRoots_UnquiescedHierarchy(t *testing.T) {
	state := NewOwnerRefRelinkState()
	state.RegisterParentUID("uid-child", "uid-parent")

	req := OwnerPatchRequest{
		OldUID: "uid-child",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{Kind: "Parent", Name: "parent", UID: "uid-parent"},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	assert.Empty(t, roots, "Must return nil/empty when no quiesced ancestor exists")
}

func TestResolveQuiescedRoots_MultipleQuiescedRoots(t *testing.T) {
	state := NewOwnerRefRelinkState()
	c1 := velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "cluster-1",
	}
	c2 := velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "cluster-2",
	}
	state.RecordQuiescedObjectWithUID("uid-c1", c1)
	state.RecordQuiescedObjectWithUID("uid-c2", c2)

	// Object shared or depending on both
	state.RegisterParentUID("uid-shared", "uid-c1")
	state.RegisterParentUID("uid-shared", "uid-c2")

	req := OwnerPatchRequest{
		OldUID: "uid-shared",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{Kind: "Cluster", Name: "cluster-1", UID: "uid-c1"},
			{Kind: "Cluster", Name: "cluster-2", UID: "uid-c2"},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	require.Len(t, roots, 2, "Must return both quiesced roots")
}

func TestResolveQuiescedRoots_ClusterScopedQuiescedRoot(t *testing.T) {
	state := NewOwnerRefRelinkState()
	clusterScopedRec := velerov1api.QuiescedObjectRef{
		Group:         "topology.x-k8s.io",
		Version:       "v1alpha1",
		Kind:          "ClusterTopology",
		Namespace:     "",
		Name:          "topology-global",
		AnnotationKey: "topology.x-k8s.io/paused",
	}
	state.RecordQuiescedObjectWithUID("uid-topo", clusterScopedRec)

	req := OwnerPatchRequest{
		OldUID:    "uid-child",
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Cluster",
		Namespace: "tenant-ns",
		Name:      "cluster-1",
		OriginalOwnerRefs: []metav1.OwnerReference{
			{
				APIVersion: "topology.x-k8s.io/v1alpha1",
				Kind:       "ClusterTopology",
				Name:       "topology-global",
				UID:        "uid-topo",
			},
		},
	}

	roots := state.ResolveQuiescedRoots(req)
	require.Len(t, roots, 1)
	assert.Equal(t, "topology.x-k8s.io", roots[0].Group)
	assert.Equal(t, "ClusterTopology", roots[0].Kind)
	assert.Empty(t, roots[0].Namespace, "Cluster-scoped root must keep empty namespace, not be overwritten with child's namespace")
	assert.Equal(t, "topology-global", roots[0].Name)
}

func TestBlockUnquiesceForTarget_EmptyGroup(t *testing.T) {
	state := NewOwnerRefRelinkState()
	c1 := velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "cluster-1",
	}
	state.RecordQuiescedObject(c1)

	// Blocking with empty group (omitted apiVersion in target ref) should still match and block
	state.BlockUnquiesceForTarget("", "Cluster", "default", "cluster-1")
	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.True(t, quiesced[0].UnquiesceBlocked, "Target with empty group must match and set UnquiesceBlocked")
}
