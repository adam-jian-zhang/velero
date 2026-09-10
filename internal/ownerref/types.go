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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// ScopeEntry identifies a Group and optional Kind that is in-scope for ownerRef remapping.
type ScopeEntry struct {
	Group string `yaml:"group" json:"group"`
	Kind  string `yaml:"kind,omitempty" json:"kind,omitempty"`
}

// SpecRefPathEntry configures object-reference-like spec JSONPaths to remap for a GVK.
type SpecRefPathEntry struct {
	Group     string   `yaml:"group" json:"group"`
	Version   string   `yaml:"version,omitempty" json:"version,omitempty"`
	Kind      string   `yaml:"kind" json:"kind"`
	JSONPaths []string `yaml:"jsonPaths" json:"jsonPaths"`
}

// QuiesceRule configures automatic controller quiescing for a GVK on restore create.
type QuiesceRule struct {
	Group           string `yaml:"group" json:"group"`
	Kind            string `yaml:"kind" json:"kind"`
	AnnotationKey   string `yaml:"annotationKey,omitempty" json:"annotationKey,omitempty"`
	AnnotationValue string `yaml:"annotationValue,omitempty" json:"annotationValue,omitempty"`
}

// QuiescedObjectRecord records metadata about an object quiesced during restore.
type QuiescedObjectRecord struct {
	Group              string `json:"group"`
	Version            string `json:"version"`
	Kind               string `json:"kind"`
	Namespace          string `json:"namespace"`
	Name               string `json:"name"`
	AnnotationKey      string `json:"annotationKey,omitempty"`
	OriginallyQuiesced bool   `json:"originallyQuiesced"`
}

// OwnerPatchRequest holds deferred Phase 1B ownerReference patch work for one child object.
type OwnerPatchRequest struct {
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

	uidMap          map[types.UID]types.UID
	ownerPatchQueue []OwnerPatchRequest
	specPatchQueue  []OwnerPatchRequest
	specRefPaths    []SpecRefPathEntry
	quiescedObjects []QuiescedObjectRecord

	uidMapLock sync.RWMutex
	queueLock  sync.Mutex
	scope      *Scope
	newUIDs    map[types.UID]struct{}
}

// NewOwnerRefRemapState creates a new initialized OwnerRefRemapState.
func NewOwnerRefRemapState() *OwnerRefRemapState {
	return &OwnerRefRemapState{
		uidMap:  make(map[types.UID]types.UID),
		newUIDs: make(map[types.UID]struct{}),
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
