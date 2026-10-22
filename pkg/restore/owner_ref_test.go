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
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func newOwnerRefPending(i int) velerov1.PendingPatchRef {
	return velerov1.PendingPatchRef{Group: "example.io", Version: "v1", Kind: "Child", Name: fmt.Sprintf("child-%d", i), Namespace: "work", RestoredUID: "child-uid", OwnerReferences: []metav1.OwnerReference{newOwnerRef("parent-new", "parent")}}
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
	req := ownerref.OwnerPatchRequest{Group: "example.io", Kind: "Child", Namespace: "work", Name: "child", Namespaced: true, ScopeKnown: true, OwnerReferences: []metav1.OwnerReference{newOwnerRef("old", "parent"), newOwnerRef("missing", "excluded")}}
	valid := ownerref.UIDMappingRecord{LiveUID: "new", GroupKind: schema.GroupKind{Group: "example.io", Kind: "Parent"}, APIVersion: "example.io/v1", Name: "parent", Namespace: "work", Namespaced: true, ScopeKnown: true}
	for _, tc := range []struct {
		name     string
		alter    func(*ownerref.UIDMappingRecord)
		expected int
	}{
		{"partial", func(*ownerref.UIDMappingRecord) {}, 1},
		{"rename", func(r *ownerref.UIDMappingRecord) { r.Name = "renamed" }, 0},
		{"kind change", func(r *ownerref.UIDMappingRecord) { r.GroupKind.Kind = "Other" }, 0},
		{"namespace change", func(r *ownerref.UIDMappingRecord) { r.Namespace = "other" }, 0},
		{"unknown", func(r *ownerref.UIDMappingRecord) { r.ScopeKnown = false }, 0},
		{"cluster parent", func(r *ownerref.UIDMappingRecord) { r.Namespaced = false; r.Namespace = "" }, 1},
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
	req.Namespaced = false
	warnings := results.Result{}
	assert.Empty(t, resolveOwnerReferences(req, map[types.UID]ownerref.UIDMappingRecord{"old": valid}, &warnings))
}

func TestOwnerRefPatchMergesLivePrecedenceWithoutSpecOrPauseChanges(t *testing.T) {
	child := newOwnerRefChild()
	controlling := true
	live := newOwnerRef("live-uid", "live")
	live.Controller = &controlling
	child.SetOwnerReferences([]metav1.OwnerReference{live})
	c := newOwnerRefRestoreClient(t, child, newOwnerRefParent())
	restore := newOwnerRefRestore()
	pending := newOwnerRefPending(0)
	pending.OwnerReferences[0].Controller = &controlling
	warnings, errs, remaining := RetryPendingPatches(context.Background(), c, restore, []velerov1.PendingPatchRef{pending})
	assert.False(t, warnings.IsEmpty())
	assert.True(t, errs.IsEmpty())
	assert.Empty(t, remaining)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(child), child))
	refs := child.GetOwnerReferences()
	require.Len(t, refs, 2)
	assert.True(t, *refs[0].Controller)
	assert.Nil(t, refs[1].Controller)
	assert.Equal(t, "parent-old", child.Object["spec"].(map[string]any)["parentUID"])
	assert.Equal(t, "true", child.GetAnnotations()["controller.example.io/paused"])
	assert.Equal(t, 1, restore.Status.OwnerRefsRelinked)
}

func TestOwnerRefPatchVersionMatchingAndReplacementAfterConflict(t *testing.T) {
	child := newOwnerRefChild()
	ref := newOwnerRef("parent-stale", "parent")
	ref.APIVersion = "example.io/v2"
	child.SetOwnerReferences([]metav1.OwnerReference{ref})
	base := newOwnerRefRestoreClient(t, child, newOwnerRefParent())
	base.RESTMapper().(*meta.DefaultRESTMapper).Add(schema.GroupVersionKind{Group: "example.io", Version: "v2", Kind: "Parent"}, meta.RESTScopeNamespace)
	calls := 0
	c := &ownerRefInterceptClient{Client: base}
	c.patch = func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		calls++
		if calls == 1 {
			require.NoError(t, base.Delete(ctx, child))
			replacement := newOwnerRefChild()
			replacement.SetUID("replacement")
			replacement.SetOwnerReferences([]metav1.OwnerReference{ref})
			require.NoError(t, base.Create(ctx, replacement))
			return apierrors.NewConflict(schema.GroupResource{Group: "example.io", Resource: "children"}, obj.GetName(), fmt.Errorf("changed"))
		}
		return base.Patch(ctx, obj, p, opts...)
	}
	warnings, errs, remaining := RetryPendingPatches(context.Background(), c, newOwnerRefRestore(), []velerov1.PendingPatchRef{newOwnerRefPending(0)})
	assert.False(t, warnings.IsEmpty())
	assert.True(t, errs.IsEmpty())
	assert.Empty(t, remaining)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(child), child))
	require.Len(t, child.GetOwnerReferences(), 1)
	assert.Equal(t, "example.io/v2", child.GetOwnerReferences()[0].APIVersion)
	assert.Equal(t, types.UID("parent-new"), child.GetOwnerReferences()[0].UID)
}

func TestOwnerRefForbiddenFallbackAndPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		name            string
		block, fallback bool
		failure         error
	}{
		{"fallback", true, true, apierrors.NewForbidden(schema.GroupResource{}, "child", fmt.Errorf("delete owner denied"))},
		{"persistent forbidden", true, false, apierrors.NewForbidden(schema.GroupResource{}, "child", fmt.Errorf("denied"))},
		{"forbidden without flag", false, false, apierrors.NewForbidden(schema.GroupResource{}, "child", fmt.Errorf("denied"))},
		{"not found", false, false, apierrors.NewNotFound(schema.GroupResource{}, "child")},
		{"bad request", false, false, apierrors.NewBadRequest("invalid")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newOwnerRefRestoreClient(t, newOwnerRefChild(), newOwnerRefParent())
			calls := 0
			c := &ownerRefInterceptClient{Client: base, patch: func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				calls++
				if tc.fallback && calls > 1 {
					return base.Patch(ctx, obj, p, opts...)
				}
				return tc.failure
			}}
			req := newOwnerRefPending(0)
			req.OwnerReferences[0].BlockOwnerDeletion = &tc.block
			_, errs, remaining := RetryPendingPatches(context.Background(), c, newOwnerRefRestore(), []velerov1.PendingPatchRef{req})
			assert.Empty(t, remaining)
			assert.Equal(t, tc.fallback, errs.IsEmpty())
			if tc.block {
				assert.Equal(t, 2, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
		})
	}
}

func TestOwnerRefTimeoutResolutionAndIndependentPersistence(t *testing.T) {
	scope := ownerref.NewScope()
	require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"inScope": "- group: example.io"}}, false))
	state := ownerref.NewState(scope)
	state.Register("parent-old", ownerref.UIDMappingRecord{LiveUID: "parent-new", GroupKind: schema.GroupKind{Group: "example.io", Kind: "Parent"}, APIVersion: "example.io/v1", Name: "parent", Namespace: "work", Namespaced: true, ScopeKnown: true})
	for i := range 101 {
		p := newOwnerRefPending(i)
		state.Enqueue(ownerref.OwnerPatchRequest{Group: p.Group, Version: p.Version, Kind: p.Kind, Namespace: p.Namespace, Name: p.Name, RestoredUID: p.RestoredUID, Namespaced: true, ScopeKnown: true, OwnerReferences: []metav1.OwnerReference{newOwnerRef("parent-old", "parent")}})
	}
	base := newOwnerRefRestoreClient(t)
	c := &ownerRefInterceptClient{Client: base, get: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
		t.Fatal("child API called after patch deadline")
		return nil
	}}
	parent := context.Background()
	expired, cancel := OwnerRefStageContext(parent, time.Nanosecond)
	defer cancel()
	<-expired.Done()
	r := newOwnerRefRestore()
	_, errs, pending := ApplyOwnerRefRelinking(expired, c, r, state)
	assert.True(t, errs.IsEmpty())
	require.Len(t, pending, 101)
	persist, persistCancel := OwnerRefStageContext(parent, time.Second)
	defer persistCancel()
	storageErrs := SavePendingPatches(persist, base, r, pending)
	assert.True(t, storageErrs.IsEmpty())
	assert.NotEmpty(t, r.Status.PendingPatchesConfigMap)
	loaded, err := LoadPendingPatches(persist, base, r)
	require.NoError(t, err)
	assert.Equal(t, pending, loaded)
	// Pass 2 likewise retains all work after its own child deadline and can
	// prepare its terminal pending partition under a fresh persistence budget.
	_, retryErrs, remaining := RetryPendingPatches(expired, c, r, loaded)
	assert.True(t, retryErrs.IsEmpty())
	require.Equal(t, loaded, remaining)
	persist2, cancel2 := OwnerRefStageContext(parent, time.Second)
	defer cancel2()
	require.NoError(t, ReconcilePendingPatchesConfigMap(persist2, base, r, remaining))
	assert.NotEmpty(t, r.Status.PendingPatchesConfigMap)
	parentCanceled, parentCancel := context.WithCancel(parent)
	parentCancel()
	storageCtx, storageCancel := OwnerRefStageContext(parentCanceled, time.Second)
	defer storageCancel()
	assert.ErrorIs(t, storageCtx.Err(), context.Canceled)
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
	records, queue := state.Freeze()
	require.Len(t, queue, 1)
	assert.Equal(t, live.GetUID(), queue[0].RestoredUID)
	assert.Equal(t, parent.GetUID(), records["parent-old"].LiveUID)
	assert.False(t, records["parent-old"].ScopeKnown)
	assert.Equal(t, types.UID("parent-old"), backup.GetOwnerReferences()[0].UID)
}

func TestOwnerRefItemLoopRegistersSynchronousAdditionalParents(t *testing.T) {
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
	records, queue := state.Freeze()
	require.Len(t, queue, 1)
	require.Contains(t, records, types.UID("parent-old"))
	assert.True(t, records["parent-old"].Namespaced)
	assert.True(t, records["parent-old"].ScopeKnown)
	assert.Equal(t, types.UID("created-parent"), records["parent-old"].LiveUID)
	assert.Equal(t, types.UID("created-child"), queue[0].RestoredUID)
	assert.Equal(t, types.UID("parent-old"), queue[0].OwnerReferences[0].UID)
	live, err := h.DynamicClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace("work").Get(t.Context(), "child", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, live.GetOwnerReferences())
}

func TestOwnerRefTransportFailuresRemainPending(t *testing.T) {
	for _, failure := range []error{
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		net.ErrClosed,
		fmt.Errorf("http2: client connection lost"),
		fmt.Errorf("http2: server sent GOAWAY and closed the connection"),
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			base := newOwnerRefRestoreClient(t, newOwnerRefChild(), newOwnerRefParent())
			attempts := 0
			c := &ownerRefInterceptClient{Client: base, patch: func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
				attempts++
				return &url.Error{Op: "Patch", URL: "https://apiserver", Err: failure}
			}}
			r := newOwnerRefRestore()
			_, errs, pending := RetryPendingPatches(t.Context(), c, r, []velerov1.PendingPatchRef{newOwnerRefPending(0)})
			require.True(t, errs.IsEmpty(), "%+v", errs)
			require.Len(t, pending, 1)
			assert.Greater(t, attempts, 1)
			assert.Zero(t, r.Status.OwnerRefsRelinked)
			_, errs, pending = RetryPendingPatches(t.Context(), base, r, pending)
			assert.True(t, errs.IsEmpty(), "%+v", errs)
			assert.Empty(t, pending)
			assert.Equal(t, 1, r.Status.OwnerRefsRelinked)
		})
	}
}

func TestOwnerRefNamespacesParticipateInRestore(t *testing.T) {
	for _, mode := range []string{"explicit", "implicit", "additional", "existing equal", "existing different", "existing update"} {
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
			if mode == "implicit" || mode == "additional" {
				r.Spec.IncludedResources = []string{"secrets"}
			}
			existing := mode == "existing equal" || mode == "existing different" || mode == "existing update"
			if existing {
				live := ns.DeepCopy()
				live.UID = "existing-namespace"
				live.OwnerReferences = nil
				if mode != "existing equal" {
					live.Labels = map[string]string{"changed": "true"}
				}
				if mode == "existing update" {
					r.Spec.ExistingResourcePolicy = velerov1.ResourcePolicyTypeUpdate
				}
				require.NoError(t, h.KubeClient.Tracker().Add(live))
			}
			scope := ownerref.NewScope()
			require.NoError(t, scope.MergeConfigMap(&corev1api.ConfigMap{Data: map[string]string{"inScope": "- kind: Namespace\n- kind: Secret"}}, false))
			state := ownerref.NewState(scope)
			archive := velerotest.NewTarWriter(t).AddItems("namespaces", root, ns).AddItems("secrets", child)
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
			records, queue := state.Freeze()
			if mode == "existing different" {
				assert.NotContains(t, records, ns.UID)
			} else {
				require.Contains(t, records, ns.UID)
				assert.True(t, records[ns.UID].ScopeKnown)
				assert.False(t, records[ns.UID].Namespaced)
				assert.NotEqual(t, ns.UID, records[ns.UID].LiveUID)
			}
			namespaceRequests := 0
			for _, req := range queue {
				if req.Kind == "Namespace" {
					namespaceRequests++
					assert.Equal(t, types.UID("created-work"), req.RestoredUID)
					assert.Equal(t, ns.OwnerReferences, req.OwnerReferences)
				} else if req.Name == "child" && mode != "existing different" {
					resolved := resolveOwnerReferences(req, records, &warnings)
					require.Len(t, resolved, 1)
					assert.Equal(t, records[ns.UID].LiveUID, resolved[0].UID)
				}
			}
			if existing {
				assert.Zero(t, namespaceRequests, "existing namespaces must not be relinked")
			} else {
				assert.Equal(t, 1, namespaceRequests, "ensuring a namespace again must not enqueue it twice")
			}
		})
	}
}
