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
	"github.com/sirupsen/logrus"
)

// BuiltInSpecRefPaths returns the default CAPI (+ common) spec-ref allowlist seed.
func BuiltInSpecRefPaths() []SpecRefPathEntry {
	return []SpecRefPathEntry{
		{
			Group:   "cluster.x-k8s.io",
			Version: "v1beta1",
			Kind:    "Cluster",
			JSONPaths: []string{
				"spec.infrastructureRef",
				"spec.controlPlaneRef",
			},
		},
		{
			Group:   "cluster.x-k8s.io",
			Version: "v1beta1",
			Kind:    "Machine",
			JSONPaths: []string{
				"spec.infrastructureRef",
				"spec.bootstrap.configRef",
			},
		},
		{
			Group:   "cluster.x-k8s.io",
			Version: "v1beta2",
			Kind:    "Cluster",
			JSONPaths: []string{
				"spec.infrastructureRef",
				"spec.controlPlaneRef",
			},
		},
		{
			Group:   "cluster.x-k8s.io",
			Version: "v1beta2",
			Kind:    "Machine",
			JSONPaths: []string{
				"spec.infrastructureRef",
				"spec.bootstrap.configRef",
			},
		},
	}
}

// MergeSpecRefPaths appends valid user entries onto the built-in seed.
// Invalid entries (missing group/kind/paths) are skipped with a warning.
func MergeSpecRefPaths(base, user []SpecRefPathEntry, log logrus.FieldLogger) []SpecRefPathEntry {
	if log == nil {
		log = logrus.StandardLogger()
	}
	out := append([]SpecRefPathEntry{}, base...)
	for _, e := range user {
		if e.Group == "" || e.Kind == "" || len(e.JSONPaths) == 0 {
			log.Warnf("Skipping invalid specRefPaths entry group=%q kind=%q paths=%v", e.Group, e.Kind, e.JSONPaths)
			continue
		}
		out = append(out, e)
	}
	return out
}
