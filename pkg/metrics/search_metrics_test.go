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

package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSearchMetrics(t *testing.T) {
	// The real ServerMetrics depends on other types in the package.
	// For now we just test that the functions don't panic.
	sm := &ServerMetrics{}
	sm.RegisterSearchMetrics()
	sm.ObserveSearchIndex(true, 1.0)
	sm.ObserveSearchRequest(true, 1.0)

	assert.NotNil(t, sm)
}
