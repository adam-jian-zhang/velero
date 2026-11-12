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
package resourcepolicies

import (
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	velerotest "github.com/velero-io/velero/pkg/test"
)

func TestExclusionYAMLTypes(t *testing.T) {
	for _, tc := range []struct {
		fields  string
		invalid bool
	}{
		{`exclude: ["123", "true", "*.tmp"]`, false},
		{`exclude: []`, false},
		{`exclude: [123]`, true}, {`exclude: [true]`, true}, {`exclude: [null]`, true},
		{`exclude: [1.5]`, true}, {`exclude: [{name: value}]`, true}, {`exclude: [[nested]]`, true},
		{`exclude: null`, true}, {`exclude: "*.tmp"`, true}, {`exclude: [" "]`, true},
		{`inheritExcludes: yes`, true}, {`inheritExcludes: "true"`, true},
		{`inheritExcludes: null`, true}, {`inheritExcludes: 1`, true},
	} {
		for _, global := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/global=%v", tc.fields, global), func(t *testing.T) {
				data := "version: v1\nvolumePolicies:\n- conditions: {}\n  action: {type: fs-backup}\n  " + tc.fields + "\n"
				cli := velerotest.NewFakeControllerRuntimeClient(t, globalPolicyConfigMap("policy", data))
				var err error
				if global {
					_, err = GetGlobalResourcePolicies(cli, "velero", "policy", logrus.New())
				} else {
					_, err = GetResourcePoliciesFromBackupWithGlobal(backupWithPolicy("policy"), cli, "", "velero", logrus.New())
				}
				if tc.invalid {
					require.Error(t, err)
					require.Contains(t, err.Error(), "volumePolicies[0]")
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestEmptyExcludeAcceptedOnAllActions(t *testing.T) {
	for _, action := range []Action{{Type: Skip}, {Type: Custom}, {Type: FSBackup}, {Type: Snapshot}, {Type: Snapshot, Parameters: map[string]any{DataMoverParameter: "velero-block"}}} {
		for _, global := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v/global=%v", action.Type, action.Parameters, global), func(t *testing.T) {
				data := fmt.Sprintf("version: v1\nvolumePolicies:\n- conditions: {}\n  action:\n    type: %s\n", action.Type)
				if action.Parameters != nil {
					data += "    parameters: {dataMover: velero-block}\n"
				}
				data += "  exclude: []\n"
				cli := velerotest.NewFakeControllerRuntimeClient(t, globalPolicyConfigMap("policy", data))
				var err error
				if global {
					_, err = GetGlobalResourcePolicies(cli, "velero", "policy", logrus.New())
				} else {
					_, err = GetResourcePoliciesFromBackupWithGlobal(backupWithPolicy("policy"), cli, "", "velero", logrus.New())
				}
				require.NoError(t, err)
			})
		}
	}
}

func TestRawBlockExclusionProvenance(t *testing.T) {
	global := VolumePolicy{Action: Action{Type: Snapshot}, Exclude: []string{"*.tmp"}}
	for _, tc := range []struct {
		name             string
		backup           *VolumePolicy
		global           *VolumePolicy
		moving           bool
		mode             *corev1api.PersistentVolumeMode
		want             []string
		dropped, invalid bool
	}{
		{name: "direct backup", backup: &global, moving: true, mode: ptr.To(corev1api.PersistentVolumeBlock), invalid: true},
		{name: "selected global is direct", global: &global, moving: true, mode: ptr.To(corev1api.PersistentVolumeBlock), invalid: true},
		{name: "inherited", backup: &VolumePolicy{Action: Action{Type: Snapshot}}, global: &global, moving: true, mode: ptr.To(corev1api.PersistentVolumeBlock), dropped: true},
		{name: "empty list inherits", backup: &VolumePolicy{Action: Action{Type: Snapshot}, Exclude: []string{}}, global: &global, moving: true, mode: ptr.To(corev1api.PersistentVolumeBlock), dropped: true},
		{name: "reset", backup: &VolumePolicy{Action: Action{Type: Snapshot}, InheritExcludes: ptr.To(false)}, global: &global, moving: true, mode: ptr.To(corev1api.PersistentVolumeBlock)},
		{name: "no patterns", backup: &VolumePolicy{Action: Action{Type: Snapshot}}, moving: true, mode: ptr.To(corev1api.PersistentVolumeBlock)},
		{name: "snapshot only", global: &global, mode: ptr.To(corev1api.PersistentVolumeBlock), want: []string{"*.tmp"}},
		{name: "filesystem default", global: &global, moving: true, want: []string{"*.tmp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Policies{}
			doc := &ResourcePolicies{Version: "v1"}
			if tc.backup != nil {
				doc.VolumePolicies = append(doc.VolumePolicies, *tc.backup)
			}
			if tc.global != nil {
				doc.VolumePolicies = append(doc.VolumePolicies, *tc.global)
				p.globalPolicyCount = 1
			}
			require.NoError(t, p.BuildPolicy(doc))
			require.NoError(t, p.Validate())
			// Deliberately use a PV without a PVC to verify source-mode detection is
			// independent of the pvcVolumeMode matching condition.
			pv := &corev1api.PersistentVolume{Spec: corev1api.PersistentVolumeSpec{VolumeMode: tc.mode}}
			patterns, dropped, err := p.GetEffectiveExclude(NewVolumeFilterData(pv, nil, nil), tc.moving)
			if tc.invalid {
				require.ErrorContains(t, err, "raw-block")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, patterns)
			require.Equal(t, tc.dropped, dropped)
		})
	}
}

func TestExclusionTypeValidationRetainsStrictFieldsAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		invalid      bool
	}{
		{"boolean true", "inheritExcludes: true", false},
		{"boolean false", "inheritExcludes: false", false},
		{"string alias", "exclude: [&pattern '*.tmp', *pattern]", false},
		{"number alias", "exclude: [&pattern 123, *pattern]", true},
		{"unknown rule field", "excludes: ['*.tmp']", true},
		{"unknown nested field", "action: {type: fs-backup, unknown: true}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := "version: v1\nvolumePolicies:\n- conditions: {}\n  action: {type: fs-backup}\n  " + tc.fields + "\n"
			if tc.name == "unknown nested field" {
				data = "version: v1\nvolumePolicies:\n- conditions: {}\n  " + tc.fields + "\n"
			}
			_, err := unmarshalResourcePolicies(&data)
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
