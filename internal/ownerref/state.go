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

const MaxTotalPendingPatches = 10000

type UIDMappingRecord struct {
	LiveUID   types.UID
	GroupKind schema.GroupKind
	// APIVersion is the destination version used for the registered parent.
	APIVersion string
	// ServedAPIVersions is a discovery snapshot, not a list of parent instances.
	ServedAPIVersions []string
	Name              string
	Namespace         string
	Namespaced        bool
	ScopeKnown        bool
}

type OwnerPatchRequest struct {
	Group           string
	Version         string
	Kind            string
	Namespace       string
	Name            string
	RestoredUID     types.UID
	Namespaced      bool
	ScopeKnown      bool
	OwnerReferences []metav1.OwnerReference
}

// OwnerRefRelinkState is local to one restore. Freeze prevents late controller
// or asynchronous creations from changing the parent identities used by Pass 1.
// Pass 2 refreshes parent UIDs from persisted pending references independently.
type OwnerRefRelinkState struct {
	Scope  *Scope
	mu     sync.RWMutex
	frozen bool
	uidMap map[types.UID]UIDMappingRecord
	queue  []OwnerPatchRequest
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
	if !s.frozen {
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
	if !s.frozen {
		req.OwnerReferences = CopyReferences(req.OwnerReferences)
		s.queue = append(s.queue, req)
	}
}

func (s *OwnerRefRelinkState) Freeze() (map[types.UID]UIDMappingRecord, []OwnerPatchRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	records := make(map[types.UID]UIDMappingRecord, len(s.uidMap))
	for uid, record := range s.uidMap {
		record.ServedAPIVersions = slices.Clone(record.ServedAPIVersions)
		records[uid] = record
	}
	queue := make([]OwnerPatchRequest, len(s.queue))
	for i, req := range s.queue {
		queue[i] = req
		queue[i].OwnerReferences = CopyReferences(req.OwnerReferences)
	}
	return records, queue
}
