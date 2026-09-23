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

package restore

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultBaselineConfigMap is the conventional fallback name when --owner-ref-configmap is not specified.
	DefaultBaselineConfigMap = "velero-ownerref-config"

	// InScopeConfigKey is the ConfigMap data key containing inScope GVK rules.
	InScopeConfigKey = "inScope"

	// QuiesceOnRestoreConfigKey is the ConfigMap data key containing quiesceOnRestore rules.
	QuiesceOnRestoreConfigKey = "quiesceOnRestore"
)

// DenyListGroupKinds lists core leaf/intermediate workloads that must NEVER receive
// ownerReference relinking to preserve standard controller adoption semantics.
var DenyListGroupKinds = []schema.GroupKind{
	{Group: "", Kind: "Pod"},
	{Group: "", Kind: "ReplicationController"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "batch", Kind: "Job"},
}

// IsDeniedGroupKind checks whether a given GroupKind is on the immutable safety deny list.
func IsDeniedGroupKind(gk schema.GroupKind) bool {
	for _, denied := range DenyListGroupKinds {
		if denied.Group == gk.Group && denied.Kind == gk.Kind {
			return true
		}
	}
	return false
}

// ValidateScopeConfig validates a deserialized ScopeConfig.
// It fails if an inScope entry has both Group and Kind empty, if an inScope entry
// targets a denied core leaf workload, or if a quiesceOnRestore rule lacks a kind or annotation.
func ValidateScopeConfig(cfg *ScopeConfig) error {
	if cfg == nil {
		return nil
	}
	for _, entry := range cfg.InScope {
		if entry.Group == "" && entry.Kind == "" {
			return fmt.Errorf("inScope entry must specify at least group or kind")
		}
		for _, denied := range DenyListGroupKinds {
			if (entry.Group == "" || entry.Group == denied.Group) && entry.Kind == denied.Kind {
				return fmt.Errorf("inScope entry %s/%s is a core leaf workload and is forbidden by the built-in deny list", entry.Group, entry.Kind)
			}
		}
	}
	for _, rule := range cfg.QuiesceOnRestore {
		if rule.Kind == "" {
			return fmt.Errorf("quiesceOnRestore rule must specify a kind")
		}
		if rule.AnnotationKey == "" {
			return fmt.Errorf("quiesceOnRestore rule for %s/%s must specify an annotationKey", rule.Group, rule.Kind)
		}
	}
	return nil
}

// FilterDeniedOwnerRefs returns a copy of refs with any ownerReference pointing to a
// parent GroupKind on the safety deny list removed.
func FilterDeniedOwnerRefs(refs []metav1.OwnerReference) []metav1.OwnerReference {
	if len(refs) == 0 {
		return nil
	}
	filtered := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			continue
		}
		if IsDeniedGroupKind(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}) {
			continue
		}
		filtered = append(filtered, ref)
	}
	return filtered
}

// ScopeConfig defines the deserialized structure of an ownerReference configuration ConfigMap.
type ScopeConfig struct {
	InScope          []InScopeEntry `yaml:"inScope"`
	QuiesceOnRestore []QuiesceRule  `yaml:"quiesceOnRestore"`
}

// InScopeEntry defines a resource pattern eligible for ownerReference relinking.
type InScopeEntry struct {
	Group   string `yaml:"group"`
	Version string `yaml:"version,omitempty"`
	Kind    string `yaml:"kind,omitempty"`
}

// QuiesceRule defines automated controller quiescing annotations injected during Phase 1A.
type QuiesceRule struct {
	Group           string `yaml:"group"`
	Version         string `yaml:"version,omitempty"`
	Kind            string `yaml:"kind"`
	AnnotationKey   string `yaml:"annotationKey"`
	AnnotationValue string `yaml:"annotationValue,omitempty"`
}

// IsInScope checks whether the provided GroupVersionKind matches any inScope entry.
func (c *ScopeConfig) IsInScope(gvk schema.GroupVersionKind) bool {
	if c == nil {
		return false
	}
	// 1. Immutable safety deny list always wins
	if IsDeniedGroupKind(gvk.GroupKind()) {
		return false
	}
	for _, entry := range c.InScope {
		if entry.Group != "" && entry.Group != gvk.Group {
			continue
		}
		if entry.Version != "" && entry.Version != gvk.Version {
			continue
		}
		if entry.Kind != "" && entry.Kind != gvk.Kind {
			continue
		}
		return true
	}
	return false
}

// ShouldQuiesce returns the matching quiesce rule if the resource should be paused on restore.
func (c *ScopeConfig) ShouldQuiesce(gvk schema.GroupVersionKind) *QuiesceRule {
	if c == nil {
		return nil
	}
	for _, rule := range c.QuiesceOnRestore {
		if rule.Group != gvk.Group {
			continue
		}
		if rule.Version != "" && rule.Version != gvk.Version {
			continue
		}
		if rule.Kind == gvk.Kind {
			r := rule
			return &r
		}
	}
	return nil
}

// MergeScopeConfig additively combines Tier 1 (Baseline) and Tier 2 (Restore Delta).
// 1. inScope entries are unioned and deduplicated by (Group, Version, Kind).
// 2. quiesceOnRestore rules in delta override rules in baseline for the same (Group, Kind).
func MergeScopeConfig(baseline, delta *ScopeConfig) *ScopeConfig {
	if baseline == nil && delta == nil {
		return nil
	}
	if baseline == nil {
		return delta
	}
	if delta == nil {
		return baseline
	}

	merged := &ScopeConfig{
		InScope:          make([]InScopeEntry, 0, len(baseline.InScope)+len(delta.InScope)),
		QuiesceOnRestore: make([]QuiesceRule, 0, len(baseline.QuiesceOnRestore)+len(delta.QuiesceOnRestore)),
	}

	// 1. Union inScope entries
	seenInScope := make(map[string]bool)
	addInScope := func(entry InScopeEntry) {
		key := fmt.Sprintf("%s/%s/%s", entry.Group, entry.Version, entry.Kind)
		if !seenInScope[key] {
			seenInScope[key] = true
			merged.InScope = append(merged.InScope, entry)
		}
	}
	for _, entry := range baseline.InScope {
		addInScope(entry)
	}
	for _, entry := range delta.InScope {
		addInScope(entry)
	}

	// 2. Merge quiesce rules with delta precedence while preserving deterministic order
	var orderedKeys []string
	quiesceMap := make(map[string]QuiesceRule)
	addQuiesceRule := func(rule QuiesceRule) {
		key := fmt.Sprintf("%s/%s", rule.Group, rule.Kind)
		if _, exists := quiesceMap[key]; !exists {
			orderedKeys = append(orderedKeys, key)
		}
		quiesceMap[key] = rule
	}
	for _, rule := range baseline.QuiesceOnRestore {
		addQuiesceRule(rule)
	}
	for _, rule := range delta.QuiesceOnRestore {
		addQuiesceRule(rule)
	}
	for _, key := range orderedKeys {
		merged.QuiesceOnRestore = append(merged.QuiesceOnRestore, quiesceMap[key])
	}

	return merged
}

// LoadSingleConfigMap fetches and unmarshals a single ScopeConfig from a named ConfigMap.
// Supports multi-key data: 'inScope' and 'quiesceOnRestore'.
func LoadSingleConfigMap(
	ctx context.Context,
	crClient client.Client,
	namespace string,
	name string,
) (*ScopeConfig, error) {
	if crClient == nil {
		return nil, fmt.Errorf("client is nil")
	}
	cm := &corev1api.ConfigMap{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return nil, err
	}
	if cm.Data == nil {
		return nil, fmt.Errorf("ConfigMap %s/%s contains neither '%s' nor '%s' key", namespace, name, InScopeConfigKey, QuiesceOnRestoreConfigKey)
	}

	inScopeRaw, hasInScope := cm.Data[InScopeConfigKey]
	quiesceRaw, hasQuiesce := cm.Data[QuiesceOnRestoreConfigKey]
	if !hasInScope && !hasQuiesce {
		return nil, fmt.Errorf("ConfigMap %s/%s contains neither '%s' nor '%s' key", namespace, name, InScopeConfigKey, QuiesceOnRestoreConfigKey)
	}

	var cfg ScopeConfig
	if hasInScope && strings.TrimSpace(inScopeRaw) != "" {
		if err := yaml.Unmarshal([]byte(inScopeRaw), &cfg.InScope); err != nil {
			return nil, fmt.Errorf("failed to unmarshal '%s' in %s/%s: %w", InScopeConfigKey, namespace, name, err)
		}
	}
	if hasQuiesce && strings.TrimSpace(quiesceRaw) != "" {
		if err := yaml.Unmarshal([]byte(quiesceRaw), &cfg.QuiesceOnRestore); err != nil {
			return nil, fmt.Errorf("failed to unmarshal '%s' in %s/%s: %w", QuiesceOnRestoreConfigKey, namespace, name, err)
		}
	}

	if err := ValidateScopeConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration in %s/%s: %w", namespace, name, err)
	}
	return &cfg, nil
}

// LoadScopeConfig resolves the effective 2-Tier scope for a restore run:
// Tier 1 (Baseline from server flag or default "velero-ownerref-config") ∪ Tier 2 (Restore Delta).
func LoadScopeConfig(
	ctx context.Context,
	crClient client.Client,
	veleroNamespace string,
	baselineConfigMapName string,
	restoreCMRef *corev1api.TypedLocalObjectReference,
	log logrus.FieldLogger,
) (*ScopeConfig, error) {
	// 1. Resolve Tier 1 Baseline
	targetBaseline := baselineConfigMapName
	isExplicitBaseline := true
	if targetBaseline == "" {
		targetBaseline = DefaultBaselineConfigMap
		isExplicitBaseline = false
	}

	var baselineScope *ScopeConfig
	cfg, err := LoadSingleConfigMap(ctx, crClient, veleroNamespace, targetBaseline)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isExplicitBaseline {
				if log != nil {
					log.Warnf("Baseline ConfigMap %q specified by --owner-ref-configmap not found; proceeding with empty baseline", targetBaseline)
				}
			} else {
				if log != nil {
					log.Infof("Conventional baseline ConfigMap %q not found; proceeding with empty baseline", targetBaseline)
				}
			}
		} else {
			if log != nil {
				log.WithError(err).Warnf("Failed to read baseline ConfigMap %s/%s; proceeding with empty baseline", veleroNamespace, targetBaseline)
			}
		}
	} else {
		baselineScope = cfg
		if log != nil {
			log.Infof("Loaded Tier 1 baseline scope from %s/%s (%d inScope, %d quiesce rules)",
				veleroNamespace, targetBaseline, len(cfg.InScope), len(cfg.QuiesceOnRestore))
		}
	}

	// 2. Resolve Tier 2 Restore Delta (if specified)
	effective := baselineScope
	if restoreCMRef != nil && restoreCMRef.Name != "" {
		restoreScope, err := LoadSingleConfigMap(ctx, crClient, veleroNamespace, restoreCMRef.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to load per-restore ConfigMap %s/%s: %w", veleroNamespace, restoreCMRef.Name, err)
		}
		if log != nil {
			log.Infof("Loaded Tier 2 per-restore scope from %s/%s (%d inScope, %d quiesce rules)",
				veleroNamespace, restoreCMRef.Name, len(restoreScope.InScope), len(restoreScope.QuiesceOnRestore))
		}
		effective = MergeScopeConfig(baselineScope, restoreScope)
	}

	if log != nil && effective != nil {
		log.Infof("Effective 2-Tier ownerReference scope: %d inScope groups, %d quiesce rules",
			len(effective.InScope), len(effective.QuiesceOnRestore))

		// Warn if any quiesceOnRestore rule has no corresponding inScope child entries
		for _, rule := range effective.QuiesceOnRestore {
			hasInScope := false
			for _, entry := range effective.InScope {
				if entry.Group == "" || entry.Group == rule.Group {
					hasInScope = true
					break
				}
			}
			if !hasInScope {
				log.Warnf("quiesceOnRestore rule for %s/%s has no corresponding inScope entries in effective scope; quiesced root may unpause prematurely if children are not in scope", rule.Group, rule.Kind)
			}
		}
	}
	return effective, nil
}

// OwnerPatchRequest holds deferred ownerReference patch work for one child object.
type OwnerPatchRequest struct {
	Group             string                  `json:"group"`
	Version           string                  `json:"version"`
	Kind              string                  `json:"kind"`
	Resource          string                  `json:"resource"`
	Namespace         string                  `json:"namespace"`
	Name              string                  `json:"name"`
	OriginalOwnerRefs []metav1.OwnerReference `json:"originalOwnerRefs"`
}

// LiveUID is the UID and namespace of an object after it is created or found live.
// Namespace is empty when the object is cluster-scoped.
type LiveUID struct {
	UID       types.UID
	Namespace string
}

// OwnerRefRemapState tracks in-memory state during Phase 1A for execution in Phase 1B.
type OwnerRefRemapState struct {
	Enabled         bool
	Scope           *ScopeConfig
	UIDMap          map[types.UID]LiveUID
	OwnerPatchQueue []OwnerPatchRequest

	uidMapLock        sync.RWMutex
	ownerPatchQueueMu sync.Mutex
}

// NewOwnerRefRemapState creates a new OwnerRefRemapState container.
func NewOwnerRefRemapState(enabled bool, scope *ScopeConfig) *OwnerRefRemapState {
	return &OwnerRefRemapState{
		Enabled: enabled,
		Scope:   scope,
		UIDMap:  make(map[types.UID]LiveUID),
	}
}

// backupAlreadyPaused reports whether the backup object already carried the quiesce annotation.
// Objects that were already paused are left unchanged and are never unpaused.
func backupAlreadyPaused(annotations map[string]string, annotationKey string) bool {
	if annotationKey == "" || len(annotations) == 0 {
		return false
	}
	_, ok := annotations[annotationKey]
	return ok
}

// RegisterUIDMapping records a mapping from backup UID to the live UID and namespace.
func (s *OwnerRefRemapState) RegisterUIDMapping(oldUID, newUID types.UID, namespace string) {
	if s == nil || !s.Enabled || oldUID == "" || newUID == "" {
		return
	}
	s.uidMapLock.Lock()
	defer s.uidMapLock.Unlock()
	s.UIDMap[oldUID] = LiveUID{UID: newUID, Namespace: namespace}
}

// EnqueueOwnerPatch registers a child object that needs ownerReferences relinked in Phase 1B.
func (s *OwnerRefRemapState) EnqueueOwnerPatch(
	gvk schema.GroupVersionKind,
	resource string,
	namespace string,
	name string,
	originalOwnerRefs []metav1.OwnerReference,
) {
	if s == nil || !s.Enabled || len(originalOwnerRefs) == 0 {
		return
	}
	req := OwnerPatchRequest{
		Group:             gvk.Group,
		Version:           gvk.Version,
		Kind:              gvk.Kind,
		Resource:          resource,
		Namespace:         namespace,
		Name:              name,
		OriginalOwnerRefs: copyOwnerReferences(originalOwnerRefs),
	}

	s.ownerPatchQueueMu.Lock()
	defer s.ownerPatchQueueMu.Unlock()
	s.OwnerPatchQueue = append(s.OwnerPatchQueue, req)
}

// copyOwnerReferences creates a deep copy of a slice of OwnerReferences.
func copyOwnerReferences(refs []metav1.OwnerReference) []metav1.OwnerReference {
	if refs == nil {
		return nil
	}
	copied := make([]metav1.OwnerReference, len(refs))
	for i, ref := range refs {
		c := ref
		if ref.Controller != nil {
			ctrl := *ref.Controller
			c.Controller = &ctrl
		}
		if ref.BlockOwnerDeletion != nil {
			block := *ref.BlockOwnerDeletion
			c.BlockOwnerDeletion = &block
		}
		copied[i] = c
	}
	return copied
}
