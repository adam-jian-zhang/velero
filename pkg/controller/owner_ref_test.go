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
	"strings"
	"testing"
	"time"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
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
	"github.com/velero-io/velero/pkg/itemoperationmap"
	"github.com/velero-io/velero/pkg/metrics"
	persistencemocks "github.com/velero-io/velero/pkg/persistence/mocks"
	"github.com/velero-io/velero/pkg/plugin/clientmgmt"
	"github.com/velero-io/velero/pkg/plugin/framework"
	pluginmocks "github.com/velero-io/velero/pkg/plugin/mocks"
	"github.com/velero-io/velero/pkg/plugin/velero"
	riav2mocks "github.com/velero-io/velero/pkg/plugin/velero/mocks/restoreitemaction/v2"
	pkgrestore "github.com/velero-io/velero/pkg/restore"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/logging"
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

// Model resource registration and item operations during synchronous restore;
// the controller owns relinking, results persistence, and phase transitions.
type ownerRefOperationRestorer struct {
	fakeRestorer
	operations []*itemoperation.RestoreOperation
	register   func(*pkgrestore.Request)
}

func (r *ownerRefOperationRestorer) RestoreWithResolvers(req *pkgrestore.Request, resolver framework.RestoreItemActionResolverV2, getter pkgrestore.VolumeSnapshotterGetter) (results.Result, results.Result) {
	*req.GetItemOperationsList() = r.operations
	if r.register != nil {
		r.register(req)
	}
	return results.Result{}, results.Result{}
}

func TestOwnerRefSingleStageResultsSurviveAsyncAndFinalization(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, failure := range []string{"", "forbidden", "exhausted", "deadline"} {
			t.Run(fmt.Sprintf("async=%t/failure=%s", async, failure), func(t *testing.T) {
				features.NewFeatureFlagSet(velerov1api.OwnerRefRelinkFeatureFlag)
				t.Cleanup(func() { features.NewFeatureFlagSet() })
				logger := velerotest.NewLogger()
				c := velerotest.NewFakeControllerRuntimeClient(t)
				backup := defaultBackup().StorageLocation("default").Phase(velerov1api.BackupPhaseCompleted).Result()
				location := builder.ForBackupStorageLocation("velero", "default").Provider("cloud").Bucket("bucket").Phase(velerov1api.BackupStorageLocationPhaseAvailable).Result()
				restore := builder.ForRestore("velero", "restore").Backup(backup.Name).Phase(velerov1api.RestorePhaseNew).Result()
				restore.Spec.ItemOperationTimeout = metav1.Duration{Duration: time.Hour}
				child := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "work", UID: "child-live"}}
				scope := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ownerref.DefaultConfigMap, Namespace: "velero"}, Data: map[string]string{"inScope": "- kind: ConfigMap"}}
				for _, obj := range []client.Object{backup, location, restore, child, scope} {
					require.NoError(t, c.Create(t.Context(), obj))
				}
				patches := 0
				api := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
						if obj.GetName() == child.Name && obj.GetNamespace() == child.Namespace {
							patches++
							switch failure {
							case "forbidden":
								return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, child.Name, fmt.Errorf("denied"))
							case "exhausted":
								return apierrors.NewServiceUnavailable("busy")
							case "deadline":
								<-ctx.Done()
								return ctx.Err()
							}
						}
						return base.Patch(ctx, obj, p, opts...)
					},
					Create: func(ctx context.Context, base client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						t.Errorf("relinking must not create checkpoint objects: %T %s", obj, obj.GetName())
						return base.Create(ctx, obj, opts...)
					},
				})

				var operations []*itemoperation.RestoreOperation
				if async {
					operations = []*itemoperation.RestoreOperation{{Spec: itemoperation.RestoreOperationSpec{OperationID: "volume-op", RestoreItemAction: "volume-plugin"}, Status: itemoperation.OperationStatus{Phase: itemoperation.OperationPhaseInProgress, Created: ptr.To(metav1.Now())}}}
				}
				manager := &pluginmocks.Manager{}
				manager.On("GetRestoreItemActionsV2").Return(nil, nil)
				manager.On("CleanupClients")
				if async {
					action := &riav2mocks.RestoreItemAction{}
					action.On("Progress", "volume-op", mock.Anything).Return(velero.OperationProgress{Completed: false}, nil).Once()
					action.On("Progress", "volume-op", mock.Anything).Return(velero.OperationProgress{Completed: true}, nil).Once()
					manager.On("GetRestoreItemActionV2", "volume-plugin").Return(action, nil)
				}
				store := &persistencemocks.BackupStore{}
				store.On("GetBackupContents", backup.Name).Return(io.NopCloser(strings.NewReader("backup")), nil)
				store.On("GetBackupVolumeSnapshots", backup.Name).Return([]*volume.Snapshot{}, nil)
				store.On("GetCSIVolumeSnapshots", backup.Name).Return([]*snapshotv1api.VolumeSnapshot{}, nil)
				store.On("GetBackupVolumeInfos", backup.Name).Return([]*volume.BackupVolumeInfo{}, nil)
				store.On("PutRestoreLog", backup.Name, restore.Name, mock.Anything).Return(nil)
				var saved map[string]results.Result
				store.On("PutRestoreResults", backup.Name, restore.Name, mock.Anything).Run(func(args mock.Arguments) {
					gz, err := gzip.NewReader(args.Get(2).(io.Reader))
					require.NoError(t, err)
					require.NoError(t, json.NewDecoder(gz).Decode(&saved))
					require.NoError(t, gz.Close())
				}).Return(nil)
				store.On("PutRestoredResourceList", restore.Name, mock.Anything).Return(nil)
				store.On("PutRestoreItemOperations", restore.Name, mock.Anything).Return(nil)
				store.On("PutRestoreVolumeInfo", restore.Name, mock.Anything).Return(nil)
				store.On("PutRestoreMetadata", restore.Name, mock.Anything).Return(nil)
				store.On("GetRestoreItemOperations", restore.Name).Return(operations, nil)
				store.On("GetRestoreVolumeInfos", restore.Name).Return([]*volume.RestoreVolumeInfo{}, nil)
				store.On("GetRestoredResourceList", restore.Name).Return(map[string][]string{}, nil)
				factory := func(logrus.FieldLogger) clientmgmt.Manager { return manager }
				getter := NewFakeSingleObjectBackupStoreGetter(store)
				timeout := time.Second
				if failure == "deadline" {
					timeout = 20 * time.Millisecond
				}
				r := NewRestoreReconciler(t.Context(), "velero", &ownerRefOperationRestorer{operations: operations, register: func(req *pkgrestore.Request) {
					req.OwnerRefRelink.Register("parent-backup", ownerref.UIDMappingRecord{LiveUID: "parent-live", GroupKind: schema.GroupKind{Kind: "ConfigMap"}, APIVersion: "v1", Name: "parent", Namespace: "work", Scope: ownerref.ScopeNamespaced})
					req.OwnerRefRelink.Enqueue(ownerref.OwnerPatchRequest{Version: "v1", Kind: "ConfigMap", Namespace: "work", Name: child.Name, RestoredUID: child.UID, Scope: ownerref.ScopeNamespaced, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "parent", UID: "parent-backup"}}})
				}}, c, logger, logrus.InfoLevel, factory, getter, metrics.NewServerMetrics(), logging.FormatText, time.Hour, time.Hour, false, api, timeout, "")
				key := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)}
				_, err := r.Reconcile(t.Context(), key)
				require.NoError(t, err)
				require.NoError(t, c.Get(t.Context(), key.NamespacedName, restore))
				expectedErrors := 0
				if failure != "" {
					expectedErrors = 2
				}
				assert.Equal(t, expectedErrors, restore.Status.Errors)
				savedErrors := saved["errors"]
				assert.Equal(t, failure == "", savedErrors.IsEmpty(), "%+v", saved)
				if failure == "" {
					assert.Equal(t, 1, restore.Status.OwnerRefsRelinked)
				} else {
					assert.Zero(t, restore.Status.OwnerRefsRelinked)
				}
				if async {
					expectedPhase := velerov1api.RestorePhaseWaitingForPluginOperations
					if failure != "" {
						expectedPhase = velerov1api.RestorePhaseWaitingForPluginOperationsPartiallyFailed
					}
					assert.Equal(t, expectedPhase, restore.Status.Phase)
				} else {
					expectedPhase := velerov1api.RestorePhaseFinalizing
					if failure != "" {
						expectedPhase = velerov1api.RestorePhaseFinalizingPartiallyFailed
					}
					assert.Equal(t, expectedPhase, restore.Status.Phase)
				}
				if async {
					reconciler := NewRestoreOperationsReconciler(logger, "velero", c, time.Second, factory, getter, metrics.NewServerMetrics(), itemoperationmap.NewRestoreItemOperationsMap())
					_, err := reconciler.Reconcile(t.Context(), key)
					require.NoError(t, err)
				}
				patchesBeforeFinalization := patches
				finalizer := NewRestoreFinalizerReconciler(logger, "velero", c, factory, getter, metrics.NewServerMetrics(), api, hook.NewMultiHookTracker(), time.Second)
				_, err = finalizer.Reconcile(t.Context(), key)
				require.NoError(t, err)
				loaded := &velerov1api.Restore{}
				require.NoError(t, c.Get(t.Context(), key.NamespacedName, loaded))
				expectedFinal := velerov1api.RestorePhaseCompleted
				if failure != "" {
					expectedFinal = velerov1api.RestorePhasePartiallyFailed
				}
				assert.Equal(t, expectedFinal, loaded.Status.Phase)
				assert.Equal(t, expectedErrors, loaded.Status.Errors, "successful async/finalization must retain relink failures")
				assert.Equal(t, patchesBeforeFinalization, patches, "finalization must not replay relinking")
			})
		}
	}
}
