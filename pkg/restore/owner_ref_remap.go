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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vmware-tanzu/velero/pkg/dag"
)

// OwnerPatchRequest holds deferred Phase-1B ownerReference patch work for one child.
type OwnerPatchRequest struct {
	Group             string                  `json:"group"`
	Version           string                  `json:"version"`
	Kind              string                  `json:"kind"`
	Resource          string                  `json:"resource"`
	Namespace         string                  `json:"namespace,omitempty"`
	Name              string                  `json:"name"`
	OriginalOwnerRefs []metav1.OwnerReference `json:"originalOwnerRefs"`
	// OwnerRefSourceNS is parallel to OriginalOwnerRefs; empty for cluster-scoped owners.
	OwnerRefSourceNS []string `json:"ownerRefSourceNS,omitempty"`
}

// OwnerRefRemapState is collected during Phase 1A and persisted for Finalizing Phase 1B.
type OwnerRefRemapState struct {
	Enabled            bool                    `json:"enabled"`
	ResourceDAGPresent bool                    `json:"resourceDAGPresent"`
	UIDMap             map[types.UID]types.UID `json:"uidMap,omitempty"`
	OwnerPatchQueue    []OwnerPatchRequest     `json:"ownerPatchQueue,omitempty"`
	SpecRefPaths       []dag.SpecRefPathEntry  `json:"specRefPaths,omitempty"`
	ResourceDAG        *dag.ResourceDAG        `json:"resourceDAG,omitempty"`

	uidMapLock        sync.RWMutex `json:"-"`
	ownerPatchQueueMu sync.Mutex   `json:"-"`
	scope             *dag.Scope   `json:"-"`
}

// NewOwnerRefRemapState returns an empty remap state.
func NewOwnerRefRemapState() *OwnerRefRemapState {
	return &OwnerRefRemapState{
		UIDMap: make(map[types.UID]types.UID),
	}
}

func (s *OwnerRefRemapState) registerUIDMapping(oldUID, newUID types.UID) {
	if s == nil || !s.Enabled || !s.ResourceDAGPresent || oldUID == "" || newUID == "" {
		return
	}
	s.uidMapLock.Lock()
	defer s.uidMapLock.Unlock()
	s.UIDMap[oldUID] = newUID
}

func (s *OwnerRefRemapState) enqueueOwnerPatch(
	itemFromBackup *unstructured.Unstructured,
	liveObj *unstructured.Unstructured,
	groupResource schema.GroupResource,
	originalOwnerRefs []metav1.OwnerReference,
	ownerRefSourceNS []string,
) {
	if s == nil || !s.Enabled || !s.ResourceDAGPresent || s.scope == nil {
		return
	}
	if len(originalOwnerRefs) == 0 {
		return
	}
	if !s.scope.IsInScope(itemFromBackup.GroupVersionKind()) {
		return
	}

	gvk := itemFromBackup.GroupVersionKind()
	req := OwnerPatchRequest{
		Group:             gvk.Group,
		Version:           gvk.Version,
		Kind:              gvk.Kind,
		Resource:          groupResource.Resource,
		Namespace:         liveObj.GetNamespace(),
		Name:              liveObj.GetName(),
		OriginalOwnerRefs: append([]metav1.OwnerReference(nil), originalOwnerRefs...),
		OwnerRefSourceNS:  append([]string(nil), ownerRefSourceNS...),
	}

	s.ownerPatchQueueMu.Lock()
	defer s.ownerPatchQueueMu.Unlock()
	s.OwnerPatchQueue = append(s.OwnerPatchQueue, req)
}

func (s *OwnerRefRemapState) registerAndMaybeEnqueue(
	itemFromBackup *unstructured.Unstructured,
	liveObj *unstructured.Unstructured,
	groupResource schema.GroupResource,
	originalOwnerRefs []metav1.OwnerReference,
	ownerRefSourceNS []string,
) {
	if s == nil || itemFromBackup == nil || liveObj == nil {
		return
	}
	s.registerUIDMapping(itemFromBackup.GetUID(), liveObj.GetUID())
	s.enqueueOwnerPatch(itemFromBackup, liveObj, groupResource, originalOwnerRefs, ownerRefSourceNS)
}

func ownerRefSourceNamespaces(refs []metav1.OwnerReference, resourceDAG *dag.ResourceDAG) []string {
	out := make([]string, len(refs))
	for i, ref := range refs {
		if resourceDAG != nil {
			if node, ok := resourceDAG.Nodes[ref.UID]; ok {
				out[i] = node.Namespace
				continue
			}
		}
		// Parent not in the DAG: leave empty so we do not assume a namespace
		// (cluster-scoped owners must not inherit the child's namespace for split checks).
		out[i] = ""
	}
	return out
}

func copyOwnerReferences(refs []metav1.OwnerReference) []metav1.OwnerReference {
	if len(refs) == 0 {
		return nil
	}
	out := make([]metav1.OwnerReference, len(refs))
	copy(out, refs)
	return out
}

// SetScope attaches the in-scope matcher used during Phase 1A enqueue.
func (s *OwnerRefRemapState) SetScope(scope *dag.Scope) {
	if s == nil {
		return
	}
	s.scope = scope
	if scope != nil {
		s.SpecRefPaths = scope.SpecRefPaths
	}
}

// SetResourceDAG attaches the parsed backup DAG for namespace lookups / spec-ref walks.
func (s *OwnerRefRemapState) SetResourceDAG(resourceDAG *dag.ResourceDAG) {
	if s == nil {
		return
	}
	s.ResourceDAG = resourceDAG
}
