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
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

// ScopeEntry identifies a Group and optional Kind that is in-scope for ownerRef remapping.
type ScopeEntry struct {
	Group string `yaml:"group" json:"group"`
	Kind  string `yaml:"kind,omitempty" json:"kind,omitempty"`
}

// SpecRefPathEntry configures object-reference-like spec field paths to remap for a GVK.
type SpecRefPathEntry struct {
	Group   string   `yaml:"group" json:"group"`
	Version string   `yaml:"version,omitempty" json:"version,omitempty"`
	Kind    string   `yaml:"kind" json:"kind"`
	Paths   []string `yaml:"paths" json:"paths"`
}

// QuiesceRule configures automatic controller quiescing for a GVK on restore create.
// SpecFieldPath is rejected at ConfigMap load; v1 quiesce is annotation-only.
type QuiesceRule struct {
	Group           string `yaml:"group" json:"group"`
	Kind            string `yaml:"kind" json:"kind"`
	AnnotationKey   string `yaml:"annotationKey,omitempty" json:"annotationKey,omitempty"`
	AnnotationValue string `yaml:"annotationValue,omitempty" json:"annotationValue,omitempty"`
	SpecFieldPath   string `yaml:"specFieldPath,omitempty" json:"specFieldPath,omitempty"`
}

// QuiescedObjectRecord records metadata about an object quiesced during restore.
type QuiescedObjectRecord = velerov1api.QuiescedObjectRef

// OwnerPatchRequest holds deferred Phase 1B ownerReference patch work for one child object.
type OwnerPatchRequest struct {
	OldUID            types.UID               `json:"oldUID,omitempty"`
	Group             string                  `json:"group"`
	Version           string                  `json:"version"`
	Kind              string                  `json:"kind"`
	Resource          string                  `json:"resource"`
	Namespace         string                  `json:"namespace,omitempty"`
	Name              string                  `json:"name"`
	OriginalOwnerRefs []metav1.OwnerReference `json:"originalOwnerRefs"`
	// OwnerRefSourceNS is parallel to OriginalOwnerRefs; empty for cluster-scoped or unknown namespace owners.
	OwnerRefSourceNS []string `json:"ownerRefSourceNS,omitempty"`
}

// GroupVersionKind returns the GVK for this patch request.
func (r OwnerPatchRequest) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   r.Group,
		Version: r.Version,
		Kind:    r.Kind,
	}
}

// OwnerRefRemapState tracks dynamic UID mappings and queues during restore.
type OwnerRefRemapState struct {
	Enabled bool `json:"enabled"`

	uidMap           map[types.UID]types.UID
	parentMap        map[types.UID][]types.UID
	quiescedByOldUID map[types.UID]QuiescedObjectRecord
	ownerPatchQueue  []OwnerPatchRequest
	specPatchQueue   []OwnerPatchRequest
	specRefPaths     []SpecRefPathEntry
	quiescedObjects  []QuiescedObjectRecord

	uidMapLock sync.RWMutex
	queueLock  sync.Mutex
	scope      *Scope
	newUIDs    map[types.UID]struct{}
}

// NewOwnerRefRemapState creates a new initialized OwnerRefRemapState.
func NewOwnerRefRemapState() *OwnerRefRemapState {
	return &OwnerRefRemapState{
		uidMap:           make(map[types.UID]types.UID),
		parentMap:        make(map[types.UID][]types.UID),
		quiescedByOldUID: make(map[types.UID]QuiescedObjectRecord),
		newUIDs:          make(map[types.UID]struct{}),
	}
}

// RegisterUIDMapping records an oldUID -> newUID mapping in a thread-safe manner.
func (s *OwnerRefRemapState) RegisterUIDMapping(oldUID, newUID types.UID) {
	if s == nil || oldUID == "" || newUID == "" {
		return
	}
	s.uidMapLock.Lock()
	defer s.uidMapLock.Unlock()
	if s.uidMap == nil {
		s.uidMap = make(map[types.UID]types.UID)
	}
	if s.newUIDs == nil {
		s.newUIDs = make(map[types.UID]struct{})
	}
	s.uidMap[oldUID] = newUID
	s.newUIDs[newUID] = struct{}{}
}

// GetNewUID retrieves the remapped newUID for a given oldUID.
func (s *OwnerRefRemapState) GetNewUID(oldUID types.UID) (types.UID, bool) {
	if s == nil || oldUID == "" {
		return "", false
	}
	s.uidMapLock.RLock()
	defer s.uidMapLock.RUnlock()
	if s.uidMap == nil {
		return "", false
	}
	newUID, ok := s.uidMap[oldUID]
	return newUID, ok
}

// IsNewUID returns true if the given UID is known to be a remapped newUID.
func (s *OwnerRefRemapState) IsNewUID(uid types.UID) bool {
	if s == nil || uid == "" {
		return false
	}
	s.uidMapLock.RLock()
	defer s.uidMapLock.RUnlock()
	if s.newUIDs == nil {
		return false
	}
	_, ok := s.newUIDs[uid]
	return ok
}

// EnqueueOwnerPatch appends an owner patch request to the queue.
func (s *OwnerRefRemapState) EnqueueOwnerPatch(req OwnerPatchRequest) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	s.ownerPatchQueue = append(s.ownerPatchQueue, req)
}

// EnqueueSpecPatch appends a spec patch request to the queue.
func (s *OwnerRefRemapState) EnqueueSpecPatch(req OwnerPatchRequest) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	s.specPatchQueue = append(s.specPatchQueue, req)
}

// RecordQuiescedObject tracks an object that was quiesced during Phase 1A.
func (s *OwnerRefRemapState) RecordQuiescedObject(rec QuiescedObjectRecord) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	s.quiescedObjects = append(s.quiescedObjects, rec)
}

// RecordQuiescedObjectWithUID records a quiesced object and associates it with its backup oldUID.
func (s *OwnerRefRemapState) RecordQuiescedObjectWithUID(oldUID types.UID, rec QuiescedObjectRecord) {
	if s == nil {
		return
	}
	s.RecordQuiescedObject(rec)
	if oldUID != "" {
		s.uidMapLock.Lock()
		defer s.uidMapLock.Unlock()
		if s.quiescedByOldUID == nil {
			s.quiescedByOldUID = make(map[types.UID]QuiescedObjectRecord)
		}
		s.quiescedByOldUID[oldUID] = rec
	}
}

// RegisterParentUID records a childOldUID -> parentOldUID relationship for transitive dependency tracking.
func (s *OwnerRefRemapState) RegisterParentUID(childOldUID, parentOldUID types.UID) {
	if s == nil || childOldUID == "" || parentOldUID == "" {
		return
	}
	s.uidMapLock.Lock()
	defer s.uidMapLock.Unlock()
	if s.parentMap == nil {
		s.parentMap = make(map[types.UID][]types.UID)
	}
	for _, existing := range s.parentMap[childOldUID] {
		if existing == parentOldUID {
			return
		}
	}
	s.parentMap[childOldUID] = append(s.parentMap[childOldUID], parentOldUID)
}

// ResolveQuiescedRoots traverses parent relationships starting from req up to any quiesced root objects.
// It uses BFS with cycle detection to trace ancestors through parentMap and returns deduplicated TargetRefs.
func (s *OwnerRefRemapState) ResolveQuiescedRoots(req OwnerPatchRequest) []velerov1api.TargetRef {
	if s == nil {
		return nil
	}
	s.uidMapLock.RLock()
	defer s.uidMapLock.RUnlock()

	if len(s.quiescedByOldUID) == 0 {
		return nil
	}

	var queue []types.UID
	visited := make(map[types.UID]struct{})

	// Seed queue with immediate parent UIDs from OriginalOwnerRefs, as well as the object's own oldUID.
	for _, ref := range req.OriginalOwnerRefs {
		if ref.UID != "" {
			queue = append(queue, ref.UID)
		}
	}
	if req.OldUID != "" {
		queue = append(queue, s.parentMap[req.OldUID]...)
	}

	var targets []velerov1api.TargetRef
	seenTargets := make(map[string]struct{})

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		if _, ok := visited[curr]; ok {
			continue
		}
		visited[curr] = struct{}{}

		// Check if this ancestor UID corresponds to a quiesced object
		if rec, ok := s.quiescedByOldUID[curr]; ok {
			targetNS := rec.Namespace
			if targetNS == "" {
				targetNS = req.Namespace
			}
			key := fmt.Sprintf("%s/%s/%s/%s", rec.Group, rec.Kind, targetNS, rec.Name)
			if _, exists := seenTargets[key]; !exists {
				seenTargets[key] = struct{}{}
				targets = append(targets, velerov1api.TargetRef{
					Group:     rec.Group,
					Kind:      rec.Kind,
					Namespace: targetNS,
					Name:      rec.Name,
				})
			}
		}

		// Continue traversing upwards to parents of curr
		for _, parentUID := range s.parentMap[curr] {
			if _, ok := visited[parentUID]; !ok {
				queue = append(queue, parentUID)
			}
		}
	}

	return targets
}

// GetOwnerPatchQueue returns a copy of the owner patch queue in a thread-safe manner.
func (s *OwnerRefRemapState) GetOwnerPatchQueue() []OwnerPatchRequest {
	if s == nil {
		return nil
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	return append([]OwnerPatchRequest(nil), s.ownerPatchQueue...)
}

// SetOwnerPatchQueue replaces the owner patch queue in a thread-safe manner.
func (s *OwnerRefRemapState) SetOwnerPatchQueue(q []OwnerPatchRequest) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	s.ownerPatchQueue = append([]OwnerPatchRequest(nil), q...)
}

// GetSpecPatchQueue returns a copy of the spec patch queue in a thread-safe manner.
func (s *OwnerRefRemapState) GetSpecPatchQueue() []OwnerPatchRequest {
	if s == nil {
		return nil
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	return append([]OwnerPatchRequest(nil), s.specPatchQueue...)
}

// SetSpecPatchQueue replaces the spec patch queue in a thread-safe manner.
func (s *OwnerRefRemapState) SetSpecPatchQueue(q []OwnerPatchRequest) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	s.specPatchQueue = append([]OwnerPatchRequest(nil), q...)
}

// GetQuiescedObjects returns a copy of the quiesced objects list in a thread-safe manner.
func (s *OwnerRefRemapState) GetQuiescedObjects() []QuiescedObjectRecord {
	if s == nil {
		return nil
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	return append([]QuiescedObjectRecord(nil), s.quiescedObjects...)
}

// BlockUnquiesceForTarget marks any quiesced object matching the target as unquiesceBlocked.
func (s *OwnerRefRemapState) BlockUnquiesceForTarget(group, kind, namespace, name string) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	for i := range s.quiescedObjects {
		q := &s.quiescedObjects[i]
		if (group == "" || q.Group == group) && q.Kind == kind && q.Name == name {
			if q.Namespace == "" || namespace == "" || q.Namespace == namespace {
				q.UnquiesceBlocked = true
			}
		}
	}
}

// BlockUnquiesceForNamespace marks all quiesced objects in the given namespace as unquiesceBlocked.
func (s *OwnerRefRemapState) BlockUnquiesceForNamespace(namespace string) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	for i := range s.quiescedObjects {
		q := &s.quiescedObjects[i]
		if namespace == "" || q.Namespace == namespace {
			q.UnquiesceBlocked = true
		}
	}
}

// GetSpecRefPaths returns a copy of the specRefPaths in a thread-safe manner.
func (s *OwnerRefRemapState) GetSpecRefPaths() []SpecRefPathEntry {
	if s == nil {
		return nil
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	return append([]SpecRefPathEntry(nil), s.specRefPaths...)
}

// SetScope sets the scope and syncs specRefPaths in a thread-safe manner.
func (s *OwnerRefRemapState) SetScope(scope *Scope) {
	if s == nil {
		return
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	s.scope = scope
	if scope != nil {
		s.specRefPaths = append([]SpecRefPathEntry(nil), scope.SpecRefPaths...)
	}
}

// GetScope returns the current scope in a thread-safe manner.
func (s *OwnerRefRemapState) GetScope() *Scope {
	if s == nil {
		return nil
	}
	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	return s.scope
}
