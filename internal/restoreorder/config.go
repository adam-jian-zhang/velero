/*
Copyright the Velero contributors.

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

package restoreorder

import (
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	api "github.com/velero-io/velero/pkg/apis/velero/v1"
)

const ConfigMapKey = "restore-order"

// ValidateReference accepts only an explicitly named, local core ConfigMap.
func ValidateReference(ref *corev1api.TypedLocalObjectReference) error {
	if ref == nil {
		return nil
	}
	if ref.Kind != "ConfigMap" || (ref.APIGroup != nil && *ref.APIGroup != "") || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 {
		return fmt.Errorf("spec.resourceRestoreOrderRef: expected a named core ConfigMap in the Restore namespace")
	}
	return nil
}

// ConfigMapRules parses a single versioned document and preserves its rule origins.
func ConfigMapRules(cm *corev1api.ConfigMap) ([]Rule, error) {
	source := fmt.Sprintf("ConfigMap %s/%s data[%q]", cm.Namespace, cm.Name, ConfigMapKey)
	data, ok := cm.Data[ConfigMapKey]
	if !ok || len(cm.Data) != 1 || len(cm.BinaryData) != 0 {
		return nil, fmt.Errorf("%s: expected exactly one data entry named %q and no binaryData", source, ConfigMapKey)
	}
	var doc struct {
		Version string                     `yaml:"version"`
		Rules   []api.ResourceRestoreOrder `yaml:"resourceRestoreOrder"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%s: expected exactly one YAML document", source)
	}
	if doc.Version != "v1" || doc.Rules == nil {
		return nil, fmt.Errorf("%s: require version v1 and a resourceRestoreOrder list (which may be empty)", source)
	}
	rules := InlineRules(doc.Rules)
	for i := range rules {
		rules[i].Source = fmt.Sprintf("%s.resourceRestoreOrder[%d]", source, i)
	}
	if err := Validate(rules); err != nil {
		return nil, err
	}
	return rules, nil
}
