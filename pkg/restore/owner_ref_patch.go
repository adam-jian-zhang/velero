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
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubenet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/util/results"
)

// DefaultOwnerRefStageTimeout is the fallback timeout applied to an ownerReference
// relinking stage when no explicit deadline or resource timeout is provided.
const DefaultOwnerRefStageTimeout = 10 * time.Minute

// OwnerRefStageContext gives each stage its own deadline while retaining
// cancellation from the restore/controller lifetime.
func OwnerRefStageContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = DefaultOwnerRefStageTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func ownerIdentity(ref metav1.OwnerReference) string {
	gv, _ := schema.ParseGroupVersion(ref.APIVersion)
	return gv.Group + "/" + ref.Kind + "/" + ref.Name
}

// ownerRefServedVersions is fixed before child patching. Merge retries perform
// no discovery or parent reads, including when a live reference uses another version.
type ownerRefServedVersions map[schema.GroupVersionKind]bool

func (versions ownerRefServedVersions) contains(ref metav1.OwnerReference) bool {
	return versions[schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)]
}

func resolveOwnerReferences(req ownerref.OwnerPatchRequest, records map[types.UID]ownerref.UIDMappingRecord, warnings *results.Result) []metav1.OwnerReference {
	var resolved []metav1.OwnerReference
	for _, ref := range req.OwnerReferences {
		record, found := records[ref.UID]
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		var reason string
		switch {
		case !found:
			reason = "parent missing or excluded from synchronous restore"
		case err != nil || record.GroupKind != (schema.GroupKind{Group: gv.Group, Kind: ref.Kind}) || record.Name != ref.Name:
			reason = "parent name or GroupKind changed"
		case req.Kind == "PersistentVolumeClaim" && req.Group == "" && ownerref.IsDeniedGroupKind(record.GroupKind):
			reason = "PVC parent is denied"
		case !record.ScopeKnown || !req.ScopeKnown:
			reason = "owner or child scope is unknown"
		case record.APIVersion == "":
			reason = "parent destination API version is unknown"
		case record.Namespaced && (!req.Namespaced || record.Namespace != req.Namespace):
			reason = "invalid owner scope or target namespace"
		}
		if reason != "" {
			warnings.Add(req.Namespace, fmt.Errorf("omitting ownerReference %s for %s/%s: %s", ownerIdentity(ref), req.Namespace, req.Name, reason))
			continue
		}
		ref = *ref.DeepCopy()
		ref.UID = record.LiveUID
		if ref.APIVersion != record.APIVersion && !slices.Contains(record.ServedAPIVersions, ref.APIVersion) {
			ref.APIVersion = record.APIVersion
		}
		resolved = append(resolved, ref)
	}
	return resolved
}

// ApplyOwnerRefRelinking freezes synchronous state and resolves every queued
// request, even when the child patch deadline has already expired.
func ApplyOwnerRefRelinking(ctx context.Context, c client.Client, restore *velerov1.Restore, state *ownerref.OwnerRefRelinkState) (results.Result, results.Result, []velerov1.PendingPatchRef) {
	warnings, errs := results.Result{}, results.Result{}
	if state == nil {
		return warnings, errs, nil
	}
	records, queue := state.Freeze()
	versions := make(ownerRefServedVersions)
	for _, record := range records {
		versions[schema.FromAPIVersionAndKind(record.APIVersion, record.GroupKind.Kind)] = true
		for _, apiVersion := range record.ServedAPIVersions {
			versions[schema.FromAPIVersionAndKind(apiVersion, record.GroupKind.Kind)] = true
		}
	}
	var pending []velerov1.PendingPatchRef
	for _, req := range queue {
		refs := resolveOwnerReferences(req, records, &warnings)
		if len(refs) == 0 {
			continue
		}
		pending = append(pending, velerov1.PendingPatchRef{
			Group: req.Group, Version: req.Version, Kind: req.Kind,
			Namespace: req.Namespace, Name: req.Name, RestoredUID: req.RestoredUID,
			OwnerReferences: refs,
		})
	}
	retryWarnings, retryErrors, remaining := applyPendingOwnerRefPatches(ctx, c, restore, pending, nil, versions)
	warnings.Merge(&retryWarnings)
	errs.Merge(&retryErrors)
	return warnings, errs, remaining
}

// IsRetriableOwnerRefError reports whether err represents a transient network, API server, or conflict condition.
func IsRetriableOwnerRefError(err error) bool {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	return kubenet.IsConnectionReset(err) || kubenet.IsConnectionRefused(err) || kubenet.IsProbableEOF(err) || kubenet.IsHTTP2ConnectionLost(err) ||
		apierrors.IsConflict(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) || apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsUnexpectedServerError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// RetryPendingPatches refreshes each distinct parent once for this Pass 2
// execution, then patches only pending children using the refreshed snapshot.
func RetryPendingPatches(ctx context.Context, c client.Client, restore *velerov1.Restore, pending []velerov1.PendingPatchRef) (results.Result, results.Result, []velerov1.PendingPatchRef) {
	warnings := results.Result{}
	refreshed, refreshErrors, versions := refreshPendingOwnerRefParents(ctx, c, pending, &warnings)
	patchWarnings, errs, remaining := applyPendingOwnerRefPatches(ctx, c, restore, refreshed, refreshErrors, versions)
	warnings.Merge(&patchWarnings)
	return warnings, errs, remaining
}

// applyPendingOwnerRefPatches performs no parent discovery. Pass 1 supplies its
// frozen resolutions; Pass 2 supplies its refreshed references and lookup errors.
func applyPendingOwnerRefPatches(ctx context.Context, c client.Client, restore *velerov1.Restore, pending []velerov1.PendingPatchRef, refreshErrors []error, versions ownerRefServedVersions) (results.Result, results.Result, []velerov1.PendingPatchRef) {
	warnings, errs := results.Result{}, results.Result{}
	var remaining []velerov1.PendingPatchRef
	for i, original := range pending {
		request := original
		request.OwnerReferences = ownerref.CopyReferences(original.OwnerReferences)
		var err error
		if refreshErrors != nil {
			err = refreshErrors[i]
		}
		if err == nil {
			if ctx.Err() != nil {
				remaining = append(remaining, request)
				continue
			}
			err = patchOwnerReferences(ctx, c, &request, &warnings, versions)
		}
		if err == nil {
			restore.Status.OwnerRefsRelinked++
		} else if isRetriableOwnerRefRequestError(err) {
			remaining = append(remaining, request)
			warnings.Add(request.Namespace, fmt.Errorf("ownerReference patch for %s/%s remains pending: %w", request.Namespace, request.Name, err))
		} else {
			errs.Add(request.Namespace, fmt.Errorf("ownerReference patch for %s/%s failed permanently: %w", request.Namespace, request.Name, err))
		}
	}
	return warnings, errs, remaining
}

func mergeOwnerReferences(live, incoming []metav1.OwnerReference, warn func(string), versions ownerRefServedVersions) []metav1.OwnerReference {
	merged := ownerref.CopyReferences(live)
	controller := ""
	for _, ref := range merged {
		if ref.Controller != nil && *ref.Controller {
			controller = ownerIdentity(ref)
			break
		}
	}
	for _, ref := range ownerref.CopyReferences(incoming) {
		identity := ownerIdentity(ref)
		if controller != "" && controller != identity && ref.Controller != nil && *ref.Controller {
			ref.Controller = nil
			warn(fmt.Sprintf("live controlling owner %s takes precedence over incoming %s", controller, identity))
		}
		matched := false
		for i := range merged {
			if ownerIdentity(merged[i]) == identity {
				if versions.contains(merged[i]) {
					ref.APIVersion = merged[i].APIVersion
				}
				if merged[i].Controller != nil && *merged[i].Controller {
					ref.Controller = merged[i].Controller
				}
				merged[i] = ref
				matched = true
				break
			}
		}
		if !matched {
			merged = append(merged, ref)
		}
		if ref.Controller != nil && *ref.Controller {
			controller = identity
		}
	}
	return merged
}

func patchOwnerReferences(ctx context.Context, c client.Client, req *velerov1.PendingPatchRef, warnings *results.Result, versions ownerRefServedVersions) error {
	seenWarnings := make(map[string]bool)
	warn := func(message string) {
		if !seenWarnings[message] {
			warnings.Add(req.Namespace, fmt.Errorf("%s/%s: %s", req.Namespace, req.Name, message))
			seenWarnings[message] = true
		}
	}
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: req.Group, Version: req.Version, Kind: req.Kind})
		if err := c.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, obj); err != nil {
			lastErr = err
			if IsRetriableOwnerRefError(err) {
				return false, nil
			}
			return false, err
		}
		if obj.GetUID() != req.RestoredUID {
			warn(fmt.Sprintf("relinking replacement child: originally created UID %s, current UID %s", req.RestoredUID, obj.GetUID()))
		}
		patch := func() error {
			refs := mergeOwnerReferences(obj.GetOwnerReferences(), req.OwnerReferences, warn, versions)
			payload, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": obj.GetResourceVersion(), "ownerReferences": refs}})
			if err != nil {
				return err
			}
			return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, payload))
		}
		lastErr = patch()
		if apierrors.IsForbidden(lastErr) {
			fallback := false
			for i := range req.OwnerReferences {
				ref := &req.OwnerReferences[i]
				if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
					fallback = true
				}
				ref.BlockOwnerDeletion = nil
			}
			if fallback {
				warn("retrying without incoming blockOwnerDeletion after Forbidden")
				lastErr = patch()
			}
		}
		if lastErr == nil {
			return true, nil
		}
		if IsRetriableOwnerRefError(lastErr) {
			return false, nil
		}
		return false, lastErr
	})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && lastErr != nil {
		return lastErr
	}
	return err
}
