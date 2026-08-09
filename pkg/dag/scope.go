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
	"fmt"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// ConfigMapInScopeKey is the ConfigMap data key for additive ownerRef in-scope entries.
	ConfigMapInScopeKey = "inScope"
	// ConfigMapSpecRefPathsKey is the ConfigMap data key for curated spec-ref JSONPaths.
	ConfigMapSpecRefPathsKey = "specRefPaths"
)

// ScopeEntry is a ConfigMap/inScope allowlist entry.
// Matching is by group (required); version and kind are optional filters.
type ScopeEntry struct {
	Group   string `json:"group" yaml:"group"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	Kind    string `json:"kind,omitempty" yaml:"kind,omitempty"`
}

// SpecRefPathEntry maps a GVK to JSONPaths of ObjectReference-like fields.
type SpecRefPathEntry struct {
	Group     string   `json:"group" yaml:"group"`
	Version   string   `json:"version,omitempty" yaml:"version,omitempty"`
	Kind      string   `json:"kind" yaml:"kind"`
	JSONPaths []string `json:"jsonPaths" yaml:"jsonPaths"`
}

// Scope resolves whether a GVK is in-scope for ownerRef remapping and holds specRefPaths.
type Scope struct {
	// seedGroups are built-in API groups that are in-scope unless denied.
	seedGroups map[string]struct{}
	// userEntries are additive ConfigMap inScope entries.
	userEntries []ScopeEntry
	// denyKinds are core/apps/batch workload kinds that can never be opted in.
	denyKinds map[string]struct{}
	// SpecRefPaths is the merged built-in + user allowlist for spec-level remapping.
	SpecRefPaths []SpecRefPathEntry
}

// builtInSeedGroups is the CAPI + KubeVirt/CDI seed from design §4.2.1.
var builtInSeedGroups = []string{
	"cluster.x-k8s.io",
	"controlplane.cluster.x-k8s.io",
	"bootstrap.cluster.x-k8s.io",
	"infrastructure.cluster.x-k8s.io",
	"ipam.cluster.x-k8s.io",
	"addons.cluster.x-k8s.io",
	"kubevirt.io",
	"cdi.kubevirt.io",
}

// denyListKinds are workload kinds that always stay on legacy strip behavior.
var denyListKinds = []string{
	"Pod",
	"ReplicaSet",
	"Deployment",
	"StatefulSet",
	"DaemonSet",
	"Job",
	"CronJob",
	"ReplicationController",
}

// NewScope returns a Scope with built-in seeds only.
func NewScope() *Scope {
	s := &Scope{
		seedGroups:   make(map[string]struct{}, len(builtInSeedGroups)),
		denyKinds:    make(map[string]struct{}, len(denyListKinds)),
		SpecRefPaths: BuiltInSpecRefPaths(),
	}
	for _, g := range builtInSeedGroups {
		s.seedGroups[g] = struct{}{}
	}
	for _, k := range denyListKinds {
		s.denyKinds[k] = struct{}{}
	}
	return s
}

// LoadFromConfigMap merges inScope and specRefPaths from a ConfigMap.
// Missing keys or invalid entries are skipped with warnings; never fails hard.
func (s *Scope) LoadFromConfigMap(cm *corev1api.ConfigMap, log logrus.FieldLogger) {
	if s == nil || cm == nil {
		return
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	if raw, ok := cm.Data[ConfigMapInScopeKey]; ok && raw != "" {
		var entries []ScopeEntry
		if err := yaml.Unmarshal([]byte(raw), &entries); err != nil {
			log.WithError(err).Warn("Failed to parse owner-ref ConfigMap inScope; using built-in seed only for inScope")
		} else {
			for _, e := range entries {
				if e.Group == "" {
					log.Warn("Skipping owner-ref inScope entry with empty group")
					continue
				}
				if e.Kind != "" {
					if _, denied := s.denyKinds[e.Kind]; denied {
						log.Warnf("Skipping owner-ref inScope entry for denied kind %s", e.Kind)
						continue
					}
				}
				s.userEntries = append(s.userEntries, e)
			}
		}
	}

	if raw, ok := cm.Data[ConfigMapSpecRefPathsKey]; ok && raw != "" {
		var entries []SpecRefPathEntry
		if err := yaml.Unmarshal([]byte(raw), &entries); err != nil {
			log.WithError(err).Warn("Failed to parse owner-ref ConfigMap specRefPaths; using built-in path seed only")
		} else {
			s.SpecRefPaths = MergeSpecRefPaths(s.SpecRefPaths, entries, log)
		}
	}
}

// IsInScope reports whether gvk should receive ownerRef patching.
// Deny list always wins; then built-in seed groups; then user ConfigMap entries.
func (s *Scope) IsInScope(gvk schema.GroupVersionKind) bool {
	if s == nil {
		return false
	}
	if _, denied := s.denyKinds[gvk.Kind]; denied {
		return false
	}
	if _, ok := s.seedGroups[gvk.Group]; ok {
		return true
	}
	for _, e := range s.userEntries {
		if e.Group != gvk.Group {
			continue
		}
		if e.Version != "" && e.Version != gvk.Version {
			continue
		}
		if e.Kind != "" && e.Kind != gvk.Kind {
			continue
		}
		return true
	}
	return false
}

// SpecRefPathsFor returns JSONPaths configured for the given GVK.
func (s *Scope) SpecRefPathsFor(gvk schema.GroupVersionKind) []string {
	if s == nil {
		return nil
	}
	var paths []string
	for _, e := range s.SpecRefPaths {
		if e.Group != gvk.Group {
			continue
		}
		if e.Version != "" && e.Version != gvk.Version {
			continue
		}
		if e.Kind != gvk.Kind {
			continue
		}
		paths = append(paths, e.JSONPaths...)
	}
	return paths
}

// ParseScopeEntries is a test helper for YAML inScope parsing.
func ParseScopeEntries(raw string) ([]ScopeEntry, error) {
	var entries []ScopeEntry
	if err := yaml.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parse inScope: %w", err)
	}
	return entries, nil
}
