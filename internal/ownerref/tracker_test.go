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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
)

func TestOwnerRefRemapTrackerConcurrent(t *testing.T) {
	tracker := NewOwnerRefRemapTracker()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			restoreName := "restore"
			state := NewOwnerRefRemapState()
			state.RegisterUIDMapping(types.UID("old"), types.UID("new"))
			tracker.Set(restoreName, state)
			got := tracker.Get(restoreName)
			if got != nil {
				_, _ = got.GetNewUID(types.UID("old"))
			}
		}(i)
	}
	wg.Wait()

	assert.NotNil(t, tracker.Get("restore"))
	tracker.Delete("restore")
	assert.Nil(t, tracker.Get("restore"))
}
