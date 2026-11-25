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
	"strings"
	"testing"

	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigMapRules(t *testing.T) {
	valid := "version: v1\nresourceRestoreOrder:\n- before: tasks.tekton.dev\n  after: pipelines.tekton.dev\n"
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"valid", valid, true}, {"empty list", "version: v1\nresourceRestoreOrder: []", true},
		{"missing version", "resourceRestoreOrder: []", false},
		{"unsupported version", "version: v2\nresourceRestoreOrder: []", false},
		{"missing list", "version: v1", false}, {"null list", "version: v1\nresourceRestoreOrder: null", false},
		{"empty document", "", false}, {"malformed", "[", false},
		{"unknown field", valid + "typo: true", false},
		{"unknown rule field", strings.ReplaceAll(valid, "before:", "befor:"), false},
		{"missing endpoint", "version: v1\nresourceRestoreOrder:\n- before: pods", false},
		{"self order", "version: v1\nresourceRestoreOrder:\n- before: pods\n  after: pods", false},
		{"invalid endpoint", strings.ReplaceAll(valid, "tasks.tekton.dev", "pods/status"), false},
		{"multiple documents", valid + "---\nversion: v1\nresourceRestoreOrder: []", false},
		{"duplicate key", valid + "version: v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "order"}, Data: map[string]string{ConfigMapKey: tc.data}}
			rules, err := ConfigMapRules(cm)
			if (err == nil) != tc.valid {
				t.Fatalf("rules %v, error %v", rules, err)
			}
			if err != nil && !strings.Contains(err.Error(), `ConfigMap velero/order data["restore-order"]`) {
				t.Fatal(err)
			}
			if len(rules) > 0 && !strings.Contains(rules[0].Source, "resourceRestoreOrder[0]") {
				t.Fatal(rules)
			}
		})
	}
	for _, data := range []map[string]string{nil, {"restore-order.yaml": valid}, {ConfigMapKey: valid, "extra": valid}} {
		if _, err := ConfigMapRules(&corev1api.ConfigMap{Data: data}); err == nil {
			t.Fatalf("accepted keys %v", data)
		}
	}
	if _, err := ConfigMapRules(&corev1api.ConfigMap{Data: map[string]string{ConfigMapKey: valid}, BinaryData: map[string][]byte{"extra": {1}}}); err == nil {
		t.Fatal("accepted binaryData")
	}
}

func TestRuleSources(t *testing.T) {
	rules := []Rule{{Before: "alphas", After: "betas"}, {Before: "betas", After: "alphas", Source: `ConfigMap velero/order data["restore-order"].resourceRestoreOrder[0]`}}
	_, err := Merge([]string{"alphas", "betas"}, nil, nil, rules)
	if err == nil {
		t.Fatal("expected cycle")
	}
	for _, source := range []string{"spec.resourceRestoreOrder[0]", rules[1].Source} {
		if !strings.Contains(err.Error(), source) {
			t.Fatal(err)
		}
	}
	_, err = Merge([]string{"alphas", "betas"}, []string{"alphas", "betas"}, nil, rules[1:])
	if err == nil || !strings.Contains(err.Error(), rules[1].Source) {
		t.Fatalf("missing server-conflict origin: %v", err)
	}
	rules[1] = rules[0]
	rules[1].Source = "ConfigMap duplicate"
	plan, err := Merge([]string{"alphas", "betas"}, nil, nil, rules)
	if err != nil || strings.Join(plan.Order, ",") != "alphas,betas" {
		t.Fatalf("duplicate: %v %v", plan, err)
	}
}

func TestValidateReference(t *testing.T) {
	group := "example.org"
	for _, ref := range []*corev1api.TypedLocalObjectReference{{Kind: "Secret", Name: "order"}, {Kind: "ConfigMap"}, {Kind: "ConfigMap", Name: "other/order"}, {Kind: "ConfigMap", Name: "order", APIGroup: &group}} {
		if ValidateReference(ref) == nil {
			t.Fatalf("accepted invalid ref %+v", ref)
		}
	}
	if err := ValidateReference(&corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: "order"}); err != nil {
		t.Fatal(err)
	}
}
