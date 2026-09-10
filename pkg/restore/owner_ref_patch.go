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
	"net"
	"reflect"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

const (
	metadataKey        = "metadata"
	annotationsKey     = "annotations"
	resourceVersionKey = "resourceVersion"
	ownerReferencesKey = "ownerReferences"
	uidKey             = "uid"
	namespaceKey       = "namespace"
)

// isRetriablePatchError determines whether an API patch failure is transient and eligible
// for Pass 2 retry in RestoreFinalizerController. Non-retriable errors (HTTP 400, 404, 422,
// webhook schema/immutability rejections) are filtered out to prevent etcd status bloat.
func isRetriablePatchError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsConflict(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsInternalError(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	return false
}

// ApplyOwnerRefRelinking runs Phase 1B ownerRef patches then spec-ref relinking.
// Permanently missing parents are logged as warnings and omitted from patches without blocking restore.
// Failed patches due to transient API errors are returned in pendingPatches for Pass 2 safety net retry.
func ApplyOwnerRefRelinking(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restore *velerov1api.Restore,
	state *ownerref.OwnerRefRelinkState,
) (results.Result, []velerov1api.PendingPatchRef) {
	warnings := results.Result{}
	if state == nil || !state.Enabled || crClient == nil {
		return warnings, nil
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	var namespaceMapping map[string]string
	if restore != nil {
		namespaceMapping = restore.Spec.NamespaceMapping
	}

	var pendingPatches []velerov1api.PendingPatchRef
	var remainingQueue []ownerref.OwnerPatchRequest
	ownerRefsRelinked := 0

	for _, req := range state.GetOwnerPatchQueue() {
		if ctx.Err() != nil {
			// Context expired (e.g. Pass 1 hit resourceTimeout): fast-fail remaining items without network calls
			remapped := remapOwnerReferencesInMemory(state, req, namespaceMapping, log)
			quiescedRoots := state.ResolveQuiescedRoots(req)
			if len(remapped) > 0 {
				remainingQueue = append(remainingQueue, req)
				pendingPatches = append(pendingPatches, velerov1api.PendingPatchRef{
					Group:           req.Group,
					Version:         req.Version,
					Kind:            req.Kind,
					Namespace:       req.Namespace,
					Name:            req.Name,
					PatchType:       "ownerRef",
					OwnerReferences: remapped,
					Targets:         quiescedRoots,
				})
			}
			warnings.Add(req.Namespace, ctx.Err())
			continue
		}

		remapped, err := patchOwnerReferences(ctx, log, crClient, state, req, namespaceMapping)
		if err != nil {
			log.WithError(err).Warnf("Failed to patch ownerReferences for %s/%s", req.Namespace, req.Name)
			warnings.Add(req.Namespace, err)
			quiescedRoots := state.ResolveQuiescedRoots(req)
			if isRetriablePatchError(err) {
				remainingQueue = append(remainingQueue, req)
				pendingPatches = append(pendingPatches, velerov1api.PendingPatchRef{
					Group:           req.Group,
					Version:         req.Version,
					Kind:            req.Kind,
					Namespace:       req.Namespace,
					Name:            req.Name,
					PatchType:       "ownerRef",
					OwnerReferences: remapped,
					Targets:         quiescedRoots,
				})
			} else {
				// Non-retriable error: do NOT enqueue to pendingPatches or remainingQueue (prevents etcd bloat).
				// Instead, mark immediate owner parents and transitive quiesced root(s) as unquiesceBlocked so they remain safely paused in etcd.
				for _, ref := range remapped {
					ownerGroup := ownerRefGroup(ref.APIVersion)
					state.BlockUnquiesceForTarget(ownerGroup, ref.Kind, req.Namespace, ref.Name)
				}
				for _, root := range quiescedRoots {
					state.BlockUnquiesceForTarget(root.Group, root.Kind, root.Namespace, root.Name)
				}
			}
			continue
		}
		if len(remapped) > 0 {
			ownerRefsRelinked++
		}
	}
	state.SetOwnerPatchQueue(remainingQueue)

	specWarnings, specPending, specRefsRelinked := processSpecReferences(ctx, log, crClient, state, namespaceMapping)
	warnings.Merge(&specWarnings)
	pendingPatches = append(pendingPatches, specPending...)
	if restore != nil {
		restore.Status.OwnerRefsRelinked += ownerRefsRelinked
		restore.Status.SpecRefsRelinked += specRefsRelinked
	}
	return warnings, pendingPatches
}

// ApplyOwnerRefRemapping is a backward-compatible alias for ApplyOwnerRefRelinking.
func ApplyOwnerRefRemapping(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restore *velerov1api.Restore,
	state *ownerref.OwnerRefRelinkState,
) (results.Result, []velerov1api.PendingPatchRef) {
	return ApplyOwnerRefRelinking(ctx, log, crClient, restore, state)
}

func patchOwnerReferences(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *ownerref.OwnerRefRelinkState,
	req ownerref.OwnerPatchRequest,
	namespaceMapping map[string]string,
) ([]metav1.OwnerReference, error) {
	remapped := remapOwnerReferencesInMemory(state, req, namespaceMapping, log)
	if len(remapped) == 0 {
		return nil, nil
	}

	if err := patchObjectOwnerRefs(ctx, crClient, req, remapped, log); err != nil {
		return remapped, err
	}

	return remapped, nil
}

func remapOwnerReferencesInMemory(
	state *ownerref.OwnerRefRelinkState,
	req ownerref.OwnerPatchRequest,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) []metav1.OwnerReference {
	var remapped []metav1.OwnerReference
	childTargetNS := req.Namespace

	for i, ref := range req.OriginalOwnerRefs {
		newUID, ok := state.GetNewUID(ref.UID)
		if !ok {
			if log != nil {
				log.Warnf("Parent with old UID %s not found in restore for %s/%s; parent may have been excluded",
					ref.UID, req.Namespace, req.Name)
			}
			// Permanent omission: do not re-enqueue for missing parent
			continue
		}

		ownerSrcNS := ""
		if i < len(req.OwnerRefSourceNS) {
			ownerSrcNS = req.OwnerRefSourceNS[i]
		}
		if ownerSrcNS != "" {
			ownerTargetNS := mapNamespace(ownerSrcNS, namespaceMapping)
			if ownerTargetNS != childTargetNS {
				if log != nil {
					log.Warnf("Skipping ownerRef %s/%s for %s/%s: namespace mapping splits owner/owned (%s != %s)",
						ref.Kind, ref.Name, req.Namespace, req.Name, ownerTargetNS, childTargetNS)
				}
				continue
			}
		}

		ref.UID = newUID
		remapped = append(remapped, ref)
	}

	return remapped
}

func mapNamespace(ns string, mapping map[string]string) string {
	if ns == "" {
		return ""
	}
	if mapping != nil {
		if mapped, ok := mapping[ns]; ok {
			return mapped
		}
	}
	return ns
}

func patchObjectOwnerRefs(
	ctx context.Context,
	crClient client.Client,
	req ownerref.OwnerPatchRequest,
	remapped []metav1.OwnerReference,
	log logrus.FieldLogger,
) error {
	if crClient == nil {
		return fmt.Errorf("nil client")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return retry.OnError(retry.DefaultBackoff, isRetriablePatchError, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   req.Group,
			Version: req.Version,
			Kind:    req.Kind,
		})

		key := client.ObjectKey{Namespace: req.Namespace, Name: req.Name}
		if err := crClient.Get(ctx, key, obj); err != nil {
			return fmt.Errorf("get %s/%s %s/%s for ownerRef patch: %w", req.Group, req.Version, req.Namespace, req.Name, err)
		}

		// Additive union/merge: do not clobber live ownerReferences on adopted objects, enforce Live Precedence
		mergedRefs := mergeOwnerReferences(obj.GetOwnerReferences(), remapped, log, req.Namespace, req.Name)
		if reflect.DeepEqual(obj.GetOwnerReferences(), mergedRefs) {
			return nil
		}

		patchBytes, err := generateOwnerRefMergePatch(obj.GetResourceVersion(), mergedRefs)
		if err != nil {
			return fmt.Errorf("marshal ownerRef patch for %s/%s: %w", req.Namespace, req.Name, err)
		}

		err = crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchBytes))
		if err != nil && apierrors.IsForbidden(err) {
			// Permission fallback: OwnerReferencesPermissionEnforcement requires 'delete' on owner
			// to set blockOwnerDeletion: true. If Velero lacks this permission, strip it and retry:
			hasBlockOwnerDeletion := false
			for i := range mergedRefs {
				if mergedRefs[i].BlockOwnerDeletion != nil && *mergedRefs[i].BlockOwnerDeletion {
					mergedRefs[i].BlockOwnerDeletion = nil
					hasBlockOwnerDeletion = true
				}
			}
			if hasBlockOwnerDeletion {
				// Also strip from remapped so any subsequent conflict retry doesn't re-introduce blockOwnerDeletion: true!
				for i := range remapped {
					remapped[i].BlockOwnerDeletion = nil
				}
				if log != nil {
					log.Warnf("Stripped blockOwnerDeletion: true on ownerRef for %s/%s due to RBAC restrictions (requires delete on owner); retrying patch", req.Namespace, req.Name)
				}
				fallbackPatchBytes, fallbackErr := generateOwnerRefMergePatch(obj.GetResourceVersion(), mergedRefs)
				if fallbackErr == nil {
					return crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, fallbackPatchBytes))
				}
			}
		}
		return err
	})
}

func ownerRefGroup(apiVersion string) string {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return apiVersion
	}
	return gv.Group
}

// mergeOwnerReferences performs an additive merge: matching refs (Group, Kind, Name;
// version suffix ignored) have UID, Controller, and BlockOwnerDeletion updated while
// the live APIVersion is preserved; unmanaged live refs are kept; new backup refs are appended.
//
// Pre-existing live objects (existingResourcePolicy: update, skip, SA merge) are never enqueued
// for remapping. Live Precedence specifically resolves controller claim races where an active
// in-cluster controller claims a newly created child (setting controller: true) between Phase 1A
// create and Phase 1B patch, or during in-place PVC volume restore where the PVC was already
// adopted. The live controller: true is preserved, and incoming restored references are demoted
// to controller: nil to prevent HTTP 422 validation rejections.
func mergeOwnerReferences(
	live, remapped []metav1.OwnerReference,
	log logrus.FieldLogger,
	objNamespace, objName string,
) []metav1.OwnerReference {
	out := append([]metav1.OwnerReference(nil), live...)

	// Check if live object already has a controlling owner
	liveControllingOwner := ""
	for _, l := range live {
		if l.Controller != nil && *l.Controller {
			liveControllingOwner = fmt.Sprintf("%s/%s", l.Kind, l.Name)
			break
		}
	}

	for _, r := range remapped {
		idx := -1
		rGroup := ownerRefGroup(r.APIVersion)
		for i, l := range out {
			if ownerRefGroup(l.APIVersion) == rGroup && l.Kind == r.Kind && l.Name == r.Name {
				idx = i
				break
			}
		}
		if idx >= 0 {
			// Match on Group (parsed from APIVersion) + Kind + Name. Keep the live
			// APIVersion so a CAPI v1alpha4 backup ref vs v1beta1 live ref updates UID
			// without duplicating the owner or writing an unserved version.
			out[idx].UID = r.UID
			out[idx].BlockOwnerDeletion = r.BlockOwnerDeletion
			// If live was already controller, preserve it; otherwise take incoming only if no other live controller exists
			if out[idx].Controller == nil || !*out[idx].Controller {
				if r.Controller != nil && *r.Controller {
					if liveControllingOwner != "" {
						if log != nil {
							log.Warnf("Live object %s/%s already has controlling owner %s; incoming parent %s/%s demoted to non-controlling ownerRef to preserve API server invariant",
								objNamespace, objName, liveControllingOwner, r.Kind, r.Name)
						}
					} else {
						out[idx].Controller = r.Controller
						liveControllingOwner = fmt.Sprintf("%s/%s", r.Kind, r.Name)
					}
				} else {
					out[idx].Controller = r.Controller
				}
			}
		} else {
			// New incoming reference: enforce controller uniqueness (Live Precedence)
			if r.Controller != nil && *r.Controller {
				if liveControllingOwner != "" {
					if log != nil {
						log.Warnf("Live object %s/%s already has controlling owner %s; incoming parent %s/%s demoted to non-controlling ownerRef to preserve API server invariant",
							objNamespace, objName, liveControllingOwner, r.Kind, r.Name)
					}
					r.Controller = nil
				} else {
					liveControllingOwner = fmt.Sprintf("%s/%s", r.Kind, r.Name)
				}
			}
			out = append(out, r)
		}
	}
	return out
}

func generateOwnerRefMergePatch(resourceVersion string, refs []metav1.OwnerReference) ([]byte, error) {
	metadata := map[string]any{
		ownerReferencesKey: refs,
	}
	if resourceVersion != "" {
		metadata[resourceVersionKey] = resourceVersion
	}
	patchObj := map[string]any{
		metadataKey: metadata,
	}
	return json.Marshal(patchObj)
}

// RetryPendingPatches retries transiently failed ownerRef and specRef patches in Pass 2 (RestoreFinalizerController).
// It fetches fresh live objects from the API server and reapplies the patches using retry.RetryOnConflict.
func RetryPendingPatches(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restore *velerov1api.Restore,
	pending []velerov1api.PendingPatchRef,
) (results.Result, []velerov1api.PendingPatchRef) {
	warnings := results.Result{}
	if len(pending) == 0 || crClient == nil {
		return warnings, nil
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	var remainingPending []velerov1api.PendingPatchRef

	for _, p := range pending {
		if ctx.Err() != nil {
			log.WithError(ctx.Err()).Warnf("Pass 2 retry timed out/canceled for %s/%s", p.Namespace, p.Name)
			warnings.Add(p.Namespace, ctx.Err())
			remainingPending = append(remainingPending, p)
			continue
		}

		gvk := schema.GroupVersionKind{Group: p.Group, Version: p.Version, Kind: p.Kind}
		key := client.ObjectKey{Namespace: p.Namespace, Name: p.Name}

		switch p.PatchType {
		case "ownerRef", "": // default to ownerRef for backward compatibility
			req := ownerref.OwnerPatchRequest{
				Group:     p.Group,
				Version:   p.Version,
				Kind:      p.Kind,
				Namespace: p.Namespace,
				Name:      p.Name,
			}
			if err := patchObjectOwnerRefs(ctx, crClient, req, p.OwnerReferences, log); err != nil {
				log.WithError(err).Warnf("Pass 2 retry failed for ownerRef patch on %s/%s", p.Namespace, p.Name)
				warnings.Add(p.Namespace, err)
				remainingPending = append(remainingPending, p)
			} else {
				log.Infof("Pass 2 retry succeeded: patched ownerReferences on %s/%s", p.Namespace, p.Name)
				if restore != nil {
					// Increment only on Pass 2 success. Safe from double-counting because
					// PendingOwnerRefPatches only holds items that FAILED Pass 1 (so they were not counted
					// in restore.Status.OwnerRefsRelinked during Pass 1). A successful retry removes the
					// item from the pending list, so it cannot be counted again on a later reconcile.
					// If this invariant ever changes (e.g. re-queueing successful items), this counter
					// would double-count and must be revisited.
					restore.Status.OwnerRefsRelinked++
				}
			}

		case "specRef":
			if p.SpecPatchJSON == "" {
				err := fmt.Errorf("empty SpecPatchJSON for specRef retry on %s/%s", p.Namespace, p.Name)
				log.WithError(err).Warnf("Pass 2 retry failed for specRef patch on %s/%s", p.Namespace, p.Name)
				warnings.Add(p.Namespace, err)
				remainingPending = append(remainingPending, p)
				continue
			}
			err := retry.OnError(retry.DefaultBackoff, isRetriablePatchError, func() error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				liveObj := &unstructured.Unstructured{}
				liveObj.SetGroupVersionKind(gvk)
				if err := crClient.Get(ctx, key, liveObj); err != nil {
					return fmt.Errorf("get %s for specRef retry: %w", key, err)
				}
				patchBytes := []byte(p.SpecPatchJSON)
				// If patch payload contains a stale metadata.resourceVersion, update it to liveObj's current RV to avoid 409 conflict
				var patchMap map[string]any
				if err := json.Unmarshal(patchBytes, &patchMap); err == nil {
					if meta, ok := patchMap[metadataKey].(map[string]any); ok {
						if _, hasRV := meta[resourceVersionKey]; hasRV {
							if rv := liveObj.GetResourceVersion(); rv != "" {
								meta[resourceVersionKey] = rv
							} else {
								delete(meta, resourceVersionKey)
							}
							if updatedBytes, err := json.Marshal(patchMap); err == nil {
								patchBytes = updatedBytes
							}
						}
					}
				}
				return crClient.Patch(ctx, liveObj, client.RawPatch(types.MergePatchType, patchBytes))
			})
			if err != nil {
				log.WithError(err).Warnf("Pass 2 retry failed for specRef patch on %s/%s", p.Namespace, p.Name)
				warnings.Add(p.Namespace, err)
				remainingPending = append(remainingPending, p)
			} else {
				log.Infof("Pass 2 retry succeeded: patched spec references on %s/%s", p.Namespace, p.Name)
				if restore != nil {
					// Same double-counting invariant as the ownerRef branch above: only failed Pass 1
					// items are retried, and a successful retry removes the item from the pending list.
					restore.Status.SpecRefsRelinked++
				}
			}
		}
	}

	return warnings, remainingPending
}
