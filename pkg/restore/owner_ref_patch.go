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
	"fmt"
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

// ApplyOwnerRefRemapping runs Phase 1B ownerRef patches then spec-ref remapping.
// Permanently missing parents are logged as warnings and omitted from patches without blocking restore.
// Failed patches due to transient API errors are returned in pendingPatches for Pass 2 safety net retry.
func ApplyOwnerRefRemapping(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restore *velerov1api.Restore,
	state *ownerref.OwnerRefRemapState,
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

	for _, req := range state.GetOwnerPatchQueue() {
		remapped, err := patchOwnerReferences(ctx, log, crClient, state, req, namespaceMapping)
		if err != nil {
			log.WithError(err).Warnf("Failed to patch ownerReferences for %s/%s", req.Namespace, req.Name)
			warnings.Add(req.Namespace, err)
			remainingQueue = append(remainingQueue, req)
			pendingPatches = append(pendingPatches, velerov1api.PendingPatchRef{
				Group:           req.Group,
				Version:         req.Version,
				Kind:            req.Kind,
				Namespace:       req.Namespace,
				Name:            req.Name,
				PatchType:       "ownerRef",
				OwnerReferences: remapped,
				Error:           err.Error(),
			})
		}
	}
	state.SetOwnerPatchQueue(remainingQueue)

	specWarnings, specPending := processSpecReferences(ctx, log, crClient, state, namespaceMapping)
	warnings.Merge(&specWarnings)
	pendingPatches = append(pendingPatches, specPending...)
	return warnings, pendingPatches
}

func patchOwnerReferences(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *ownerref.OwnerRefRemapState,
	req ownerref.OwnerPatchRequest,
	namespaceMapping map[string]string,
) ([]metav1.OwnerReference, error) {
	var remapped []metav1.OwnerReference
	childTargetNS := req.Namespace

	for i, ref := range req.OriginalOwnerRefs {
		newUID, ok := state.GetNewUID(ref.UID)
		if !ok {
			log.Warnf("Parent with old UID %s not found in restore for %s/%s; parent may have been excluded",
				ref.UID, req.Namespace, req.Name)
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
				log.Warnf("Skipping ownerRef %s/%s for %s/%s: namespace mapping splits owner/owned (%s != %s)",
					ref.Kind, ref.Name, req.Namespace, req.Name, ownerTargetNS, childTargetNS)
				continue
			}
		}

		ref.UID = newUID
		remapped = append(remapped, ref)
	}

	if len(remapped) == 0 {
		return nil, nil
	}

	if err := patchObjectOwnerRefs(ctx, crClient, req, remapped, log); err != nil {
		return remapped, err
	}

	return remapped, nil
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

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
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

// mergeOwnerReferences performs an additive merge: matching refs (Group, Kind, Name)
// have UID, Controller, and BlockOwnerDeletion updated; unmanaged live refs are preserved;
// new backup refs are appended. Enforces Kubernetes invariant that at most one ownerReference
// can have Controller: true using Live Precedence.
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
			out[idx].APIVersion = r.APIVersion
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
				p.Error = err.Error()
				remainingPending = append(remainingPending, p)
			} else {
				log.Infof("Pass 2 retry succeeded: patched ownerReferences on %s/%s", p.Namespace, p.Name)
			}

		case "specRef":
			err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				liveObj := &unstructured.Unstructured{}
				liveObj.SetGroupVersionKind(gvk)
				if err := crClient.Get(ctx, key, liveObj); err != nil {
					return fmt.Errorf("get %s for specRef retry: %w", key, err)
				}
				if p.SpecPatchJSON != "" {
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
				}
				return nil
			})
			if err != nil {
				log.WithError(err).Warnf("Pass 2 retry failed for specRef patch on %s/%s", p.Namespace, p.Name)
				warnings.Add(p.Namespace, err)
				p.Error = err.Error()
				remainingPending = append(remainingPending, p)
			} else {
				log.Infof("Pass 2 retry succeeded: patched spec references on %s/%s", p.Namespace, p.Name)
			}
		}
	}

	return warnings, remainingPending
}
