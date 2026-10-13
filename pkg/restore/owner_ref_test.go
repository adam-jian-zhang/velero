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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/plugin/velero"
	riav2 "github.com/velero-io/velero/pkg/plugin/velero/restoreitemaction/v2"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/filesystem"
	"github.com/velero-io/velero/pkg/util/results"
)

func newOwnerRefRestoreClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1.AddToScheme(scheme))
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1api.SchemeGroupVersion, {Group: "example.io", Version: "v1"}, {Group: "example.io", Version: "v2"}})
	mapper.Add(corev1api.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(corev1api.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Parent"}, meta.RESTScopeNamespace)
	return fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objects...).Build()
}

func newOwnerRefRestore() *velerov1.Restore {
	return &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{Name: "restore", Namespace: "velero", UID: "restore-uid"}}
}

func newOwnerRef(uid types.UID, name string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "Parent", Name: name, UID: uid}
}

func newOwnerRefChild() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Child"})
	obj.SetName("child-0")
	obj.SetNamespace("work")
	obj.SetUID("child-uid")
	obj.SetAnnotations(map[string]string{"controller.example.io/paused": "true"})
	obj.Object["spec"] = map[string]any{"parentUID": "parent-old"}
	return obj
}

func newOwnerRefParent() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Parent"})
	obj.SetName("parent")
	obj.SetNamespace("work")
	obj.SetUID("parent-new")
	return obj
}

type ownerRefInterceptClient struct {
	client.Client
	patch  func(context.Context, client.Object, client.Patch, ...client.PatchOption) error
	get    func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
	create func(context.Context, client.Object, ...client.CreateOption) error
}

func (c *ownerRefInterceptClient) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	if c.patch != nil {
		return c.patch(ctx, obj, p, opts...)
	}
	return c.Client.Patch(ctx, obj, p, opts...)
}

func (c *ownerRefInterceptClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.get != nil {
		return c.get(ctx, key, obj, opts...)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *ownerRefInterceptClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.create != nil {
		return c.create(ctx, obj, opts...)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestOwnerRefResolutionScopeIdentityAndPartialOwners(t *testing.T) {
	req := ownerref.OwnerPatchRequest{Group: "example.io", Kind: "Child", Namespace: "work", Name: "child", Scope: ownerref.ScopeNamespaced, OwnerReferences: []metav1.OwnerReference{newOwnerRef("old", "parent"), newOwnerRef("missing", "excluded")}}
	valid := ownerref.UIDMappingRecord{LiveUID: "new", GroupKind: schema.GroupKind{Group: "example.io", Kind: "Parent"}, APIVersion: "example.io/v1", Name: "parent", Namespace: "work", Scope: ownerref.ScopeNamespaced}
	for _, tc := range []struct {
		name     string
		alter    func(*ownerref.UIDMappingRecord)
		expected int
	}{
		{"partial", func(*ownerref.UIDMappingRecord) {}, 1},
		{"rename", func(r *ownerref.UIDMappingRecord) { r.Name = "renamed" }, 0},
		{"kind change", func(r *ownerref.UIDMappingRecord) { r.GroupKind.Kind = "Other" }, 0},
		{"namespace change", func(r *ownerref.UIDMappingRecord) { r.Namespace = "other" }, 0},
		{"unknown", func(r *ownerref.UIDMappingRecord) { r.Scope = ownerref.ScopeUnknown }, 0},
		{"cluster parent", func(r *ownerref.UIDMappingRecord) { r.Scope = ownerref.ScopeCluster; r.Namespace = "" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			tc.alter(&r)
			warnings := results.Result{}
			refs := resolveOwnerReferences(req, map[types.UID]ownerref.UIDMappingRecord{"old": r}, &warnings)
			assert.Len(t, refs, tc.expected)
			assert.False(t, warnings.IsEmpty())
			if len(refs) > 0 {
				assert.Equal(t, types.UID("new"), refs[0].UID)
			}
		})
	}
	req.Scope = ownerref.ScopeCluster
	warnings := results.Result{}
	assert.Empty(t, resolveOwnerReferences(req, map[types.UID]ownerref.UIDMappingRecord{"old": valid}, &warnings))
}

func TestOwnerRefEarlyRegistrationAndExistingParentOnly(t *testing.T) {
	scope := ownerref.NewScope()
	require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"inScope": "- group: example.io"}}, false))
	state := ownerref.NewState(scope)
	ctx := &restoreContext{ownerRefRelink: state}
	backup := newOwnerRefChild()
	backup.SetUID("backup-child")
	backup.SetOwnerReferences([]metav1.OwnerReference{newOwnerRef("parent-old", "parent")})
	live := newOwnerRefChild()
	ctx.registerOwnerRefObject(backup, live, live.GroupVersionKind(), true)
	parentBackup := newOwnerRefChild()
	parentBackup.SetUID("parent-old")
	parent := newOwnerRefChild()
	parent.SetUID("parent-live")
	parent.SetName("parent")
	parent.SetKind("Parent")
	ctx.registerOwnerRefObject(parentBackup, parent, parent.GroupVersionKind(), false)
	records, queue := state.Complete()
	require.Len(t, queue, 1)
	assert.Equal(t, live.GetUID(), queue[0].RestoredUID)
	assert.Equal(t, parent.GetUID(), records["parent-old"].LiveUID)
	assert.Equal(t, ownerref.ScopeUnknown, records["parent-old"].Scope)
	assert.Equal(t, types.UID("parent-old"), backup.GetOwnerReferences()[0].UID)
}

func TestOwnerRefItemLoopRegistersSynchronousAdditionalParents(t *testing.T) {
	for _, targetNamespace := range []string{"work", "remapped"} {
		t.Run(targetNamespace, func(t *testing.T) {
			h := newHarness(t)
			h.AddItems(t, velerotest.Secrets())
			h.AddItems(t, velerotest.ServiceAccounts())
			h.restorer.resourcePriorities.HighPriorities = []string{"secrets"}
			// Fake dynamic clients do not assign UIDs; emulate the API server here.
			h.DynamicClient.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				obj.SetUID(types.UID("created-" + obj.GetName()))
				err := h.DynamicClient.Tracker().Create(action.GetResource(), obj, action.GetNamespace())
				return true, obj, err
			})
			parent := &corev1api.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "work", UID: "parent-old"}}
			child := &corev1api.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "work", UID: "child-old", OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ServiceAccount", Name: parent.Name, UID: parent.UID}}}}
			scope := ownerref.NewScope()
			require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"inScope": "- kind: Secret"}}, false))
			state := ownerref.NewState(scope)
			restore := defaultRestore().IncludedResources("secrets", "serviceaccounts").Result()
			if targetNamespace != "work" {
				restore.Spec.NamespaceMapping = map[string]string{"work": targetNamespace}
			}
			request := &Request{
				Restore: restore, Backup: defaultBackup().Result(), Log: h.log, OwnerRefRelink: state,
				BackupReader: velerotest.NewTarWriter(t).AddItems("secrets", child).AddItems("serviceaccounts", parent).Done(),
			}
			action := &pluggableAction{
				selector: velero.ResourceSelector{IncludedResources: []string{"secrets"}},
				executeFunc: func(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
					// Even a plugin mutating its backup input cannot change the stashed
					// source UID/references used for registration after Create.
					backupInput := input.ItemFromBackup.(*unstructured.Unstructured)
					backupInput.SetUID("plugin-modified")
					backupInput.SetOwnerReferences(nil)
					return &velero.RestoreItemActionExecuteOutput{UpdatedItem: input.Item, AdditionalItems: []velero.ResourceIdentifier{{GroupResource: schema.GroupResource{Resource: "serviceaccounts"}, Namespace: "work", Name: "parent"}}}, nil
				},
			}
			warnings, errs := h.restorer.Restore(request, []riav2.RestoreItemAction{action}, nil)
			assert.True(t, errs.IsEmpty(), "%+v", errs)
			assert.True(t, warnings.IsEmpty(), "%+v", warnings)
			records, queue := state.Complete()
			require.Len(t, queue, 1)
			require.Contains(t, records, types.UID("parent-old"))
			assert.Equal(t, ownerref.ScopeNamespaced, records["parent-old"].Scope)
			assert.Equal(t, types.UID("created-parent"), records["parent-old"].LiveUID)
			assert.Equal(t, targetNamespace, records["parent-old"].Namespace)
			assert.Equal(t, targetNamespace, queue[0].Namespace)
			require.Len(t, resolveOwnerReferences(queue[0], records, &warnings), 1)
			assert.Equal(t, types.UID("created-child"), queue[0].RestoredUID)
			assert.Equal(t, types.UID("parent-old"), queue[0].OwnerReferences[0].UID)
			live, err := h.DynamicClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace(targetNamespace).Get(t.Context(), "child", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Empty(t, live.GetOwnerReferences())
		})
	}
}

func TestOwnerRefExcludedChildStillSuppliesParentUID(t *testing.T) {
	h := newHarness(t)
	h.AddItems(t, velerotest.Secrets())
	h.AddItems(t, velerotest.ServiceAccounts())
	h.DynamicClient.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		obj.SetUID(types.UID("created-" + obj.GetName()))
		err := h.DynamicClient.Tracker().Create(action.GetResource(), obj, action.GetNamespace())
		return true, obj, err
	})
	parent := &corev1api.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "work", UID: "parent-old",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "work", UID: "namespace-old"}},
	}}
	child := &corev1api.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: "work", UID: "child-old",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ServiceAccount", Name: parent.Name, UID: parent.UID}},
	}}
	scope := ownerref.NewScope()
	require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"inScope": "- kind: Secret\n- kind: ServiceAccount"}}, true))
	require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"outOfScope": "- kind: ServiceAccount"}}, false))
	state := ownerref.NewState(scope)
	request := &Request{
		Restore: defaultRestore().IncludedResources("secrets", "serviceaccounts").Result(),
		Backup:  defaultBackup().Result(), Log: h.log, OwnerRefRelink: state,
		BackupReader: velerotest.NewTarWriter(t).AddItems("secrets", child).AddItems("serviceaccounts", parent).Done(),
	}
	warnings, errs := h.restorer.Restore(request, nil, nil)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	require.True(t, warnings.IsEmpty(), "%+v", warnings)
	records, queue := state.Complete()
	require.Len(t, queue, 1)
	assert.Equal(t, "Secret", queue[0].Kind)
	require.Contains(t, records, types.UID("parent-old"))
	assert.Equal(t, types.UID("created-parent"), records["parent-old"].LiveUID)
	resolved := resolveOwnerReferences(queue[0], records, &warnings)
	require.Len(t, resolved, 1)
	assert.Equal(t, types.UID("created-parent"), resolved[0].UID)
	assert.True(t, warnings.IsEmpty(), "%+v", warnings)
	live, err := h.DynamicClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}).Namespace("work").Get(t.Context(), "parent", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, live.GetOwnerReferences())
}

func TestOwnerRefNamespacesRegisterAsParentsOnly(t *testing.T) {
	for _, mode := range []string{"explicit", "implicit", "additional", "existing equal", "existing different", "existing update", "synthesized"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.AddItems(t, velerotest.Namespaces())
			h.AddItems(t, velerotest.Secrets())
			// getNamespace reads the extracted archive from disk.
			h.restorer.fileSystem = filesystem.NewFileSystem()
			h.restorer.resourcePriorities.HighPriorities = []string{"namespaces"}
			h.KubeClient.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
				ns := action.(ktesting.CreateAction).GetObject().(*corev1api.Namespace).DeepCopy()
				ns.UID = types.UID("created-" + ns.Name)
				return true, ns, h.KubeClient.Tracker().Create(action.GetResource(), ns, "")
			})
			h.DynamicClient.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				obj.SetUID(types.UID("created-" + obj.GetName()))
				return true, obj, h.DynamicClient.Tracker().Create(action.GetResource(), obj, action.GetNamespace())
			})
			root := &corev1api.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "root", UID: "root-old"}}
			ns := &corev1api.Namespace{TypeMeta: root.TypeMeta, ObjectMeta: metav1.ObjectMeta{Name: "work", UID: "namespace-old", OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: root.Name, UID: root.UID}}}}
			child := &corev1api.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: ns.Name, UID: "child-old", OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: ns.Name, UID: ns.UID}}}}
			r := defaultRestore().Result()
			if mode == "implicit" || mode == "additional" || mode == "synthesized" {
				r.Spec.IncludedResources = []string{"secrets"}
			}
			existing := mode == "existing equal" || mode == "existing different" || mode == "existing update"
			if existing {
				live := ns.DeepCopy()
				live.UID = "existing-namespace"
				live.OwnerReferences = []metav1.OwnerReference{{APIVersion: "example.io/v1", Kind: "ClusterOwner", Name: "live-owner", UID: "live-owner-uid"}}
				if mode != "existing equal" {
					live.Labels = map[string]string{"changed": "true"}
				}
				if mode == "existing update" {
					r.Spec.ExistingResourcePolicy = velerov1.ResourcePolicyTypeUpdate
				}
				require.NoError(t, h.KubeClient.Tracker().Add(live))
			}
			scope := ownerref.NewScope()
			require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"inScope": "- kind: Secret\n- kind: PersistentVolumeClaim"}}, false))
			state := ownerref.NewState(scope)
			archive := velerotest.NewTarWriter(t).AddItems("secrets", child)
			if mode != "synthesized" {
				archive.AddItems("namespaces", root, ns)
			}
			var actions []riav2.RestoreItemAction
			if mode == "additional" {
				// The outer item passes the selector, while its additional item
				// first ensures work through restoreItem's namespace path.
				outer := &corev1api.Secret{TypeMeta: child.TypeMeta, ObjectMeta: metav1.ObjectMeta{Name: "outer", Namespace: "root", Labels: map[string]string{"selected": "true"}}}
				archive.AddItems("secrets", outer)
				r.Spec.LabelSelector = &metav1.LabelSelector{MatchLabels: outer.Labels}
				actions = append(actions, &pluggableAction{
					selector: velero.ResourceSelector{IncludedResources: []string{"secrets"}},
					executeFunc: func(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
						out := &velero.RestoreItemActionExecuteOutput{UpdatedItem: input.Item}
						if input.Item.(*unstructured.Unstructured).GetName() == "outer" {
							out.AdditionalItems = []velero.ResourceIdentifier{{GroupResource: schema.GroupResource{Resource: "secrets"}, Namespace: "work", Name: "child"}}
						}
						return out, nil
					},
				})
			}
			warnings, errs := h.restorer.Restore(&Request{Restore: r, Backup: defaultBackup().Result(), Log: h.log, OwnerRefRelink: state, BackupReader: archive.Done()}, actions, nil)
			require.True(t, errs.IsEmpty(), "%+v", errs)
			require.True(t, warnings.IsEmpty(), "%+v", warnings)
			records, queue := state.Complete()
			if mode == "existing different" || mode == "synthesized" {
				assert.NotContains(t, records, ns.UID)
			} else {
				require.Contains(t, records, ns.UID)
				assert.Equal(t, ownerref.ScopeCluster, records[ns.UID].Scope)
				assert.NotEqual(t, ns.UID, records[ns.UID].LiveUID)
			}
			require.Len(t, queue, 1, "only the Secret child should be queued, even when a namespace is ensured repeatedly")
			for _, req := range queue {
				assert.NotEqual(t, "Namespace", req.Kind)
				if req.Name == "child" && mode != "existing different" && mode != "synthesized" {
					resolved := resolveOwnerReferences(req, records, &warnings)
					require.Len(t, resolved, 1)
					assert.Equal(t, records[ns.UID].LiveUID, resolved[0].UID)
					// A PVC uses the workload parent filter, not the child deny list.
					pvc := req
					pvc.Kind = "PersistentVolumeClaim"
					require.True(t, scope.IsInScope(schema.GroupVersionKind{Version: "v1", Kind: pvc.Kind}))
					resolved = resolveOwnerReferences(pvc, records, &warnings)
					require.Len(t, resolved, 1)
					assert.Equal(t, records[ns.UID].LiveUID, resolved[0].UID)
					assert.True(t, warnings.IsEmpty(), "%+v", warnings)
				}
			}
			live, err := h.KubeClient.CoreV1().Namespaces().Get(t.Context(), ns.Name, metav1.GetOptions{})
			require.NoError(t, err)
			if existing {
				assert.Equal(t, []metav1.OwnerReference{{APIVersion: "example.io/v1", Kind: "ClusterOwner", Name: "live-owner", UID: "live-owner-uid"}}, live.OwnerReferences)
			} else {
				assert.Empty(t, live.OwnerReferences, "new namespaces must retain stripped ownership")
			}
			if mode == "synthesized" {
				assert.Len(t, records, 1, "without an archive Namespace, only the Secret has a backup UID mapping")
			}
		})
	}
}
