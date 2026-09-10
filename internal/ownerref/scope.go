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
	"strings"

	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Deny list: core workloads that must NEVER receive ownerRef patching (preserving controller adoption).
var DenyListGroupKinds = []schema.GroupKind{
	{Group: "", Kind: "Pod"},
	{Group: "", Kind: "ReplicationController"},
	{Group: "apps", Kind: "Deployment"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "apps", Kind: "StatefulSet"},
	{Group: "apps", Kind: "DaemonSet"},
	{Group: "batch", Kind: "Job"},
	{Group: "batch", Kind: "CronJob"},
}

// Built-in seed groups for CAPI and KubeVirt/CDI.
var BuiltInSeedGroups = []string{
	"cluster.x-k8s.io",
	"controlplane.cluster.x-k8s.io",
	"bootstrap.cluster.x-k8s.io",
	"infrastructure.cluster.x-k8s.io",
	"ipam.cluster.x-k8s.io",
	"addons.cluster.x-k8s.io",
	"kubevirt.io",
	"cdi.kubevirt.io",
}

// DefaultConfigMapName is the conventional default ConfigMap name for owner-ref configuration.
const DefaultConfigMapName = "velero-ownerref-config"

// BuiltInSpecRefPaths returns default curated ObjectReference-like JSONPaths.
func BuiltInSpecRefPaths() []SpecRefPathEntry {
	return []SpecRefPathEntry{
		{
			Group: "cluster.x-k8s.io",
			Kind:  "Cluster",
			JSONPaths: []string{
				"spec.infrastructureRef",
				"spec.controlPlaneRef",
			},
		},
		{
			Group: "cluster.x-k8s.io",
			Kind:  "Machine",
			JSONPaths: []string{
				"spec.infrastructureRef",
				"spec.bootstrap.configRef",
			},
		},
		{
			Group: "kubevirt.io",
			Kind:  "VirtualMachine",
			JSONPaths: []string{
				"spec.template.spec.volumes[*].dataVolume",
				"spec.template.spec.volumes[*].persistentVolumeClaim",
			},
		},
	}
}

// BuiltInQuiesceRules returns default auto-quiesce rules on restore create.
func BuiltInQuiesceRules() []QuiesceRule {
	return []QuiesceRule{
		{
			Group:           "cluster.x-k8s.io",
			Kind:            "Cluster",
			AnnotationKey:   "cluster.x-k8s.io/paused",
			AnnotationValue: "",
		},
	}
}

// Scope determines which resources are eligible for ownerReference remapping,
// spec.*Ref rewriting, and automated controller quiescing.
type Scope struct {
	seedGroups   map[string]struct{}
	denyGKs      map[schema.GroupKind]struct{}
	userEntries  []ScopeEntry
	SpecRefPaths []SpecRefPathEntry
	QuiesceRules []QuiesceRule
}

// NewScope returns a Scope populated with built-in seeds and deny list.
func NewScope() *Scope {
	s := &Scope{
		seedGroups:   make(map[string]struct{}),
		denyGKs:      make(map[schema.GroupKind]struct{}),
		SpecRefPaths: BuiltInSpecRefPaths(),
		QuiesceRules: BuiltInQuiesceRules(),
	}
	for _, g := range BuiltInSeedGroups {
		s.seedGroups[g] = struct{}{}
	}
	for _, gk := range DenyListGroupKinds {
		s.denyGKs[gk] = struct{}{}
	}
	return s
}

// IsInScope checks whether a GVK is eligible for ownerReference remapping.
// 1. Deny list always wins (GroupKind check for workloads).
// 2. Built-in seed groups match.
// 3. User entries from ConfigMap match.
func (s *Scope) IsInScope(gvk schema.GroupVersionKind) bool {
	if s == nil {
		return false
	}
	// 1. Deny list always wins
	if _, denied := s.denyGKs[gvk.GroupKind()]; denied {
		return false
	}
	// 2. Built-in seed groups
	if _, ok := s.seedGroups[gvk.Group]; ok {
		return true
	}
	// 3. User entries from ConfigMap
	for _, e := range s.userEntries {
		if strings.TrimSpace(e.Group) == "" && strings.TrimSpace(e.Kind) == "" {
			continue
		}
		if e.Group == gvk.Group {
			if e.Kind == "" || e.Kind == gvk.Kind {
				return true
			}
		}
	}
	return false
}

// HasSpecRefPaths returns true if there are spec JSONPaths defined for the GVK.
func (s *Scope) HasSpecRefPaths(gvk schema.GroupVersionKind) bool {
	if s == nil {
		return false
	}
	return len(s.SpecRefPathsFor(gvk)) > 0
}

// SpecRefPathsFor returns all unique spec JSONPaths matching the given GVK.
func (s *Scope) SpecRefPathsFor(gvk schema.GroupVersionKind) []string {
	if s == nil {
		return nil
	}
	var paths []string
	seen := make(map[string]struct{})
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
		for _, p := range e.JSONPaths {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// MatchesQuiesceRule returns the matching quiesce rule for a GVK, if configured.
func (s *Scope) MatchesQuiesceRule(gvk schema.GroupVersionKind) (QuiesceRule, bool) {
	if s == nil {
		return QuiesceRule{}, false
	}
	for _, r := range s.QuiesceRules {
		if r.Group == gvk.Group && (r.Kind == "" || r.Kind == gvk.Kind) {
			return r, true
		}
	}
	return QuiesceRule{}, false
}

// LoadScopeFromConfigMap loads custom user entries additively from a ConfigMap.
func LoadScopeFromConfigMap(cm *corev1api.ConfigMap) (*Scope, error) {
	s := NewScope()
	if cm == nil || len(cm.Data) == 0 {
		return s, nil
	}

	if inScopeRaw, ok := cm.Data["inScope"]; ok && strings.TrimSpace(inScopeRaw) != "" {
		var userEntries []ScopeEntry
		if err := yaml.Unmarshal([]byte(inScopeRaw), &userEntries); err != nil {
			return nil, fmt.Errorf("unmarshaling inScope: %w", err)
		}
		for _, e := range userEntries {
			if strings.TrimSpace(e.Group) == "" && strings.TrimSpace(e.Kind) == "" {
				continue
			}
			s.userEntries = append(s.userEntries, e)
		}
	}

	if specRefRaw, ok := cm.Data["specRefPaths"]; ok && strings.TrimSpace(specRefRaw) != "" {
		var userSpecPaths []SpecRefPathEntry
		if err := yaml.Unmarshal([]byte(specRefRaw), &userSpecPaths); err != nil {
			return nil, fmt.Errorf("unmarshaling specRefPaths: %w", err)
		}
		s.SpecRefPaths = append(s.SpecRefPaths, userSpecPaths...)
	}

	if quiesceRaw, ok := cm.Data["quiesceOnRestore"]; ok && strings.TrimSpace(quiesceRaw) != "" {
		var userQuiesceRules []QuiesceRule
		if err := yaml.Unmarshal([]byte(quiesceRaw), &userQuiesceRules); err != nil {
			return nil, fmt.Errorf("unmarshaling quiesceOnRestore: %w", err)
		}
		s.QuiesceRules = append(s.QuiesceRules, userQuiesceRules...)
	}

	return s, nil
}
