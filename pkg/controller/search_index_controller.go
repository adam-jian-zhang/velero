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
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/constant"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	"github.com/vmware-tanzu/velero/pkg/persistence"
	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"github.com/vmware-tanzu/velero/pkg/util/kube"
)

const reindexAnnotationKey = "search.velero.io/reindex"

type searchIndexReconciler struct {
	client.Client
	logger            logrus.FieldLogger
	clock             clock.Clock
	backupStoreGetter persistence.ObjectBackupStoreGetter
	newPluginManager  func(logrus.FieldLogger) clientmgmt.Manager
	searchProvider    velero.SearchProvider // nil if feature off
	namespace         string                // velero install namespace
	maxWorkers        int
	resyncInterval    time.Duration
	metrics           *metrics.ServerMetrics

	indexSem chan struct{}

	processedMu   sync.RWMutex
	processed     map[string]struct{}
	seeded        atomic.Bool
	backfillDone  atomic.Bool
	pendingMu     sync.Mutex
	pendingIndex  map[string]struct{}
}

func NewSearchIndexReconciler(
	c client.Client,
	logger logrus.FieldLogger,
	backupStoreGetter persistence.ObjectBackupStoreGetter,
	newPluginManager func(logrus.FieldLogger) clientmgmt.Manager,
	searchProvider velero.SearchProvider,
	namespace string,
	maxWorkers int,
	resyncInterval time.Duration,
	metrics *metrics.ServerMetrics,
) *searchIndexReconciler {
	return &searchIndexReconciler{
		Client:            c,
		logger:            logger,
		clock:             clock.RealClock{},
		backupStoreGetter: backupStoreGetter,
		newPluginManager:  newPluginManager,
		searchProvider:    searchProvider,
		namespace:         namespace,
		maxWorkers:        maxWorkers,
		resyncInterval:    resyncInterval,
		metrics:           metrics,
		indexSem:          make(chan struct{}, maxWorkers),
		processed:         make(map[string]struct{}),
		pendingIndex:      make(map[string]struct{}),
	}
}

func (r *searchIndexReconciler) SetupWithManager(mgr ctrl.Manager) error {
	periodic := kube.NewPeriodicalEnqueueSource(
		r.logger, mgr.GetClient(), &velerov1api.BackupList{},
		r.resyncInterval, kube.PeriodicalEnqueueSourceOption{},
	)
	return ctrl.NewControllerManagedBy(mgr).
		For(&velerov1api.Backup{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(ue event.UpdateEvent) bool {
				old := ue.ObjectOld.(*velerov1api.Backup)
				newObj := ue.ObjectNew.(*velerov1api.Backup)
				return terminalIndexable(old.Status.Phase, newObj.Status.Phase) ||
					hasReindexAnnotation(newObj)
			},
			CreateFunc: func(ce event.CreateEvent) bool {
				b := ce.Object.(*velerov1api.Backup)
				return isIndexablePhase(b.Status.Phase)
			},
			DeleteFunc: func(de event.DeleteEvent) bool {
				// Orphan cleanup only — primary de-index is BackupDeletionController (C8).
				return true
			},
			GenericFunc: func(ge event.GenericEvent) bool {
				return false
			},
		})).
		WatchesRawSource(periodic).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.maxWorkers}).
		Named(constant.ControllerSearchIndex).
		Complete(r)
}

func terminalIndexable(oldPhase, newPhase velerov1api.BackupPhase) bool {
	return !isIndexablePhase(oldPhase) && isIndexablePhase(newPhase)
}

func isIndexablePhase(phase velerov1api.BackupPhase) bool {
	return phase == velerov1api.BackupPhaseCompleted || phase == velerov1api.BackupPhasePartiallyFailed
}

func hasReindexAnnotation(b *velerov1api.Backup) bool {
	if b.Annotations == nil {
		return false
	}
	_, ok := b.Annotations[reindexAnnotationKey]
	return ok
}

func (r *searchIndexReconciler) ensureSeeded(ctx context.Context) error {
	if r.seeded.Load() {
		return nil
	}
	names, err := r.searchProvider.ListIndexedBackups(ctx)
	if err != nil {
		return err
	}
	r.processedMu.Lock()
	for _, name := range names {
		r.processed[name] = struct{}{}
	}
	r.processedMu.Unlock()
	r.seeded.Store(true)
	r.logger.WithField("count", len(names)).Info("Seeded search index processed set from store")
	return nil
}

func (r *searchIndexReconciler) maybeMarkReady(ctx context.Context) {
	if r.backfillDone.Load() {
		return
	}
	r.pendingMu.Lock()
	pending := len(r.pendingIndex)
	r.pendingMu.Unlock()
	if pending > 0 {
		return
	}

	// Confirm cluster has no missing indexable backups relative to processed set.
	list := &velerov1api.BackupList{}
	if err := r.List(ctx, list, client.InNamespace(r.namespace)); err != nil {
		r.logger.WithError(err).Warn("Unable to list backups while checking search readiness")
		return
	}
	for i := range list.Items {
		b := &list.Items[i]
		if !isIndexablePhase(b.Status.Phase) {
			continue
		}
		if !r.alreadyIndexed(b.Name) {
			return
		}
	}

	if err := r.searchProvider.MarkReady(ctx); err != nil {
		r.logger.WithError(err).Warn("MarkReady failed")
		return
	}
	r.backfillDone.Store(true)
	if r.metrics != nil {
		r.metrics.SetSearchReady(true)
	}
	r.logger.Info("Search index cold-start backfill complete; Ready=true")
}

func (r *searchIndexReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.searchProvider == nil {
		return ctrl.Result{}, nil
	}

	if err := r.ensureSeeded(ctx); err != nil {
		r.logger.WithError(err).Warn("Failed to seed processed backups; requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	b := &velerov1api.Backup{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		if apierrors.IsNotFound(err) {
			// Orphan cleanup when Backup CR is gone (missed BDC hook / external delete).
			if err := r.searchProvider.DeleteBackup(ctx, req.Name); err != nil {
				r.logger.WithError(err).Warn("DeleteBackup on NotFound failed; requeueing")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			if r.metrics != nil {
				r.metrics.ObserveSearchDelete(true)
			}
			r.markUnindexed(req.Name)
			r.maybeMarkReady(ctx)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Do not de-index on Deleting — BackupDeletionController owns that after
	// object-store delete succeeds (design C8).
	if !isIndexablePhase(b.Status.Phase) {
		r.maybeMarkReady(ctx)
		return ctrl.Result{}, nil
	}

	if r.alreadyIndexed(b.Name) && !hasReindexAnnotation(b) {
		r.maybeMarkReady(ctx)
		return ctrl.Result{}, nil
	}

	result, err := r.handleIndex(ctx, b)
	if err == nil && result.RequeueAfter == 0 && result.Requeue == false {
		r.maybeMarkReady(ctx)
	}
	return result, err
}

func (r *searchIndexReconciler) handleIndex(ctx context.Context, b *velerov1api.Backup) (ctrl.Result, error) {
	if hasReindexAnnotation(b) {
		orig := b.DeepCopy()
		delete(b.Annotations, reindexAnnotationKey)
		if err := r.Patch(ctx, b, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.trackPending(b.Name, true)
	defer r.trackPending(b.Name, false)

	loc := &velerov1api.BackupStorageLocation{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.StorageLocation}, loc); err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	backupStore, err := r.backupStoreGetter.Get(loc, r.newPluginManager(r.logger), r.logger)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	url, err := backupStore.GetDownloadURL(velerov1api.DownloadTarget{
		Kind: velerov1api.DownloadTargetKindBackupContents,
		Name: b.Name,
	})
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	select {
	case r.indexSem <- struct{}{}:
	default:
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	defer func() { <-r.indexSem }()

	start := r.clock.Now()
	err = r.searchProvider.IndexBackup(ctx, b.Name, url)
	if r.metrics != nil {
		r.metrics.ObserveSearchIndex(err == nil, r.clock.Since(start).Seconds())
	}

	if err != nil {
		r.logger.WithError(err).Warn("IndexBackup failed; requeueing")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	r.markIndexed(b.Name)
	return ctrl.Result{}, nil
}

func (r *searchIndexReconciler) trackPending(name string, add bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if add {
		r.pendingIndex[name] = struct{}{}
	} else {
		delete(r.pendingIndex, name)
	}
}

func (r *searchIndexReconciler) alreadyIndexed(name string) bool {
	r.processedMu.RLock()
	defer r.processedMu.RUnlock()
	_, ok := r.processed[name]
	return ok
}

func (r *searchIndexReconciler) markIndexed(name string) {
	r.processedMu.Lock()
	defer r.processedMu.Unlock()
	r.processed[name] = struct{}{}
}

func (r *searchIndexReconciler) markUnindexed(name string) {
	r.processedMu.Lock()
	defer r.processedMu.Unlock()
	delete(r.processed, name)
}
