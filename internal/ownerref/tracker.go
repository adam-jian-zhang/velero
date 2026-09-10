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
)

// OwnerRefRemapTracker provides thread-safe in-memory sharing of owner-ref remap state
// between RestoreController and RestoreFinalizerController.
type OwnerRefRemapTracker struct {
	lock     sync.RWMutex
	trackers map[string]*OwnerRefRemapState
}

// NewOwnerRefRemapTracker creates a new OwnerRefRemapTracker.
func NewOwnerRefRemapTracker() *OwnerRefRemapTracker {
	return &OwnerRefRemapTracker{
		trackers: make(map[string]*OwnerRefRemapState),
	}
}

// Set stores the remap state for a restore.
func (t *OwnerRefRemapTracker) Set(restoreName string, state *OwnerRefRemapState) {
	if t == nil {
		return
	}
	t.lock.Lock()
	defer t.lock.Unlock()
	t.trackers[restoreName] = state
}

// Get retrieves the remap state for a restore.
func (t *OwnerRefRemapTracker) Get(restoreName string) *OwnerRefRemapState {
	if t == nil {
		return nil
	}
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.trackers[restoreName]
}

// Delete removes the remap state for a restore.
func (t *OwnerRefRemapTracker) Delete(restoreName string) {
	if t == nil {
		return
	}
	t.lock.Lock()
	defer t.lock.Unlock()
	delete(t.trackers, restoreName)
}
