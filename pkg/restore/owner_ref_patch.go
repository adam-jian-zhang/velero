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
	"net/http"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubenet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/util/results"
)

// DefaultOwnerRefStageTimeout is the fallback timeout applied to an ownerReference
// relinking stage when no explicit deadline or resource timeout is provided.
const DefaultOwnerRefStageTimeout = 10 * time.Minute

// OwnerRefStageContext bounds ownerReference API work while retaining
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
		case (record.Scope != ownerref.ScopeNamespaced && record.Scope != ownerref.ScopeCluster) || (req.Scope != ownerref.ScopeNamespaced && req.Scope != ownerref.ScopeCluster):
			reason = "owner or child scope is unknown"
		case record.APIVersion == "":
			reason = "parent destination API version is unknown"
		case record.Scope == ownerref.ScopeNamespaced && (req.Scope != ownerref.ScopeNamespaced || record.Namespace != req.Namespace):
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

// ApplyOwnerRefRelinking resolves the completed synchronous UID map and patches
// eligible children once, with bounded in-place retries. Every unfinished request
// is a restore error; no work is deferred to the finalizer or persisted for replay.
func ApplyOwnerRefRelinking(ctx context.Context, c client.Client, restore *velerov1.Restore, state *ownerref.OwnerRefRelinkState) (results.Result, results.Result) {
	warnings, errs := results.Result{}, results.Result{}
	if state == nil {
		return warnings, errs
	}
	records, queue := state.Complete()
	versions := make(ownerRefServedVersions)
	for _, record := range records {
		versions[schema.FromAPIVersionAndKind(record.APIVersion, record.GroupKind.Kind)] = true
		for _, apiVersion := range record.ServedAPIVersions {
			versions[schema.FromAPIVersionAndKind(apiVersion, record.GroupKind.Kind)] = true
		}
	}
	unfinished := 0
	for _, req := range queue {
		// Resolve even after the API budget expires: omission-only children are
		// warnings, whereas children with resolvable unattempted work are errors.
		req.OwnerReferences = resolveOwnerReferences(req, records, &warnings)
		if len(req.OwnerReferences) == 0 {
			continue
		}
		err := ctx.Err()
		if err == nil {
			err = patchOwnerReferences(ctx, c, req, &warnings, versions)
		}
		if err != nil {
			unfinished++
			errs.Add(req.Namespace, fmt.Errorf("ownerReference relinking for %s %s/%s failed: %w", schema.GroupKind{Group: req.Group, Kind: req.Kind}, req.Namespace, req.Name, err))
			continue
		}
		restore.Status.OwnerRefsRelinked++
	}
	if unfinished > 0 {
		errs.AddVeleroError(fmt.Errorf("ownerReference relinking left %d child requests unfinished; no deferred retry is scheduled", unfinished))
	}
	return warnings, errs
}

// IsRetriableOwnerRefError reports whether err represents a transient network, API server, or conflict condition.
func IsRetriableOwnerRefError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	// UnexpectedServerResponse can also describe a proxy's non-JSON 4xx
	// rejection. A permanent HTTP status must win over that generic cause.
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		if code >= http.StatusBadRequest && code < http.StatusInternalServerError && code != http.StatusConflict && code != http.StatusTooManyRequests {
			return false
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	return kubenet.IsConnectionReset(err) || kubenet.IsConnectionRefused(err) || kubenet.IsProbableEOF(err) || kubenet.IsHTTP2ConnectionLost(err) ||
		apierrors.IsConflict(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) || apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsUnexpectedServerError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// mergeOwnerReferences preserves unrelated live entries. Matching logical owners
// use the resolved UID; a served live version and a live controlling owner win.
// For the same UID, explicit live flags (including false) win, while incoming
// flags fill nil values. A replaced UID uses incoming flags except that live
// controller:true remains controlling. Conflicting incoming controllers demote.
func mergeOwnerReferences(live, incoming []metav1.OwnerReference, warn func(string), versions ownerRefServedVersions) []metav1.OwnerReference {
	merged := ownerref.CopyReferences(live)
	controller := ""
	for _, ref := range merged {
		if ref.Controller != nil && *ref.Controller {
			controller = ownerIdentity(ref)
			break
		}
	}
	seen := make(map[string]bool)
	for _, ref := range ownerref.CopyReferences(incoming) {
		identity := ownerIdentity(ref)
		if seen[identity] {
			warn(fmt.Sprintf("ignoring duplicate incoming ownerReference %s; first resolved entry takes precedence", identity))
			continue
		}
		seen[identity] = true
		match := slices.IndexFunc(merged, func(current metav1.OwnerReference) bool {
			return ownerIdentity(current) == identity
		})
		if match >= 0 {
			current := merged[match]
			if versions.contains(current) {
				ref.APIVersion = current.APIVersion
			}
			if current.UID == ref.UID {
				if current.Controller != nil {
					ref.Controller = current.Controller
				}
				if current.BlockOwnerDeletion != nil {
					ref.BlockOwnerDeletion = current.BlockOwnerDeletion
				}
			} else if current.Controller != nil && *current.Controller {
				ref.Controller = current.Controller
			}
		}
		if controller != "" && controller != identity && ref.Controller != nil && *ref.Controller {
			controlling := false
			ref.Controller = &controlling
			warn(fmt.Sprintf("controlling owner %s takes precedence over incoming %s; demoting incoming controller flag", controller, identity))
		}
		if match >= 0 {
			merged[match] = ref
		} else {
			merged = append(merged, ref)
		}
		if ref.Controller != nil && *ref.Controller {
			controller = identity
		}
	}
	return merged
}

// patchOwnerReferences uses client-go's DefaultBackoff: four GET/merge/PATCH
// attempts, starting at 10ms with factor 5 and 10% jitter. Retry-After is a
// minimum delay, but never extends the shared stage deadline. Forbidden is
// permanent: ownership flags are never weakened in response to an API error.
func patchOwnerReferences(ctx context.Context, c client.Client, req ownerref.OwnerPatchRequest, warnings *results.Result, versions ownerRefServedVersions) error {
	seenWarnings := make(map[string]bool)
	warn := func(message string) {
		if !seenWarnings[message] {
			warnings.Add(req.Namespace, fmt.Errorf("%s/%s: %s", req.Namespace, req.Name, message))
			seenWarnings[message] = true
		}
	}
	backoff := retry.DefaultBackoff
	attempts := backoff.Steps
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			delay := backoff.Step()
			if seconds, ok := apierrors.SuggestsClientDelay(lastErr); ok {
				delay = max(delay, time.Duration(seconds)*time.Second)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: req.Group, Version: req.Version, Kind: req.Kind})
		lastErr = c.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, obj)
		if lastErr == nil {
			if obj.GetUID() != req.RestoredUID {
				warn(fmt.Sprintf("relinking replacement child: originally created UID %s, current UID %s", req.RestoredUID, obj.GetUID()))
			}
			refs := mergeOwnerReferences(obj.GetOwnerReferences(), req.OwnerReferences, warn, versions)
			payload, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": obj.GetResourceVersion(), "ownerReferences": refs}})
			if err != nil {
				return err
			}
			lastErr = c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, payload))
		}
		if lastErr == nil {
			return nil
		}
		if !IsRetriableOwnerRefError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("retry limit reached after %d attempts: %w", attempts, lastErr)
}
