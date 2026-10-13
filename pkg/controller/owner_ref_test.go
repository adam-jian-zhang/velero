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

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/velero-io/velero/internal/hook"
	"github.com/velero-io/velero/internal/ownerref"
	"github.com/velero-io/velero/internal/volume"
	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/features"
	"github.com/velero-io/velero/pkg/itemoperation"
	"github.com/velero-io/velero/pkg/metrics"
	persistencemocks "github.com/velero-io/velero/pkg/persistence/mocks"
	"github.com/velero-io/velero/pkg/plugin/clientmgmt"
	pluginmocks "github.com/velero-io/velero/pkg/plugin/mocks"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/results"
)

func TestOwnerRefControllerFeatureGateAndPVCBaseline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  bool
		baseline string
		expected string
	}{
		{"disabled", false, "- group: example.io", "requires server feature"},
		{"enabled", true, "- group: example.io", ""},
		{"PVC baseline", true, "- kind: PersistentVolumeClaim", "per-restore opt-in"},
		{"deferred section", true, "[", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			features.NewFeatureFlagSet()
			t.Cleanup(func() { features.NewFeatureFlagSet() })
			if tc.enabled {
				features.NewFeatureFlagSet(velerov1api.OwnerRefRelinkFeatureFlag)
			}
			c := velerotest.NewFakeControllerRuntimeClient(t)
			backup := defaultBackup().StorageLocation("default").Phase(velerov1api.BackupPhaseCompleted).Result()
			location := builder.ForBackupStorageLocation("velero", "default").Provider("cloud").Bucket("bucket").Phase(velerov1api.BackupStorageLocationPhaseAvailable).Result()
			require.NoError(t, c.Create(t.Context(), backup))
			require.NoError(t, c.Create(t.Context(), location))
			baseline := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ownerref.DefaultConfigMap, Namespace: "velero"}, Data: map[string]string{"inScope": tc.baseline}}
			delta := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "delta", Namespace: "velero"}, Data: map[string]string{"inScope": "- kind: PersistentVolumeClaim"}}
			require.NoError(t, c.Create(t.Context(), baseline))
			require.NoError(t, c.Create(t.Context(), delta))
			r := &restoreReconciler{ctx: t.Context(), namespace: "velero", kbClient: c, logger: velerotest.NewLogger()}
			restore := builder.ForRestore("velero", "restore").Backup(backup.Name).Result()
			restore.Spec.OwnerRefConfigMap = &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: delta.Name}
			info, _, _ := r.validateAndComplete(t.Context(), restore)
			if tc.expected == "" {
				assert.Empty(t, restore.Status.ValidationErrors)
				require.NotNil(t, info.ownerRefState)
				assert.True(t, info.ownerRefState.Scope.IsInScope(schema.GroupVersionKind{Kind: "PersistentVolumeClaim"}))
			} else {
				assert.Contains(t, fmt.Sprint(restore.Status.ValidationErrors), tc.expected)
				assert.Nil(t, info.ownerRefState)
			}
		})
	}
}

type ownerRefTransientPatchClient struct{ client.Client }

func newOwnerRefFinalizerClient(t *testing.T) client.Client {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1api.SchemeGroupVersion})
	mapper.Add(corev1api.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	parent := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "work", UID: "parent-new"}}
	return velerotest.NewFakeControllerRuntimeClientBuilder(t).WithRESTMapper(mapper).WithObjects(parent).Build()
}

func (c *ownerRefTransientPatchClient) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "ConfigMap" {
		return apierrors.NewTooManyRequests("busy", 0)
	}
	return c.Client.Patch(ctx, obj, p, opts...)
}

func TestOwnerRefFinalizerCompletionStorageFailureAndTerminalNoReplay(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		missing, transient, corrupt, previousError bool
		replaced                                   bool
		phase                                      velerov1api.RestorePhase
	}{
		{name: "success", phase: velerov1api.RestorePhaseCompleted},
		{name: "previous failure", previousError: true, phase: velerov1api.RestorePhasePartiallyFailed},
		{name: "missing child", missing: true, phase: velerov1api.RestorePhasePartiallyFailed},
		{name: "transient", transient: true, phase: velerov1api.RestorePhasePartiallyFailed},
		{name: "missing snapshot", corrupt: true, phase: velerov1api.RestorePhaseFinalizing},
		{name: "replaced parent", replaced: true, phase: velerov1api.RestorePhaseCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newOwnerRefFinalizerClient(t)
			backup := defaultBackup().StorageLocation("default").Result()
			location := builder.ForBackupStorageLocation("velero", "default").Provider("cloud").Bucket("bucket").Result()
			restore := builder.ForRestore("velero", "restore").Backup(backup.Name).Phase(velerov1api.RestorePhaseFinalizing).Result()
			restore.UID = "restore-uid"
			restore.Status.PendingOwnerRefPatches = []velerov1api.PendingPatchRef{{Version: "v1", Kind: "ConfigMap", Namespace: "work", Name: "child", RestoredUID: "child-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "parent", UID: "parent-new"}}}}
			if tc.corrupt {
				restore.Status.PendingPatchesConfigMap = "absent"
			}
			if tc.previousError {
				restore.Status.Errors = 1
			}
			for _, obj := range []client.Object{backup, location, restore} {
				require.NoError(t, c.Create(t.Context(), obj))
			}
			if !tc.missing {
				require.NoError(t, c.Create(t.Context(), &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "work", UID: "child-uid"}}))
			}
			if tc.replaced {
				parent := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "work"}}
				require.NoError(t, c.Delete(t.Context(), parent))
				parent.UID = "replacement-parent"
				require.NoError(t, c.Create(t.Context(), parent))
			}
			store := &persistencemocks.BackupStore{}
			pm := &pluginmocks.Manager{}
			store.On("GetBackupVolumeInfos", backup.Name).Return([]*volume.BackupVolumeInfo{}, nil)
			store.On("GetRestoreVolumeInfos", restore.Name).Return([]*volume.RestoreVolumeInfo{}, nil)
			store.On("GetRestoredResourceList", restore.Name).Return(map[string][]string{}, nil)
			store.On("GetRestoreItemOperations", restore.Name).Return([]*itemoperation.RestoreOperation{}, nil)
			store.On("PutRestoreVolumeInfo", restore.Name, mock.Anything).Return(nil)
			store.On("GetRestoreResults", restore.Name).Return(map[string]results.Result{"warnings": {}, "errors": {}}, nil).Maybe()
			store.On("PutRestoreResults", backup.Name, restore.Name, mock.Anything).Return(nil).Maybe()
			pm.On("CleanupClients").Return()
			var patchClient client.Client = c
			if tc.transient {
				patchClient = &ownerRefTransientPatchClient{Client: c}
			}
			r := NewRestoreFinalizerReconciler(velerotest.NewLogger(), "velero", c, func(logrus.FieldLogger) clientmgmt.Manager { return pm }, NewFakeSingleObjectBackupStoreGetter(store), metrics.NewServerMetrics(), patchClient, hook.NewMultiHookTracker(), time.Second)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)}
			_, err := r.Reconcile(t.Context(), request)
			if tc.corrupt {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			loaded := &velerov1api.Restore{}
			require.NoError(t, c.Get(t.Context(), request.NamespacedName, loaded))
			assert.Equal(t, tc.phase, loaded.Status.Phase)
			if tc.replaced {
				assert.Zero(t, loaded.Status.Errors)
				assert.Positive(t, loaded.Status.Warnings)
				assert.Equal(t, 1, loaded.Status.OwnerRefsRelinked)
				child := &corev1api.ConfigMap{}
				require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "work", Name: "child"}, child))
				require.Len(t, child.OwnerReferences, 1)
				assert.Equal(t, "replacement-parent", string(child.OwnerReferences[0].UID))
			}
			if tc.transient || tc.corrupt {
				require.Len(t, loaded.Status.PendingOwnerRefPatches, 1)
			} else {
				assert.Empty(t, loaded.Status.PendingOwnerRefPatches)
			}
			if !tc.corrupt {
				count := loaded.Status.OwnerRefsRelinked
				_, err = r.Reconcile(t.Context(), request)
				require.NoError(t, err)
				require.NoError(t, c.Get(t.Context(), request.NamespacedName, loaded))
				assert.Equal(t, count, loaded.Status.OwnerRefsRelinked)
			}
		})
	}
}

func TestOwnerRefSyncCommitPreservesProgressAndRejectsReplacement(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	restore := builder.ForRestore("velero", "restore").Phase(velerov1api.RestorePhaseInProgress).Result()
	restore.UID = "restore-uid"
	require.NoError(t, c.Create(t.Context(), restore))
	original := restore.DeepCopy()
	live := restore.DeepCopy()
	live.Status.Progress = &velerov1api.RestoreProgress{TotalItems: 10, ItemsRestored: 10}
	require.NoError(t, c.Update(t.Context(), live))
	restore.Status.Phase = velerov1api.RestorePhaseFinalizing
	restore.Status.OwnerRefsRelinked = 1
	r := &restoreReconciler{kbClient: c, globalCrClient: c}
	require.NoError(t, r.commitOwnerRefRestore(t.Context(), original, restore))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(restore), live))
	require.NotNil(t, live.Status.Progress)
	assert.Equal(t, 10, live.Status.Progress.ItemsRestored)
	assert.Equal(t, velerov1api.RestorePhaseFinalizing, live.Status.Phase)
	require.Error(t, r.commitOwnerRefRestore(t.Context(), original, restore))
}

func TestOwnerRefCommitStillGuardsResourceVersionAfterPermanentFailuresDrain(t *testing.T) {
	features.NewFeatureFlagSet()
	t.Cleanup(func() { features.NewFeatureFlagSet() })
	c := velerotest.NewFakeControllerRuntimeClient(t)
	restore := builder.ForRestore("velero", "restore").Phase(velerov1api.RestorePhaseFinalizing).Result()
	restore.Status.PendingOwnerRefPatches = []velerov1api.PendingPatchRef{{Name: "child"}}
	require.NoError(t, c.Create(t.Context(), restore))
	original := restore.DeepCopy()
	live := restore.DeepCopy()
	live.Annotations = map[string]string{"concurrent": "update"}
	require.NoError(t, c.Update(t.Context(), live))
	restore.Status.PendingOwnerRefPatches = nil
	r := NewRestoreFinalizerReconciler(velerotest.NewLogger(), "velero", c, nil, nil, metrics.NewServerMetrics(), c, hook.NewMultiHookTracker(), time.Second)
	err := r.finishProcessing(velerov1api.RestorePhasePartiallyFailed, restore, original, t.Context())
	assert.True(t, apierrors.IsConflict(err))
}

func TestOwnerRefSyncCommitRetriesTransientErrors(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	restore := builder.ForRestore("velero", "restore").Phase(velerov1api.RestorePhaseInProgress).Result()
	restore.UID = "restore-uid"
	require.NoError(t, c.Create(t.Context(), restore))
	original := restore.DeepCopy()

	getAttempts := 0
	patchAttempts := 0
	faultClient := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, base client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*velerov1api.Restore); ok && getAttempts < 1 {
				getAttempts++
				return apierrors.NewTooManyRequests("server busy", 0)
			}
			return base.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*velerov1api.Restore); ok && patchAttempts < 1 {
				patchAttempts++
				return apierrors.NewServiceUnavailable("temporary service unavailable")
			}
			return base.Patch(ctx, obj, patch, opts...)
		},
	})

	r := &restoreReconciler{kbClient: faultClient, globalCrClient: faultClient}
	restore.Status.Phase = velerov1api.RestorePhaseFinalizing
	require.NoError(t, r.commitOwnerRefRestore(t.Context(), original, restore))
	assert.Equal(t, 1, getAttempts)
	assert.Equal(t, 1, patchAttempts)

	live := &velerov1api.Restore{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(restore), live))
	assert.Equal(t, velerov1api.RestorePhaseFinalizing, live.Status.Phase)
}

func TestOwnerRefSyncCommitAbortsOnPermanentError(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	restore := builder.ForRestore("velero", "restore").Phase(velerov1api.RestorePhaseInProgress).Result()
	restore.UID = "restore-uid"
	require.NoError(t, c.Create(t.Context(), restore))
	original := restore.DeepCopy()

	faultClient := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return apierrors.NewBadRequest("invalid payload")
		},
	})

	r := &restoreReconciler{kbClient: faultClient, globalCrClient: faultClient}
	restore.Status.Phase = velerov1api.RestorePhaseFinalizing
	err := r.commitOwnerRefRestore(t.Context(), original, restore)
	require.Error(t, err)
	assert.True(t, apierrors.IsBadRequest(err))
}
