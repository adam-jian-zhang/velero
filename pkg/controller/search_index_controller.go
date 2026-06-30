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

	processedMu sync.RWMutex
	processed   map[string]struct{}
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
					newObj.Status.Phase == velerov1api.BackupPhaseDeleting ||
					hasReindexAnnotation(newObj)
			},
			CreateFunc: func(ce event.CreateEvent) bool {
				b := ce.Object.(*velerov1api.Backup)
				return isIndexablePhase(b.Status.Phase)
			},
			DeleteFunc: func(de event.DeleteEvent) bool {
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

func (r *searchIndexReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.searchProvider == nil {
		return ctrl.Result{}, nil
	}

	b := &velerov1api.Backup{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		if apierrors.IsNotFound(err) {
			_ = r.searchProvider.DeleteBackup(ctx, req.Name)
			r.markUnindexed(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	switch {
	case b.Status.Phase == velerov1api.BackupPhaseDeleting || b.DeletionTimestamp != nil:
		return r.handleDelete(ctx, b)

	case isIndexablePhase(b.Status.Phase):
		if r.alreadyIndexed(b.Name) && !hasReindexAnnotation(b) {
			return ctrl.Result{}, nil
		}
		return r.handleIndex(ctx, b)

	default:
		return ctrl.Result{}, nil
	}
}

func (r *searchIndexReconciler) handleIndex(ctx context.Context, b *velerov1api.Backup) (ctrl.Result, error) {
	if hasReindexAnnotation(b) {
		orig := b.DeepCopy()
		delete(b.Annotations, reindexAnnotationKey)
		if err := r.Patch(ctx, b, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}

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

	if b.DeletionTimestamp != nil {
		return r.handleDelete(ctx, b)
	}

	start := r.clock.Now()
	err = r.searchProvider.IndexBackup(ctx, b.Name, url)
	if r.metrics != nil {
		// r.metrics.ObserveSearchIndex(err == nil, r.clock.Since(start))
	} else {
		_ = start
	}

	if err != nil {
		r.logger.WithError(err).Warn("IndexBackup failed; requeueing")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil // simplistic backoff for now
	}
	r.markIndexed(b.Name)
	return ctrl.Result{}, nil
}

func (r *searchIndexReconciler) handleDelete(ctx context.Context, b *velerov1api.Backup) (ctrl.Result, error) {
	if err := r.searchProvider.DeleteBackup(ctx, b.Name); err != nil {
		r.logger.WithError(err).Warn("DeleteBackup failed; requeueing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	r.markUnindexed(b.Name)
	return ctrl.Result{}, nil
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
