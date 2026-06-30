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
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"k8s.io/utils/clock"

	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/constant"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"github.com/vmware-tanzu/velero/pkg/util/kube"
)

type searchRequestReconciler struct {
	client.Client
	logger         logrus.FieldLogger
	clock          clock.Clock
	searchProvider velero.SearchProvider
	defaultTTL     time.Duration
	maxPendingTTL  time.Duration
	resultBudget   int // bytes
	metrics        *metrics.ServerMetrics
}

func NewSearchRequestReconciler(
	c client.Client,
	logger logrus.FieldLogger,
	searchProvider velero.SearchProvider,
	defaultTTL time.Duration,
	maxPendingTTL time.Duration,
	resultBudget int,
	metrics *metrics.ServerMetrics,
) *searchRequestReconciler {
	return &searchRequestReconciler{
		Client:         c,
		logger:         logger,
		clock:          clock.RealClock{},
		searchProvider: searchProvider,
		defaultTTL:     defaultTTL,
		maxPendingTTL:  maxPendingTTL,
		resultBudget:   resultBudget,
		metrics:        metrics,
	}
}

func (r *searchRequestReconciler) SetupWithManager(mgr ctrl.Manager, gcInterval time.Duration) error {
	periodic := kube.NewPeriodicalEnqueueSource(
		r.logger, mgr.GetClient(), &velerov2alpha1.SearchRequestList{},
		gcInterval, kube.PeriodicalEnqueueSourceOption{},
	)
	return ctrl.NewControllerManagedBy(mgr).
		For(&velerov2alpha1.SearchRequest{}).
		WatchesRawSource(periodic).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Named(constant.ControllerSearchRequest).
		Complete(r)
}

func (r *searchRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sr := &velerov2alpha1.SearchRequest{}
	if err := r.Get(ctx, req.NamespacedName, sr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Expired? delete regardless of phase.
	if sr.Status.Expiration != nil && !r.clock.Now().Before(sr.Status.Expiration.Time) {
		return ctrl.Result{}, r.Delete(ctx, sr)
	}

	// 2. Pending-too-long?
	if isPending(sr.Status.Phase) {
		pendingFor := r.clock.Since(sr.CreationTimestamp.Time)
		if pendingFor > r.maxPendingTTL {
			return r.fail(ctx, sr, "search request pending beyond maxPendingTtl; index may be unready")
		}
	}

	switch sr.Status.Phase {
	case "", velerov2alpha1.SearchRequestPhaseNew:
		ready, err := r.searchProvider.Ready(ctx)
		if err != nil {
			r.logger.WithError(err).Debug("Failed to check readiness")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if !ready {
			// requeue; periodic GC will fail it if it stays pending too long
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.process(ctx, sr)

	case velerov2alpha1.SearchRequestPhaseProcessing:
		// crashed mid-process; reprocess
		return r.process(ctx, sr)

	default: // Processed / Failed
		// wait for expiration; periodic source will re-enqueue
		return ctrl.Result{}, nil
	}
}

func isPending(phase velerov2alpha1.SearchRequestPhase) bool {
	return phase == "" || phase == velerov2alpha1.SearchRequestPhaseNew || phase == velerov2alpha1.SearchRequestPhaseProcessing
}

func (r *searchRequestReconciler) process(ctx context.Context, sr *velerov2alpha1.SearchRequest) (ctrl.Result, error) {
	orig := sr.DeepCopy()
	sr.Status.Phase = velerov2alpha1.SearchRequestPhaseProcessing
	sr.Status.StartTimestamp = &metav1.Time{Time: r.clock.Now()}
	if err := r.Status().Patch(ctx, sr, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch status to Processing: %w", err)
	}

	params := velero.SearchParams{
		Name:       sr.Spec.Query.Name,
		Namespace:  sr.Spec.Query.Namespace,
		Kind:       sr.Spec.Query.Kind,
		APIVersion: sr.Spec.Query.APIVersion,
		Labels:     sr.Spec.Query.Labels,
		BackupName: sr.Spec.Query.BackupName,
		Limit:      clampLimit(sr.Spec.Limit),
	}

	start := r.clock.Now()
	res, err := r.searchProvider.Search(ctx, params)
	if r.metrics != nil {
		// r.metrics.ObserveSearchRequest(err == nil, r.clock.Since(start))
	} else {
		_ = start // avoid unused var if metrics are not ready
	}

	if err != nil {
		return r.fail(ctx, sr, fmt.Sprintf("search failed: %v", err))
	}

	matches := toMatches(res.Records)
	encoded, err := json.Marshal(matches)
	if err != nil {
		return r.fail(ctx, sr, fmt.Sprintf("failed to marshal results: %v", err))
	}
	for len(encoded) > r.resultBudget && len(matches) > 0 {
		matches = matches[:len(matches)-1]
		encoded, err = json.Marshal(matches)
		if err != nil {
			return r.fail(ctx, sr, fmt.Sprintf("failed to marshal results: %v", err))
		}
	}
	truncated := len(matches) < len(res.Records)

	orig2 := sr.DeepCopy()
	sr.Status.Phase = velerov2alpha1.SearchRequestPhaseProcessed
	sr.Status.Results = matches
	sr.Status.TotalCount = res.TotalCount
	sr.Status.CompletionTimestamp = &metav1.Time{Time: r.clock.Now()}
	ttl := effectiveTTL(sr.Spec.TTL.Duration, r.defaultTTL)
	sr.Status.Expiration = &metav1.Time{Time: r.clock.Now().Add(ttl)}
	if truncated {
		sr.Status.Message = "results truncated due to size; narrow the query or use the gRPC API"
	}
	if err := r.Status().Patch(ctx, sr, client.MergeFrom(orig2)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch status to Processed: %w", err)
	}

	return ctrl.Result{RequeueAfter: ttl}, nil
}

func (r *searchRequestReconciler) fail(ctx context.Context, sr *velerov2alpha1.SearchRequest, msg string) (ctrl.Result, error) {
	orig := sr.DeepCopy()
	sr.Status.Phase = velerov2alpha1.SearchRequestPhaseFailed
	sr.Status.Message = msg
	sr.Status.CompletionTimestamp = &metav1.Time{Time: r.clock.Now()}
	ttl := effectiveTTL(sr.Spec.TTL.Duration, r.defaultTTL)
	sr.Status.Expiration = &metav1.Time{Time: r.clock.Now().Add(ttl)}
	if err := r.Status().Patch(ctx, sr, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch status to Failed: %w", err)
	}
	return ctrl.Result{RequeueAfter: ttl}, nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 500 {
		return 500
	}
	return limit
}

func effectiveTTL(reqTTL, defaultTTL time.Duration) time.Duration {
	if reqTTL == 0 {
		return defaultTTL
	}
	return reqTTL
}

func toMatches(records []velero.ResourceRecord) []velerov2alpha1.SearchResourceMatch {
	if records == nil {
		return nil
	}
	matches := make([]velerov2alpha1.SearchResourceMatch, 0, len(records))
	for _, rec := range records {
		matches = append(matches, velerov2alpha1.SearchResourceMatch{
			BackupName:   rec.BackupName,
			ResourceName: rec.ResourceName,
			APIVersion:   rec.APIVersion,
			Kind:         rec.Kind,
			Namespace:    rec.Namespace,
			Labels:       rec.Labels,
		})
	}
	return matches
}
