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
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Accumulator is a lock-protected collector of nodes and ownership edges.
// Shared across backup worker goroutines and optionally merged across backup passes.
type Accumulator struct {
	mu    sync.Mutex
	nodes map[types.UID]ResourceNode
	edges []ResourceEdge
	warns []string
}

// NewAccumulator returns an empty Accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{
		nodes: make(map[types.UID]ResourceNode),
	}
}

// RecordItem records a backed-up object and its ownerReference edges.
func (a *Accumulator) RecordItem(obj *unstructured.Unstructured) {
	if a == nil || obj == nil {
		return
	}
	uid := obj.GetUID()
	if uid == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.nodes[uid] = ResourceNode{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}

	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == "" {
			a.warns = append(a.warns, fmt.Sprintf(
				"malformed ownerRef on %s/%s: empty uid (kind=%s name=%s)",
				obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name,
			))
			continue
		}
		a.edges = append(a.edges, ResourceEdge{
			ParentUID: ref.UID,
			ChildUID:  uid,
		})
	}
}

// Merge loads nodes/edges/warnings from an existing ResourceDAG into the accumulator.
// Existing nodes with the same UID are overwritten (last-write wins).
func (a *Accumulator) Merge(existing *ResourceDAG) {
	if a == nil || existing == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.nodes == nil {
		a.nodes = make(map[types.UID]ResourceNode)
	}
	for uid, node := range existing.Nodes {
		a.nodes[uid] = node
	}
	a.edges = append(a.edges, existing.Edges...)
	a.warns = append(a.warns, existing.Warnings...)
}

// Snapshot returns a deep copy of the current graph and runs cycle detection.
func (a *Accumulator) Snapshot() *ResourceDAG {
	if a == nil {
		return &ResourceDAG{
			Version: ResourceDAGVersion,
			Nodes:   make(map[types.UID]ResourceNode),
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	nodes := make(map[types.UID]ResourceNode, len(a.nodes))
	for uid, node := range a.nodes {
		nodes[uid] = node
	}
	edges := make([]ResourceEdge, len(a.edges))
	copy(edges, a.edges)
	warns := make([]string, len(a.warns))
	copy(warns, a.warns)

	dag := &ResourceDAG{
		Version:  ResourceDAGVersion,
		Nodes:    nodes,
		Edges:    edges,
		Warnings: warns,
	}
	dag.Warnings = append(dag.Warnings, DetectCycles(dag)...)
	return dag
}
