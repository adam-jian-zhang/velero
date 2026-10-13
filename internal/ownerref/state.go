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
	"slices"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// ResourceScope distinguishes discovery failure from a known resource scope.
type ResourceScope uint8

const (
	ScopeUnknown ResourceScope = iota
	ScopeNamespaced
	ScopeCluster
)

type UIDMappingRecord struct {
	LiveUID   types.UID
	GroupKind schema.GroupKind
	// APIVersion is the destination version used for the registered parent.
	APIVersion string
	// ServedAPIVersions is a discovery snapshot, not a list of parent instances.
	ServedAPIVersions []string
	Name              string
	Namespace         string
	Scope             ResourceScope
}

type OwnerPatchRequest struct {
	Group           string
	Version         string
	Kind            string
	Namespace       string
	Name            string
	RestoredUID     types.UID
	Scope           ResourceScope
	OwnerReferences []metav1.OwnerReference
}

// OwnerRefRelinkState holds registration state for one synchronous restore.
// Complete transfers it to the relinking stage and rejects further registration.
type OwnerRefRelinkState struct {
	Scope    *Scope
	mu       sync.Mutex
	complete bool
	uidMap   map[types.UID]UIDMappingRecord
	queue    []OwnerPatchRequest
}

func NewState(scope *Scope) *OwnerRefRelinkState {
	return &OwnerRefRelinkState{Scope: scope, uidMap: make(map[types.UID]UIDMappingRecord)}
}

func (s *OwnerRefRelinkState) Register(oldUID types.UID, record UIDMappingRecord) {
	if oldUID == "" || record.LiveUID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.complete {
		record.ServedAPIVersions = slices.Clone(record.ServedAPIVersions)
		s.uidMap[oldUID] = record
	}
}

func CopyReferences(refs []metav1.OwnerReference) []metav1.OwnerReference {
	if refs == nil {
		return nil
	}
	copyRefs := make([]metav1.OwnerReference, len(refs))
	for i := range refs {
		refs[i].DeepCopyInto(&copyRefs[i])
	}
	return copyRefs
}

func (s *OwnerRefRelinkState) Enqueue(req OwnerPatchRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.complete {
		req.OwnerReferences = CopyReferences(req.OwnerReferences)
		s.queue = append(s.queue, req)
	}
}

// Complete transfers the registered identities and queued children to the caller.
// It is called once after synchronous restoration; subsequent calls return no work.
func (s *OwnerRefRelinkState) Complete() (map[types.UID]UIDMappingRecord, []OwnerPatchRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.complete = true
	records, queue := s.uidMap, s.queue
	s.uidMap, s.queue = nil, nil
	return records, queue
}
