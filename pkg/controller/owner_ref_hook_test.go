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
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/velero-io/velero/internal/hook"
	"github.com/velero-io/velero/internal/volume"
	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/features"
	"github.com/velero-io/velero/pkg/itemoperation"
	"github.com/velero-io/velero/pkg/metrics"
	persistencemocks "github.com/velero-io/velero/pkg/persistence/mocks"
	"github.com/velero-io/velero/pkg/plugin/clientmgmt"
	pluginmocks "github.com/velero-io/velero/pkg/plugin/mocks"
	pkgrestore "github.com/velero-io/velero/pkg/restore"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/results"
)

func TestOwnerRefHookOutcomesSurviveFinalizerRetry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		failure         string
		featureDisabled bool
		hookSucceeded   bool
	}{
		{name: "snapshot load", failure: "load"},
		{name: "snapshot preparation", failure: "prepare"},
		{name: "results upload", failure: "results"},
		{name: "status conflict", failure: "commit"},
		{name: "status conflict with feature disabled", failure: "commit", featureDisabled: true},
		{name: "ambiguous status commit", failure: "ambiguous"},
		{name: "successful hook and snapshot load retry", failure: "load", hookSucceeded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			features.NewFeatureFlagSet()
			t.Cleanup(func() { features.NewFeatureFlagSet() })
			if !tc.featureDisabled {
				features.NewFeatureFlagSet(velerov1api.OwnerRefRelinkFeatureFlag)
			}
			c := newOwnerRefFinalizerClient(t)
			backup := defaultBackup().StorageLocation("default").Result()
			location := builder.ForBackupStorageLocation("velero", "default").Provider("cloud").Bucket("bucket").Result()
			restore := builder.ForRestore("velero", "restore-hooks").Backup(backup.Name).Phase(velerov1api.RestorePhaseFinalizing).Result()
			restore.UID = "restore-uid"
			pendingCount := 1
			if tc.failure == "load" || tc.failure == "prepare" {
				pendingCount = 101
			}
			pending := make([]velerov1api.PendingPatchRef, pendingCount)
			for i := range pending {
				name := fmt.Sprintf("child-%d", i)
				pending[i] = velerov1api.PendingPatchRef{
					Version: "v1", Kind: "ConfigMap", Namespace: "work", Name: name, RestoredUID: "child-uid",
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "parent", UID: "parent-new"}},
				}
				require.NoError(t, c.Create(t.Context(), &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "work", UID: "child-uid"}}))
			}
			storageErrors := pkgrestore.SavePendingPatchesHybrid(t.Context(), c, restore, pending)
			require.True(t, storageErrors.IsEmpty(), "%+v", storageErrors)
			for _, obj := range []client.Object{backup, location, restore} {
				require.NoError(t, c.Create(t.Context(), obj))
			}

			faultInjected := false
			faultClient := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, base client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tc.failure == "load" && key.Name == restore.Status.PendingPatchesConfigMap && !faultInjected {
						faultInjected = true
						return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
					}
					return base.Get(ctx, key, obj, opts...)
				},
				Create: func(ctx context.Context, base client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if tc.failure == "prepare" && !faultInjected {
						faultInjected = true
						return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), fmt.Errorf("snapshot preparation failed"))
					}
					return base.Create(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*velerov1api.Restore); !ok {
						if tc.failure == "prepare" {
							return apierrors.NewTooManyRequests("child patch remains pending", 0)
						}
						return base.Patch(ctx, obj, patch, opts...)
					}
					if tc.failure == "commit" && !faultInjected {
						faultInjected = true
						live := &velerov1api.Restore{}
						require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(obj), live))
						live.Annotations = map[string]string{"concurrent": "update"}
						require.NoError(t, base.Update(ctx, live))
					}
					if err := base.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if tc.failure == "ambiguous" && !faultInjected {
						faultInjected = true
						return apierrors.NewTimeoutError("terminal status was committed but response was lost", 0)
					}
					return nil
				},
			})

			store := &persistencemocks.BackupStore{}
			pm := &pluginmocks.Manager{}
			persistedResults := map[string]results.Result{"warnings": {}, "errors": {}}
			store.On("GetBackupVolumeInfos", backup.Name).Return([]*volume.BackupVolumeInfo{}, nil)
			store.On("GetRestoreVolumeInfos", restore.Name).Return([]*volume.RestoreVolumeInfo{}, nil)
			store.On("GetRestoredResourceList", restore.Name).Return(map[string][]string{}, nil)
			store.On("GetRestoreItemOperations", restore.Name).Return([]*itemoperation.RestoreOperation{}, nil)
			store.On("PutRestoreVolumeInfo", restore.Name, mock.Anything).Return(nil)
			store.On("GetRestoreResults", restore.Name).Return(func(string) map[string]results.Result { return persistedResults }, nil).Maybe()
			if tc.failure == "results" {
				store.On("PutRestoreResults", backup.Name, restore.Name, mock.Anything).Return(fmt.Errorf("results upload failed")).Once().Run(func(mock.Arguments) { faultInjected = true })
			}
			store.On("PutRestoreResults", backup.Name, restore.Name, mock.Anything).Return(nil).Maybe().Run(func(args mock.Arguments) {
				reader, err := gzip.NewReader(args.Get(2).(io.Reader))
				require.NoError(t, err)
				defer reader.Close()
				require.NoError(t, json.NewDecoder(reader).Decode(&persistedResults))
			})
			pm.On("CleanupClients").Return()
			tracker := hook.NewMultiHookTracker()
			tracker.Add(restore.Name, "work", "pod", "container", hook.HookSourceAnnotation, "hook", "", 0)
			var hookErr error
			failedHooks := 0
			if !tc.hookSucceeded {
				hookErr = fmt.Errorf("hook failed")
				failedHooks = 1
			}
			require.NoError(t, tracker.Record(restore.Name, "work", "pod", "container", hook.HookSourceAnnotation, "hook", "", 0, !tc.hookSucceeded, hookErr))
			resourceTimeout := 5 * time.Second
			if tc.failure == "prepare" {
				resourceTimeout = 200 * time.Millisecond
			}
			r := NewRestoreFinalizerReconciler(velerotest.NewLogger(), "velero", faultClient, func(logrus.FieldLogger) clientmgmt.Manager { return pm }, NewFakeSingleObjectBackupStoreGetter(store), metrics.NewServerMetrics(), faultClient, tracker, resourceTimeout)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)}
			_, err := r.Reconcile(t.Context(), request)
			require.Error(t, err)
			require.True(t, faultInjected)
			attempted, failed := tracker.Stat(restore.Name)
			assert.Equal(t, 1, attempted, "retain hook outcomes while the terminal commit is unconfirmed")
			assert.Equal(t, failedHooks, failed)
			assert.Len(t, tracker.HookErrs(restore.Name), failedHooks)

			_, err = r.Reconcile(t.Context(), request)
			require.NoError(t, err)
			live := &velerov1api.Restore{}
			require.NoError(t, c.Get(t.Context(), request.NamespacedName, live))
			expectedPhase := velerov1api.RestorePhasePartiallyFailed
			if tc.hookSucceeded {
				expectedPhase = velerov1api.RestorePhaseCompleted
			}
			assert.Equal(t, expectedPhase, live.Status.Phase)
			require.NotNil(t, live.Status.HookStatus)
			assert.Equal(t, 1, live.Status.HookStatus.HooksAttempted)
			assert.Equal(t, failedHooks, live.Status.HookStatus.HooksFailed)
			if failedHooks > 0 {
				assert.Positive(t, live.Status.Errors)
				assert.Contains(t, persistedResults["errors"].Namespaces["work"], hookErr.Error())
				occurrences := 0
				for _, message := range persistedResults["errors"].Namespaces["work"] {
					if message == hookErr.Error() {
						occurrences++
					}
				}
				assert.Equal(t, 1, occurrences, "replayed hook diagnostics must only be persisted once")
			}
			assert.Equal(t, ownerRefResultCount(persistedResults["errors"]), live.Status.Errors)
			assert.Equal(t, ownerRefResultCount(persistedResults["warnings"]), live.Status.Warnings)
			attempted, failed = tracker.Stat(restore.Name)
			assert.Zero(t, attempted, "release hook outcomes after confirming terminal status")
			assert.Zero(t, failed)
			assert.Empty(t, tracker.HookErrs(restore.Name))
		})
	}
}

func TestOwnerRefResultsSurviveRestartAfterStatusConflict(t *testing.T) {
	for _, diagnostic := range []string{"error", "warning"} {
		t.Run(diagnostic, func(t *testing.T) {
			features.NewFeatureFlagSet()
			t.Cleanup(func() { features.NewFeatureFlagSet() })
			c := newOwnerRefFinalizerClient(t)
			backup := defaultBackup().StorageLocation("default").Result()
			location := builder.ForBackupStorageLocation("velero", "default").Provider("cloud").Bucket("bucket").Result()
			restore := builder.ForRestore("velero", "restore-results").Backup(backup.Name).Phase(velerov1api.RestorePhaseFinalizing).Result()
			restore.UID = "restore-uid"
			ref := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "parent", UID: "parent-new"}
			if diagnostic == "warning" {
				block := true
				ref.BlockOwnerDeletion = &block
			}
			restore.Status.PendingOwnerRefPatches = []velerov1api.PendingPatchRef{{Version: "v1", Kind: "ConfigMap", Namespace: "work", Name: "child", RestoredUID: "child-uid", OwnerReferences: []metav1.OwnerReference{ref}}}
			for _, obj := range []client.Object{backup, location, restore, &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "work", Name: "child", UID: "child-uid"}}} {
				require.NoError(t, c.Create(t.Context(), obj))
			}
			childAttempts, commits := 0, 0
			faultClient := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*velerov1api.Restore); ok {
						commits++
						if commits == 1 {
							return apierrors.NewConflict(schema.GroupResource{Group: "velero.io", Resource: "restores"}, obj.GetName(), fmt.Errorf("concurrent update"))
						}
					} else {
						childAttempts++
						if childAttempts == 1 {
							return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), fmt.Errorf("admission denied"))
						}
					}
					return base.Patch(ctx, obj, patch, opts...)
				},
			})
			store := &persistencemocks.BackupStore{}
			pm := &pluginmocks.Manager{}
			persisted := map[string]results.Result{"warnings": {}, "errors": {}}
			store.On("GetBackupVolumeInfos", backup.Name).Return([]*volume.BackupVolumeInfo{}, nil)
			store.On("GetRestoreVolumeInfos", restore.Name).Return([]*volume.RestoreVolumeInfo{}, nil)
			store.On("GetRestoredResourceList", restore.Name).Return(map[string][]string{}, nil)
			store.On("GetRestoreItemOperations", restore.Name).Return([]*itemoperation.RestoreOperation{}, nil)
			store.On("PutRestoreVolumeInfo", restore.Name, mock.Anything).Return(nil)
			store.On("GetRestoreResults", restore.Name).Return(func(string) map[string]results.Result { return persisted }, nil)
			store.On("PutRestoreResults", backup.Name, restore.Name, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				reader, err := gzip.NewReader(args.Get(2).(io.Reader))
				require.NoError(t, err)
				defer reader.Close()
				require.NoError(t, json.NewDecoder(reader).Decode(&persisted))
			})
			pm.On("CleanupClients").Return()
			for attempt := range 2 {
				// No in-memory outcomes survive between reconciler instances.
				r := NewRestoreFinalizerReconciler(velerotest.NewLogger(), "velero", faultClient, func(logrus.FieldLogger) clientmgmt.Manager { return pm }, NewFakeSingleObjectBackupStoreGetter(store), metrics.NewServerMetrics(), faultClient, hook.NewMultiHookTracker(), time.Second)
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)})
				if attempt == 0 {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			live := &velerov1api.Restore{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(restore), live))
			assert.Empty(t, live.Status.PendingOwnerRefPatches)
			assert.Equal(t, 1, live.Status.OwnerRefsRelinked)
			if diagnostic == "error" {
				assert.Equal(t, velerov1api.RestorePhasePartiallyFailed, live.Status.Phase)
				assert.Equal(t, 1, live.Status.Errors)
				assert.Len(t, persisted["errors"].Namespaces["work"], 1)
			} else {
				assert.Equal(t, velerov1api.RestorePhaseCompleted, live.Status.Phase)
				assert.Equal(t, 1, live.Status.Warnings)
				assert.Len(t, persisted["warnings"].Namespaces["work"], 1)
			}
			store.AssertNumberOfCalls(t, "GetRestoreResults", 2)
		})
	}
}
