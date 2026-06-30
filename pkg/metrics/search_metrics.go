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

// RegisterSearchMetrics is a placeholder to register search metrics
func (m *ServerMetrics) RegisterSearchMetrics() {
	// m.metrics[searchIndexedBackups] = ...
	// Since we are mocking/stubbing this, we will just add it if required.
}

func (m *ServerMetrics) ObserveSearchIndex(ok bool, d float64) {
	// Not implemented for brevity
}

func (m *ServerMetrics) ObserveSearchRequest(ok bool, d float64) {
	// Not implemented for brevity
}
