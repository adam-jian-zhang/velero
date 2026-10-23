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
	"context"
	"fmt"

	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/velero-io/velero/pkg/util/results"
)

const (
	DefaultConfigMap    = "velero-ownerref-config"
	InScopeConfigKey    = "inScope"
	OutOfScopeConfigKey = "outOfScope"
)

// DenyListGroupKinds lists core leaf/intermediate workloads that must NEVER receive
// ownerReference relinking to preserve standard controller adoption semantics.
var DenyListGroupKinds = []schema.GroupKind{
	{Group: "", Kind: "Pod"},
	{Group: "", Kind: "ReplicationController"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "batch", Kind: "Job"},
}

// IsDeniedGroupKind checks the workload deny list, also used to filter PVC parents.
// Namespace is deliberately absent: it can still own an included child.
func IsDeniedGroupKind(gk schema.GroupKind) bool {
	for _, denied := range DenyListGroupKinds {
		if denied.Group == gk.Group && denied.Kind == gk.Kind {
			return true
		}
	}
	return false
}

// IsDeniedChildGroupKind excludes workloads and Namespaces from relinking their
// own ownerReferences. It does not prevent registration of their parent UID mappings.
func IsDeniedChildGroupKind(gk schema.GroupKind) bool {
	return gk == (schema.GroupKind{Group: "", Kind: "Namespace"}) || IsDeniedGroupKind(gk)
}

type ScopeEntry struct {
	Group string `json:"group"`
	Kind  string `json:"kind,omitempty"`
}

type Scope struct {
	entries         map[ScopeEntry]struct{}
	excludedEntries map[ScopeEntry]struct{}
}

func NewScope() *Scope {
	return &Scope{entries: make(map[ScopeEntry]struct{}), excludedEntries: make(map[ScopeEntry]struct{})}
}

func (s *Scope) IsInScope(gvk schema.GroupVersionKind) bool {
	if s == nil || IsDeniedChildGroupKind(gvk.GroupKind()) {
		return false
	}
	return !matchesScopeEntries(s.excludedEntries, gvk) && matchesScopeEntries(s.entries, gvk)
}

func matchesScopeEntries(entries map[ScopeEntry]struct{}, gvk schema.GroupVersionKind) bool {
	_, exact := entries[ScopeEntry{Group: gvk.Group, Kind: gvk.Kind}]
	_, group := entries[ScopeEntry{Group: gvk.Group}]
	return exact || group
}

// MergeConfigMap validates a source before adding any of its entries. PVCs may
// only be selected by a per-restore source, never by a server baseline. Per-restore
// outOfScope entries override all inclusions without affecting parent UID registration.
func (s *Scope) MergeConfigMap(cm *corev1api.ConfigMap, baseline bool) error {
	for key := range cm.Data {
		if key != InScopeConfigKey && key != OutOfScopeConfigKey {
			return fmt.Errorf("unsupported ownerReference configuration section %q", key)
		}
	}
	if _, present := cm.Data[OutOfScopeConfigKey]; baseline && present {
		return fmt.Errorf("%s is only supported in a per-restore ConfigMap, not a server baseline", OutOfScopeConfigKey)
	}
	if len(cm.BinaryData) != 0 {
		return fmt.Errorf("ownerReference configuration must use data, not binaryData")
	}
	entries, err := parseScopeEntries(cm.Data[InScopeConfigKey], InScopeConfigKey)
	if err != nil {
		return err
	}
	exclusions, err := parseScopeEntries(cm.Data[OutOfScopeConfigKey], OutOfScopeConfigKey)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		gk := schema.GroupKind{Group: entry.Group, Kind: entry.Kind}
		if IsDeniedChildGroupKind(gk) {
			return fmt.Errorf("%s entry %s is forbidden by the built-in child deny list", InScopeConfigKey, gk)
		}
		if baseline && entry.Group == "" && entry.Kind == "PersistentVolumeClaim" {
			return fmt.Errorf("PersistentVolumeClaim requires explicit per-restore opt-in and cannot appear in a server baseline")
		}
	}
	for _, entry := range entries {
		s.entries[entry] = struct{}{}
	}
	for _, entry := range exclusions {
		s.excludedEntries[entry] = struct{}{}
	}
	return nil
}

func parseScopeEntries(value, section string) ([]ScopeEntry, error) {
	var entries []ScopeEntry
	if err := yaml.UnmarshalStrict([]byte(value), &entries); err != nil {
		return nil, fmt.Errorf("invalid %s configuration: %w", section, err)
	}
	for _, entry := range entries {
		if entry.Group == "" && entry.Kind == "" {
			return nil, fmt.Errorf("%s entry must specify group or kind", section)
		}
	}
	return entries, nil
}

// LoadScope loads both sources in the Velero namespace. Missing baseline
// storage is optional; readable invalid baseline content is always an error.
func LoadScope(ctx context.Context, c client.Client, namespace, baseline string, ref *corev1api.TypedLocalObjectReference) (*Scope, results.Result, error) {
	warnings := results.Result{}
	if ref != nil && (ref.Name == "" || (ref.Kind != "" && ref.Kind != "ConfigMap") || (ref.APIGroup != nil && *ref.APIGroup != "")) {
		return nil, warnings, fmt.Errorf("invalid ownerRefConfigMap reference: name must be set, kind must be ConfigMap and apiGroup must be empty")
	}
	if baseline == "" {
		baseline = DefaultConfigMap
	}
	scope := NewScope()
	cm := &corev1api.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: baseline}, cm); err != nil {
		warnings.AddVeleroError(fmt.Errorf("ownerReference baseline ConfigMap %s/%s unavailable; using an empty baseline: %w", namespace, baseline, err))
	} else if err := scope.MergeConfigMap(cm, true); err != nil {
		return nil, warnings, fmt.Errorf("invalid baseline ConfigMap %s: %w", baseline, err)
	}
	if ref != nil {
		cm = &corev1api.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, cm); err != nil {
			return nil, warnings, fmt.Errorf("load ownerRefConfigMap %s/%s: %w", namespace, ref.Name, err)
		}
		if err := scope.MergeConfigMap(cm, false); err != nil {
			return nil, warnings, fmt.Errorf("invalid ownerRefConfigMap %s: %w", ref.Name, err)
		}
	}
	return scope, warnings, nil
}
