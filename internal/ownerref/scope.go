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

// Deny list: leaf & intermediate workloads that must NEVER receive ownerRef patching (preserving controller adoption).
// Top-level workloads (Deployment, StatefulSet, DaemonSet, CronJob) are excluded so that operator-managed
// workloads can have ownerReferences remapped via ConfigMap inScope opt-in.
var defaultDenyListGroupKinds = []schema.GroupKind{
	{Group: "", Kind: "Pod"},
	{Group: "", Kind: "ReplicationController"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "batch", Kind: "Job"},
}

// DenyListGroupKinds returns a defensive copy of the built-in deny list group kinds.
func DenyListGroupKinds() []schema.GroupKind {
	return append([]schema.GroupKind(nil), defaultDenyListGroupKinds...)
}

// ConfigMap keys and defaults for owner-ref configuration.
const (
	// DefaultConfigMapName is the conventional default ConfigMap name for owner-ref configuration.
	DefaultConfigMapName = "velero-ownerref-config"

	// ConfigMapKeyInScope is the ConfigMap data key for additive in-scope GVK entries.
	ConfigMapKeyInScope = "inScope"

	// ConfigMapKeySpecRefPaths is the ConfigMap data key for spec-level pointer remapping paths.
	ConfigMapKeySpecRefPaths = "specRefPaths"

	// ConfigMapKeyQuiesceOnRestore is the ConfigMap data key for controller pause/quiesce rules.
	ConfigMapKeyQuiesceOnRestore = "quiesceOnRestore"
)

// Scope determines which resources are eligible for ownerReference remapping,
// spec.*Ref rewriting, and automated controller quiescing.
type Scope struct {
	denyGKs      map[schema.GroupKind]struct{}
	userEntries  []ScopeEntry
	SpecRefPaths []SpecRefPathEntry
	QuiesceRules []QuiesceRule
}

// NewScope returns a clean Scope populated only with the built-in deny list.
func NewScope() *Scope {
	s := &Scope{
		denyGKs:      make(map[schema.GroupKind]struct{}),
		userEntries:  []ScopeEntry{},
		SpecRefPaths: []SpecRefPathEntry{},
		QuiesceRules: []QuiesceRule{},
	}
	for _, gk := range defaultDenyListGroupKinds {
		s.denyGKs[gk] = struct{}{}
	}
	return s
}

// IsInScope checks whether a GVK is eligible for ownerReference remapping.
// 1. Deny list always wins (GroupKind check for workloads).
// 2. Entries from ConfigMap (userEntries) match.
func (s *Scope) IsInScope(gvk schema.GroupVersionKind) bool {
	if s == nil {
		return false
	}
	// 1. Deny list always wins
	if _, denied := s.denyGKs[gvk.GroupKind()]; denied {
		return false
	}
	// 2. User entries from ConfigMap
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

// IsDeniedOwner reports whether an ownerReference parent GVK is on the built-in deny list.
func (s *Scope) IsDeniedOwner(apiVersion, kind string) bool {
	if s == nil {
		return false
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	group := ""
	if err == nil {
		group = gv.Group
	}
	_, denied := s.denyGKs[schema.GroupKind{Group: group, Kind: kind}]
	return denied
}

// HasSpecRefPaths returns true if there are spec field paths defined for the GVK.
func (s *Scope) HasSpecRefPaths(gvk schema.GroupVersionKind) bool {
	if s == nil {
		return false
	}
	return len(s.SpecRefPathsFor(gvk)) > 0
}

// SpecRefPathsFor returns all unique spec field paths matching the given GVK.
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
		for _, p := range e.Paths {
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

// MergeConfigMap parses user entries additively from a ConfigMap into the receiver Scope.
// - inScope: entries are unioned, rejecting any entry forbidden by the deny list.
// - specRefPaths: entries are appended, validating dotted paths.
// - quiesceOnRestore: entries are merged; if a rule for the same (Group, Kind) already exists, it is replaced.
func (s *Scope) MergeConfigMap(cm *corev1api.ConfigMap) error {
	if s == nil || cm == nil || len(cm.Data) == 0 {
		return nil
	}

	if inScopeRaw, ok := cm.Data[ConfigMapKeyInScope]; ok && strings.TrimSpace(inScopeRaw) != "" {
		var userEntries []ScopeEntry
		if err := yaml.Unmarshal([]byte(inScopeRaw), &userEntries); err != nil {
			return fmt.Errorf("unmarshaling %s: %w", ConfigMapKeyInScope, err)
		}
		for _, e := range userEntries {
			group := strings.TrimSpace(e.Group)
			kind := strings.TrimSpace(e.Kind)
			if group == "" && kind == "" {
				return fmt.Errorf("%s entry has empty group and kind", ConfigMapKeyInScope)
			}
			gk := schema.GroupKind{Group: group, Kind: kind}
			if _, denied := s.denyGKs[gk]; denied {
				return fmt.Errorf("inScope entry %s/%s is a core leaf workload and is forbidden by the built-in deny list", e.Group, e.Kind)
			}
			s.addScopeEntry(ScopeEntry{Group: group, Kind: kind})
		}
	}

	if specRefRaw, ok := cm.Data[ConfigMapKeySpecRefPaths]; ok && strings.TrimSpace(specRefRaw) != "" {
		var userSpecPaths []SpecRefPathEntry
		if err := yaml.Unmarshal([]byte(specRefRaw), &userSpecPaths); err != nil {
			return fmt.Errorf("unmarshaling %s: %w", ConfigMapKeySpecRefPaths, err)
		}
		for i := range userSpecPaths {
			e := &userSpecPaths[i]
			e.Group = strings.TrimSpace(e.Group)
			e.Version = strings.TrimSpace(e.Version)
			e.Kind = strings.TrimSpace(e.Kind)
			if e.Kind == "" {
				return fmt.Errorf("%s entry requires kind", ConfigMapKeySpecRefPaths)
			}
			if len(e.Paths) == 0 {
				return fmt.Errorf("%s entry %s/%s requires at least one path", ConfigMapKeySpecRefPaths, e.Group, e.Kind)
			}
			for j, p := range e.Paths {
				trimmedPath := strings.TrimSpace(p)
				if err := validateDottedSpecPath(trimmedPath); err != nil {
					return fmt.Errorf("%s path %q: %w", ConfigMapKeySpecRefPaths, p, err)
				}
				e.Paths[j] = trimmedPath
			}
		}
		s.SpecRefPaths = append(s.SpecRefPaths, userSpecPaths...)
	}

	if quiesceRaw, ok := cm.Data[ConfigMapKeyQuiesceOnRestore]; ok && strings.TrimSpace(quiesceRaw) != "" {
		var userQuiesceRules []QuiesceRule
		if err := yaml.Unmarshal([]byte(quiesceRaw), &userQuiesceRules); err != nil {
			return fmt.Errorf("unmarshaling %s: %w", ConfigMapKeyQuiesceOnRestore, err)
		}
		for _, r := range userQuiesceRules {
			r.Group = strings.TrimSpace(r.Group)
			r.Kind = strings.TrimSpace(r.Kind)
			r.AnnotationKey = strings.TrimSpace(r.AnnotationKey)
			r.AnnotationValue = strings.TrimSpace(r.AnnotationValue)
			if strings.TrimSpace(r.SpecFieldPath) != "" {
				return fmt.Errorf("%s specFieldPath is not supported; v1 quiesce is annotation-only", ConfigMapKeyQuiesceOnRestore)
			}
			if r.Kind == "" {
				return fmt.Errorf("%s entry requires kind", ConfigMapKeyQuiesceOnRestore)
			}
			if r.AnnotationKey == "" {
				return fmt.Errorf("%s entry %s/%s requires annotationKey", ConfigMapKeyQuiesceOnRestore, r.Group, r.Kind)
			}
			if strings.ContainsAny(r.AnnotationKey, " \t") {
				return fmt.Errorf("%s annotationKey %q is not a valid Kubernetes annotation key", ConfigMapKeyQuiesceOnRestore, r.AnnotationKey)
			}
			s.addOrReplaceQuiesceRule(r)
		}
	}

	return nil
}

func (s *Scope) addScopeEntry(e ScopeEntry) {
	for _, existing := range s.userEntries {
		if existing.Group == e.Group && existing.Kind == e.Kind {
			return
		}
	}
	s.userEntries = append(s.userEntries, e)
}

func (s *Scope) addOrReplaceQuiesceRule(newRule QuiesceRule) {
	for i, existing := range s.QuiesceRules {
		if existing.Group == newRule.Group && existing.Kind == newRule.Kind {
			s.QuiesceRules[i] = newRule
			return
		}
	}
	s.QuiesceRules = append(s.QuiesceRules, newRule)
}

// LoadScopeFromConfigMap loads custom user entries from a ConfigMap into a new Scope.
func LoadScopeFromConfigMap(cm *corev1api.ConfigMap) (*Scope, error) {
	s := NewScope()
	if err := s.MergeConfigMap(cm); err != nil {
		return nil, err
	}
	return s, nil
}

func validateDottedSpecPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "$") || strings.Contains(path, "..") {
		return fmt.Errorf("JSONPath syntax is not supported; use dotted spec.* paths with optional [*]")
	}
	if !strings.HasPrefix(path, "spec.") {
		return fmt.Errorf("must start with 'spec.' (e.g. 'spec.infrastructureRef')")
	}
	normalized := strings.ReplaceAll(path, "[*]", ".[*].")
	for _, seg := range strings.Split(normalized, ".") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if (strings.Contains(seg, "[") || strings.Contains(seg, "]")) && seg != "[*]" {
			return fmt.Errorf("wildcard array segments must be exactly [*]")
		}
	}
	return nil
}
