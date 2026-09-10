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
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/ownerref"
)

func TestSplitJSONPath(t *testing.T) {
	cases := []struct {
		input    string
		expected []string
	}{
		{
			input:    "spec.infrastructureRef",
			expected: []string{"spec", "infrastructureRef"},
		},
		{
			input:    "spec.template.spec.volumes[*].dataVolume",
			expected: []string{"spec", "template", "spec", "volumes", "[*]", "dataVolume"},
		},
		{
			input:    "spec.volumes.[*].dataVolume",
			expected: []string{"spec", "volumes", "[*]", "dataVolume"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := splitJSONPath(tc.input)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestRemapSpecRefFields_CAPICluster(t *testing.T) {
	state := ownerref.NewOwnerRefRemapState()
	state.RegisterUIDMapping("old-vsphere-cluster-uid", "new-vsphere-cluster-uid")

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "my-cluster",
				"namespace": "target-ns",
			},
			"spec": map[string]any{
				"infrastructureRef": map[string]any{
					"apiVersion":      "infrastructure.cluster.x-k8s.io/v1beta1",
					"kind":            "VSphereCluster",
					"name":            "vsphere-cluster-1",
					"namespace":       "src-ns",
					"uid":             "old-vsphere-cluster-uid",
					"resourceVersion": "old-rv",
				},
				"controlPlaneRef": map[string]any{
					"name": "unmapped-cp",
				},
			},
		},
	}

	jsonPaths := []string{
		"spec.infrastructureRef",
		"spec.controlPlaneRef",
	}

	namespaceMapping := map[string]string{
		"src-ns": "target-ns",
	}

	changed, patchObj, allResolved, err := remapSpecRefFields(obj, jsonPaths, state, namespaceMapping, logrus.StandardLogger())
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, allResolved)

	// Check obj mutated in place
	infraRef, found, err := unstructured.NestedMap(obj.Object, "spec", "infrastructureRef")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new-vsphere-cluster-uid", infraRef["uid"])
	assert.Equal(t, "target-ns", infraRef["namespace"])
	assert.Nil(t, infraRef["resourceVersion"], "stale resourceVersion should be set to nil for RFC 7386 merge patch deletion")

	// Check patchObj structure (RFC 7386 Merge Patch)
	patchInfraRef, found, err := unstructured.NestedMap(patchObj, "spec", "infrastructureRef")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new-vsphere-cluster-uid", patchInfraRef["uid"])
	assert.Equal(t, "target-ns", patchInfraRef["namespace"])
	assert.Nil(t, patchInfraRef["resourceVersion"], "patch should explicitly specify resourceVersion: null to delete it on API server")

	patchBytes, err := json.Marshal(patchObj)
	require.NoError(t, err)
	assert.Contains(t, string(patchBytes), `"resourceVersion":null`)
}

func TestRemapSpecRefFields_KubeVirtArrayWildcard(t *testing.T) {
	state := ownerref.NewOwnerRefRemapState()
	state.RegisterUIDMapping("old-dv-1-uid", "new-dv-1-uid")
	state.RegisterUIDMapping("old-pvc-2-uid", "new-pvc-2-uid")

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      "vm-1",
				"namespace": "target-ns",
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"volumes": []any{
							map[string]any{
								"name": "volume-dv",
								"dataVolume": map[string]any{
									"name":      "dv-1",
									"namespace": "src-ns",
									"uid":       "old-dv-1-uid",
								},
							},
							map[string]any{
								"name": "volume-pvc",
								"persistentVolumeClaim": map[string]any{
									"claimName": "pvc-2",
									"namespace": "src-ns",
									"uid":       "old-pvc-2-uid",
								},
							},
						},
					},
				},
			},
		},
	}

	jsonPaths := []string{
		"spec.template.spec.volumes[*].dataVolume",
		"spec.template.spec.volumes[*].persistentVolumeClaim",
	}

	namespaceMapping := map[string]string{
		"src-ns": "target-ns",
	}

	changed, patchObj, allResolved, err := remapSpecRefFields(obj, jsonPaths, state, namespaceMapping, logrus.StandardLogger())
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, allResolved)

	// In patchObj, volumes array must be replaced as a whole per JSON Merge Patch
	volumes, found, err := unstructured.NestedSlice(patchObj, "spec", "template", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, volumes, 2)

	v1, ok := volumes[0].(map[string]any)
	require.True(t, ok)
	dv, ok := v1["dataVolume"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new-dv-1-uid", dv["uid"])
	assert.Equal(t, "target-ns", dv["namespace"])

	v2, ok := volumes[1].(map[string]any)
	require.True(t, ok)
	pvc, ok := v2["persistentVolumeClaim"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new-pvc-2-uid", pvc["uid"])
	assert.Equal(t, "target-ns", pvc["namespace"])
}

func TestProcessSpecReferences_Integration(t *testing.T) {
	scheme := runtime.NewScheme()
	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      "my-vm",
				"namespace": "prod",
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"volumes": []any{
							map[string]any{
								"name": "vol-1",
								"dataVolume": map[string]any{
									"name": "dv-1",
									"uid":  "old-dv-uid",
								},
							},
						},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm).Build()

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	state.RegisterUIDMapping("old-dv-uid", "new-dv-uid")
	scope := ownerref.NewScope()
	state.SetScope(scope)

	req := ownerref.OwnerPatchRequest{
		Group:     "kubevirt.io",
		Version:   "v1",
		Kind:      "VirtualMachine",
		Resource:  "virtualmachines",
		Namespace: "prod",
		Name:      "my-vm",
	}
	state.EnqueueSpecPatch(req)

	warnings := processSpecReferences(context.Background(), logrus.StandardLogger(), fakeClient, state, nil)
	assert.True(t, warnings.IsEmpty())

	liveVM := &unstructured.Unstructured{}
	liveVM.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "prod", Name: "my-vm"}, liveVM)
	require.NoError(t, err)

	volumes, found, err := unstructured.NestedSlice(liveVM.Object, "spec", "template", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	v0 := volumes[0].(map[string]any)
	dv := v0["dataVolume"].(map[string]any)
	assert.Equal(t, "new-dv-uid", dv["uid"])

	// Verified that resolved spec patch request is pruned from queue
	assert.Empty(t, state.GetSpecPatchQueue())
}

func TestRemapSpecRefFields_MultiPass_AlreadyRemapped(t *testing.T) {
	state := ownerref.NewOwnerRefRemapState()
	state.RegisterUIDMapping("old-uid-1", "new-uid-1")
	state.RegisterUIDMapping("old-uid-2", "new-uid-2")

	// Object where ref1 was already remapped to new-uid-1 in pass 1,
	// and ref2 is still old-uid-2 waiting to be remapped in pass 2.
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"ref1": map[string]any{
					"uid": "new-uid-1",
				},
				"ref2": map[string]any{
					"uid": "old-uid-2",
				},
			},
		},
	}

	paths := []string{"spec.ref1", "spec.ref2"}
	changed, patchObj, allResolved, err := remapSpecRefFields(obj, paths, state, nil, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, allResolved)

	// patchObj should only update ref2
	assert.NotContains(t, patchObj["spec"].(map[string]any), "ref1")
	assert.Equal(t, "new-uid-2", patchObj["spec"].(map[string]any)["ref2"].(map[string]any)["uid"])
}
