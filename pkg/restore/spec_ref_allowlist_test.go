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
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/ownerref"
)

func TestSplitDottedPath(t *testing.T) {
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
			got := splitDottedPath(tc.input)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestRemapSpecRefFields_CAPICluster(t *testing.T) {
	state := ownerref.NewOwnerRefRelinkState()
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

	paths := []string{
		"spec.infrastructureRef",
		"spec.controlPlaneRef",
	}

	namespaceMapping := map[string]string{
		"src-ns": "target-ns",
	}

	changed, patchObj, allResolved, err := remapSpecRefFields(obj, paths, state, namespaceMapping, logrus.StandardLogger())
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
	state := ownerref.NewOwnerRefRelinkState()
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

	paths := []string{
		"spec.template.spec.volumes[*].dataVolume",
		"spec.template.spec.volumes[*].persistentVolumeClaim",
	}

	namespaceMapping := map[string]string{
		"src-ns": "target-ns",
	}

	changed, patchObj, allResolved, err := remapSpecRefFields(obj, paths, state, namespaceMapping, logrus.StandardLogger())
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

func TestRemapSpecRefFields_DirectArrayWildcard(t *testing.T) {
	state := ownerref.NewOwnerRefRelinkState()
	state.RegisterUIDMapping("old-c1-uid", "new-c1-uid")
	state.RegisterUIDMapping("old-c2-uid", "new-c2-uid")

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "multicluster.x-k8s.io/v1alpha1",
			"kind":       "ClusterSet",
			"metadata": map[string]any{
				"name":      "set-1",
				"namespace": "target-ns",
			},
			"spec": map[string]any{
				"clusterRefs": []any{
					map[string]any{
						"apiVersion": "cluster.x-k8s.io/v1beta1",
						"kind":       "Cluster",
						"name":       "c1",
						"namespace":  "src-ns",
						"uid":        "old-c1-uid",
					},
					map[string]any{
						"apiVersion": "cluster.x-k8s.io/v1beta1",
						"kind":       "Cluster",
						"name":       "c2",
						"namespace":  "src-ns",
						"uid":        "old-c2-uid",
					},
				},
			},
		},
	}

	paths := []string{
		"spec.clusterRefs[*]",
	}

	namespaceMapping := map[string]string{
		"src-ns": "target-ns",
	}

	changed, patchObj, allResolved, err := remapSpecRefFields(obj, paths, state, namespaceMapping, logrus.StandardLogger())
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, allResolved)

	refs, found, err := unstructured.NestedSlice(patchObj, "spec", "clusterRefs")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, refs, 2)

	r1, ok := refs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new-c1-uid", r1["uid"])
	assert.Equal(t, "target-ns", r1["namespace"])

	r2, ok := refs[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new-c2-uid", r2["uid"])
	assert.Equal(t, "target-ns", r2["namespace"])
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

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-dv-uid", "new-dv-uid")
	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "kubevirt.io",
		Kind:  "VirtualMachine",
		Paths: []string{"spec.template.spec.volumes[*].dataVolume"},
	})
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

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), fakeClient, state, nil)
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, pending)
	assert.Equal(t, 1, remapped)

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
	state := ownerref.NewOwnerRefRelinkState()
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

func TestProcessSpecReferences_NonRetriableErrorBlocksTarget(t *testing.T) {
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
									"apiVersion": "cdi.kubevirt.io/v1beta1",
									"kind":       "DataVolume",
									"name":       "dv-1",
									"uid":        "old-dv-uid",
								},
							},
						},
					},
				},
			},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm).Build()
	mockClient := &mockErrorPatchClient{
		Client: baseClient,
		patchErr: apierrors.NewInvalid(
			schema.GroupKind{Group: "kubevirt.io", Kind: "VirtualMachine"},
			"my-vm",
			nil,
		),
	}

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-dv-uid", "new-dv-uid")
	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "kubevirt.io",
		Kind:  "VirtualMachine",
		Paths: []string{"spec.template.spec.volumes[*].dataVolume"},
	})
	state.SetScope(scope)

	// Target DataVolume is quiesced in the same namespace
	dvObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]any{
				"name":      "dv-1",
				"namespace": "prod",
			},
		},
	}
	dvRule := ownerref.QuiesceRule{
		Group:         "cdi.kubevirt.io",
		Kind:          "DataVolume",
		AnnotationKey: "cdi.kubevirt.io/storage-paused",
	}
	rec, _ := InjectQuiesceMetadata(dvObj, dvRule, "restore-test", logrus.StandardLogger())
	state.RecordQuiescedObject(rec)

	req := ownerref.OwnerPatchRequest{
		Group:     "kubevirt.io",
		Version:   "v1",
		Kind:      "VirtualMachine",
		Resource:  "virtualmachines",
		Namespace: "prod",
		Name:      "my-vm",
	}
	state.EnqueueSpecPatch(req)

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), mockClient, state, nil)
	assert.False(t, warnings.IsEmpty())
	assert.Empty(t, pending, "Non-retriable specRef failure must not be queued into pendingPatches")
	assert.Equal(t, 0, remapped)

	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.True(t, quiesced[0].UnquiesceBlocked, "Target DataVolume must be marked UnquiesceBlocked")
}

type mockErrorGetClient struct {
	client.Client
	getErr error
}

func (m *mockErrorGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if m.getErr != nil {
		return m.getErr
	}
	return m.Client.Get(ctx, key, obj, opts...)
}

func TestProcessSpecReferences_RetriablePatchErrorEnqueuesNonEmptyPatch(t *testing.T) {
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
									"apiVersion": "cdi.kubevirt.io/v1beta1",
									"kind":       "DataVolume",
									"name":       "dv-1",
									"uid":        "old-dv-uid",
								},
							},
						},
					},
				},
			},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm).Build()
	mockClient := &mockErrorPatchClient{
		Client: baseClient,
		patchErr: apierrors.NewConflict(
			schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"},
			"my-vm",
			fmt.Errorf("conflict"),
		),
	}

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-dv-uid", "new-dv-uid")
	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "kubevirt.io",
		Kind:  "VirtualMachine",
		Paths: []string{"spec.template.spec.volumes[*].dataVolume"},
	})
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

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), mockClient, state, nil)
	assert.False(t, warnings.IsEmpty())
	assert.Equal(t, 0, remapped)
	require.Len(t, pending, 1, "Retriable patch error with valid patch bytes must be queued for Pass 2")
	assert.NotEmpty(t, pending[0].SpecPatchJSON, "SpecPatchJSON must not be empty")
	require.Len(t, pending[0].Targets, 1, "Targets must be populated")
	assert.Equal(t, "DataVolume", pending[0].Targets[0].Kind)
	assert.Equal(t, "dv-1", pending[0].Targets[0].Name)
}

func TestProcessSpecReferences_GetFailureOmitsEmptyPatchDoesNotNamespaceBlock(t *testing.T) {
	scheme := runtime.NewScheme()
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	mockClient := &mockErrorGetClient{
		Client: baseClient,
		getErr: apierrors.NewInternalError(fmt.Errorf("transient etcd error")),
	}

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-dv-uid", "new-dv-uid")
	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "kubevirt.io",
		Kind:  "VirtualMachine",
		Paths: []string{"spec.template.spec.volumes[*].dataVolume"},
	})
	state.SetScope(scope)

	dvObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]any{
				"name":      "dv-1",
				"namespace": "prod",
			},
		},
	}
	dvRule := ownerref.QuiesceRule{
		Group:         "cdi.kubevirt.io",
		Kind:          "DataVolume",
		AnnotationKey: "cdi.kubevirt.io/storage-paused",
	}
	rec, _ := InjectQuiesceMetadata(dvObj, dvRule, "restore-test", logrus.StandardLogger())
	state.RecordQuiescedObject(rec)

	req := ownerref.OwnerPatchRequest{
		Group:     "kubevirt.io",
		Version:   "v1",
		Kind:      "VirtualMachine",
		Resource:  "virtualmachines",
		Namespace: "prod",
		Name:      "my-vm",
	}
	state.EnqueueSpecPatch(req)

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), mockClient, state, nil)
	assert.False(t, warnings.IsEmpty())
	assert.Empty(t, pending, "Get failure without patch bytes must NOT enqueue into pendingPatches")
	assert.Equal(t, 0, remapped)

	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.False(t, quiesced[0].UnquiesceBlocked, "GET-before-Targets must not namespace-block sibling quiesced objects")
}

func TestProcessSpecReferences_CrossNamespaceTargetRefRemapped(t *testing.T) {
	scheme := runtime.NewScheme()
	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      "my-vm",
				"namespace": "tenant-prod",
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"volumes": []any{
							map[string]any{
								"name": "vol-1",
								"dataVolume": map[string]any{
									"apiVersion": "cdi.kubevirt.io/v1beta1",
									"kind":       "DataVolume",
									"name":       "dv-shared",
									"namespace":  "infra-src",
									"uid":        "old-dv-uid",
								},
							},
						},
					},
				},
			},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm).Build()
	mockClient := &mockErrorPatchClient{
		Client: baseClient,
		patchErr: apierrors.NewConflict(
			schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"},
			"my-vm",
			fmt.Errorf("conflict"),
		),
	}

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-dv-uid", "new-dv-uid")
	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "kubevirt.io",
		Kind:  "VirtualMachine",
		Paths: []string{"spec.template.spec.volumes[*].dataVolume"},
	})
	state.SetScope(scope)

	req := ownerref.OwnerPatchRequest{
		Group:     "kubevirt.io",
		Version:   "v1",
		Kind:      "VirtualMachine",
		Resource:  "virtualmachines",
		Namespace: "tenant-prod",
		Name:      "my-vm",
	}
	state.EnqueueSpecPatch(req)

	nsMapping := map[string]string{
		"infra-src": "infra-dest",
	}

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), mockClient, state, nsMapping)
	assert.False(t, warnings.IsEmpty())
	assert.Equal(t, 0, remapped)
	require.Len(t, pending, 1)
	require.Len(t, pending[0].Targets, 1)
	assert.Equal(t, "cdi.kubevirt.io", pending[0].Targets[0].Group)
	assert.Equal(t, "DataVolume", pending[0].Targets[0].Kind)
	assert.Equal(t, "infra-dest", pending[0].Targets[0].Namespace)
	assert.Equal(t, "dv-shared", pending[0].Targets[0].Name)
}

func TestProcessSpecReferences_MultiHopTransitiveRootInTargets(t *testing.T) {
	scheme := runtime.NewScheme()
	machine := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Machine",
			"metadata": map[string]any{
				"name":      "worker-1",
				"namespace": "default",
			},
			"spec": map[string]any{
				"infrastructureRef": map[string]any{
					"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
					"kind":       "VSphereMachine",
					"name":       "vsphere-vm-1",
					"namespace":  "default",
					"uid":        "old-vm-uid",
				},
			},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	mockClient := &mockErrorPatchClient{
		Client: baseClient,
		patchErr: apierrors.NewConflict(
			schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "machines"},
			"worker-1",
			fmt.Errorf("conflict"),
		),
	}

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-vm-uid", "new-vm-uid")
	state.RegisterUIDMapping("old-cluster-uid", "new-cluster-uid")
	state.RegisterUIDMapping("old-ms-uid", "new-ms-uid")
	state.RegisterUIDMapping("old-machine-uid", "new-machine-uid")

	clusterObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-1",
				"namespace": "default",
			},
		},
	}
	rule := ownerref.QuiesceRule{
		Group:         "cluster.x-k8s.io",
		Kind:          "Cluster",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}
	rec, _ := InjectQuiesceMetadata(clusterObj, rule, "restore-test", logrus.StandardLogger())
	state.RecordQuiescedObjectWithUID("old-cluster-uid", rec)

	// Register multi-hop parent hierarchy:
	// Machine -> MachineSet -> Cluster
	state.RegisterParentUID("old-ms-uid", "old-cluster-uid")
	state.RegisterParentUID("old-machine-uid", "old-ms-uid")

	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "cluster.x-k8s.io",
		Kind:  "Machine",
		Paths: []string{"spec.infrastructureRef"},
	})
	state.SetScope(scope)

	req := ownerref.OwnerPatchRequest{
		OldUID:    "old-machine-uid",
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Resource:  "machines",
		Namespace: "default",
		Name:      "worker-1",
	}
	state.EnqueueSpecPatch(req)

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), mockClient, state, nil)
	assert.False(t, warnings.IsEmpty())
	assert.Equal(t, 0, remapped)
	require.Len(t, pending, 1)

	// Targets must contain both spec target (VSphereMachine) and transitive quiesced root (Cluster)
	require.Len(t, pending[0].Targets, 2, "Targets must contain spec target and quiesced root")

	assert.Equal(t, "infrastructure.cluster.x-k8s.io", pending[0].Targets[0].Group)
	assert.Equal(t, "VSphereMachine", pending[0].Targets[0].Kind)
	assert.Equal(t, "default", pending[0].Targets[0].Namespace)
	assert.Equal(t, "vsphere-vm-1", pending[0].Targets[0].Name)

	assert.Equal(t, "cluster.x-k8s.io", pending[0].Targets[1].Group)
	assert.Equal(t, "Cluster", pending[0].Targets[1].Kind)
	assert.Equal(t, "default", pending[0].Targets[1].Namespace)
	assert.Equal(t, "cluster-1", pending[0].Targets[1].Name)

	// CanUnquiesce for Cluster must return FALSE due to Cluster pinned in Targets
	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.False(t, CanUnquiesce(quiesced[0], pending), "CanUnquiesce must return false due to root Cluster pinned in Targets")
}

func TestProcessSpecReferences_MultiHopNonRetriableErrorBlocksQuiescedRoot(t *testing.T) {
	scheme := runtime.NewScheme()
	machine := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Machine",
			"metadata": map[string]any{
				"name":      "worker-1",
				"namespace": "default",
			},
			"spec": map[string]any{
				"infrastructureRef": map[string]any{
					"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
					"kind":       "VSphereMachine",
					"name":       "vsphere-vm-1",
					"namespace":  "default",
					"uid":        "old-vm-uid",
				},
			},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	mockClient := &mockErrorPatchClient{
		Client: baseClient,
		patchErr: apierrors.NewInvalid(
			schema.GroupKind{Group: "cluster.x-k8s.io", Kind: "Machine"},
			"worker-1",
			nil,
		),
	}

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-vm-uid", "new-vm-uid")
	state.RegisterUIDMapping("old-cluster-uid", "new-cluster-uid")
	state.RegisterUIDMapping("old-ms-uid", "new-ms-uid")
	state.RegisterUIDMapping("old-machine-uid", "new-machine-uid")

	clusterObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-1",
				"namespace": "default",
			},
		},
	}
	rule := ownerref.QuiesceRule{
		Group:         "cluster.x-k8s.io",
		Kind:          "Cluster",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}
	rec, _ := InjectQuiesceMetadata(clusterObj, rule, "restore-test", logrus.StandardLogger())
	state.RecordQuiescedObjectWithUID("old-cluster-uid", rec)

	// Register multi-hop parent hierarchy:
	// Machine -> MachineSet -> Cluster
	state.RegisterParentUID("old-ms-uid", "old-cluster-uid")
	state.RegisterParentUID("old-machine-uid", "old-ms-uid")

	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "cluster.x-k8s.io",
		Kind:  "Machine",
		Paths: []string{"spec.infrastructureRef"},
	})
	state.SetScope(scope)

	req := ownerref.OwnerPatchRequest{
		OldUID:    "old-machine-uid",
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Resource:  "machines",
		Namespace: "default",
		Name:      "worker-1",
	}
	state.EnqueueSpecPatch(req)

	warnings, pending, remapped := processSpecReferences(context.Background(), logrus.StandardLogger(), mockClient, state, nil)
	assert.False(t, warnings.IsEmpty())
	assert.Equal(t, 0, remapped)
	assert.Empty(t, pending, "Non-retriable patch failure must not enter pendingPatches")

	// Quiesced Cluster must be marked UnquiesceBlocked
	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.True(t, quiesced[0].UnquiesceBlocked, "Cluster must be marked UnquiesceBlocked when child specRef patch fails permanently")
	assert.False(t, CanUnquiesce(quiesced[0], nil), "CanUnquiesce must return false for blocked cluster")
}

func TestProcessSpecReferences_ResourceTimeout_QueuePreserved(t *testing.T) {
	scheme := runtime.NewScheme()
	machineObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Machine",
			"metadata": map[string]any{
				"name":      "worker-1",
				"namespace": "default",
			},
			"spec": map[string]any{
				"infrastructureRef": map[string]any{
					"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
					"kind":       "VSphereMachine",
					"name":       "vm-1",
					"uid":        "old-vm-uid",
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineObj).Build()

	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	state.RegisterUIDMapping("old-vm-uid", "new-vm-uid")
	state.RegisterUIDMapping("old-cluster-uid", "new-cluster-uid")
	state.RegisterUIDMapping("old-machine-uid", "new-machine-uid")

	clusterObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-1",
				"namespace": "default",
			},
		},
	}
	rule := ownerref.QuiesceRule{
		Group:         "cluster.x-k8s.io",
		Kind:          "Cluster",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}
	rec, _ := InjectQuiesceMetadata(clusterObj, rule, "restore-test", logrus.StandardLogger())
	state.RecordQuiescedObjectWithUID("old-cluster-uid", rec)

	// Register Machine -> Cluster parent hierarchy
	state.RegisterParentUID("old-machine-uid", "old-cluster-uid")

	scope := ownerref.NewScope()
	scope.SpecRefPaths = append(scope.SpecRefPaths, ownerref.SpecRefPathEntry{
		Group: "cluster.x-k8s.io",
		Kind:  "Machine",
		Paths: []string{"spec.infrastructureRef"},
	})
	state.SetScope(scope)

	req := ownerref.OwnerPatchRequest{
		OldUID:    "old-machine-uid",
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Machine",
		Resource:  "machines",
		Namespace: "default",
		Name:      "worker-1",
	}
	state.EnqueueSpecPatch(req)

	// Simulate Pass 1 context timeout (resourceTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	warnings, pending, remapped := processSpecReferences(ctx, logrus.StandardLogger(), fakeClient, state, nil)
	assert.False(t, warnings.IsEmpty(), "Warnings must capture context expiration")
	assert.Equal(t, 0, remapped, "Remapped count must not increment on Pass 1 timeout")

	// Verify pending patch was preserved with payload and targets via fallback fetch
	require.Len(t, pending, 1, "Spec patch must be preserved in PendingOwnerRefPatches instead of disappearing")
	assert.Equal(t, "Machine", pending[0].Kind)
	assert.Equal(t, "specRef", pending[0].PatchType)
	assert.Contains(t, pending[0].SpecPatchJSON, "new-vm-uid", "SpecPatchJSON must contain the remapped UID")

	// Target must include the transitive quiesced root Cluster
	var foundClusterTarget bool
	for _, target := range pending[0].Targets {
		if target.Group == "cluster.x-k8s.io" && target.Kind == "Cluster" && target.Name == "cluster-1" {
			foundClusterTarget = true
			break
		}
	}
	assert.True(t, foundClusterTarget, "Targets must contain the transitive root Cluster")

	// Spec patch queue in state must retain the timed-out item
	assert.Len(t, state.GetSpecPatchQueue(), 1, "Spec patch queue must retain item for Pass 2")

	// Quiesced Cluster must NOT be unquiesced while child spec patch is pending
	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.False(t, CanUnquiesce(quiesced[0], pending), "Cluster must remain paused while child spec patch is pending")
}
