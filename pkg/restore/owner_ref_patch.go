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

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/label"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

const (
	metadataKey        = "metadata"
	labelsKey          = "labels"
	annotationsKey     = "annotations"
	ownerReferencesKey = "ownerReferences"
)

// ApplyOwnerRefRelinking applies Phase 1B ownerReference merge-patches to restored dependent resources.
func ApplyOwnerRefRelinking(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *OwnerRefRemapState,
) results.Result {
	warnings := results.Result{}
	if state == nil || !state.Enabled || crClient == nil {
		return warnings
	}

	state.ownerPatchQueueMu.Lock()
	queue := append([]OwnerPatchRequest(nil), state.OwnerPatchQueue...)
	state.ownerPatchQueueMu.Unlock()

	for _, req := range queue {
		if err := patchSingleChildOwnerRefs(ctx, log, crClient, state, req); err != nil {
			if log != nil {
				log.WithError(err).Warnf("Failed to patch ownerReferences for %s/%s", req.Namespace, req.Name)
			}
			warnings.Add(req.Namespace, err)
		}
	}

	return warnings
}

func patchSingleChildOwnerRefs(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *OwnerRefRemapState,
	req OwnerPatchRequest,
) error {
	var remapped []metav1.OwnerReference

	state.uidMapLock.RLock()
	childTargetNS := req.Namespace
	seenUIDs := make(map[types.UID]struct{})

	for _, ref := range req.OriginalOwnerRefs {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			if log != nil {
				log.Warnf("Skipping malformed parent ownerReference apiVersion %q for %s/%s",
					ref.APIVersion, req.Namespace, req.Name)
			}
			continue
		}
		if IsDeniedGroupKind(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}) {
			if log != nil {
				log.Warnf("Skipping deny-listed parent ownerReference %s/%s for %s/%s",
					ref.Kind, ref.Name, req.Namespace, req.Name)
			}
			continue
		}

		mapped, ok := state.UIDMap[ref.UID]
		if !ok {
			if log != nil {
				log.Warnf("Skipping unmapped ownerReference %s/%s for %s/%s",
					ref.Kind, ref.Name, req.Namespace, req.Name)
			}
			continue
		}

		// A cluster-scoped parent has an empty namespace and may own a namespaced child.
		// A namespaced parent must live in the child's namespace or GC will delete the child.
		if mapped.Namespace != "" && mapped.Namespace != childTargetNS {
			if log != nil {
				log.Warnf("Skipping ownerRef %s/%s for %s/%s: namespace mapping splits owner/owned",
					ref.Kind, ref.Name, req.Namespace, req.Name)
			}
			continue
		}

		if _, seen := seenUIDs[mapped.UID]; seen {
			if log != nil {
				log.Warnf("Skipping duplicate parent ownerReference UID %q (%s/%s) for %s/%s",
					mapped.UID, ref.Kind, ref.Name, req.Namespace, req.Name)
			}
			continue
		}
		seenUIDs[mapped.UID] = struct{}{}

		refCopy := ref
		refCopy.UID = mapped.UID
		remapped = append(remapped, refCopy)
	}
	state.uidMapLock.RUnlock()

	if len(remapped) == 0 {
		return nil
	}

	return patchMergedOwnerRefs(ctx, log, crClient, req, remapped)
}

func generateOwnerRefMergePatch(remapped []metav1.OwnerReference) ([]byte, error) {
	patchObj := map[string]any{
		metadataKey: map[string]any{
			ownerReferencesKey: remapped,
		},
	}
	return json.Marshal(patchObj)
}

// patchMergedOwnerRefs GETs the live child, merges owner references, and patches
// under RetryOnConflict. A single HTTP 403 caused by blockOwnerDeletion is retried
// once with that field cleared. A second 403 is returned to the caller.
func patchMergedOwnerRefs(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	req OwnerPatchRequest,
	remapped []metav1.OwnerReference,
) error {
	stripped := false
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := applyOwnerRefMerge(ctx, log, crClient, req, remapped)
		if err == nil || stripped || !apierrors.IsForbidden(err) || !refsBlockOwnerDeletion(remapped) {
			return err
		}
		stripped = true
		clearBlockOwnerDeletion(remapped)
		if log != nil {
			log.Warnf("Stripped blockOwnerDeletion: true on ownerRef for %s/%s due to RBAC restrictions (requires delete on owner); proceeding with UID and Controller relinking", req.Namespace, req.Name)
		}
		return applyOwnerRefMerge(ctx, log, crClient, req, remapped)
	})
}

func applyOwnerRefMerge(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	req OwnerPatchRequest,
	remapped []metav1.OwnerReference,
) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   req.Group,
		Version: req.Version,
		Kind:    req.Kind,
	})
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, live); err != nil {
		return fmt.Errorf("failed to get %s/%s for ownerRef merge: %w", req.Namespace, req.Name, err)
	}

	merged := mergeOwnerReferences(log, req, live.GetOwnerReferences(), remapped)
	patchBytes, err := generateOwnerRefMergePatch(merged)
	if err != nil {
		return fmt.Errorf("failed to marshal ownerRef patch for %s/%s: %w", req.Namespace, req.Name, err)
	}

	targetObj := &unstructured.Unstructured{}
	targetObj.SetGroupVersionKind(live.GroupVersionKind())
	targetObj.SetNamespace(req.Namespace)
	targetObj.SetName(req.Name)
	return crClient.Patch(ctx, targetObj, client.RawPatch(types.MergePatchType, patchBytes))
}

type ownerRefKey struct {
	group string
	kind  string
	name  string
}

func ownerRefGroup(apiVersion string) string {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return ""
	}
	return gv.Group
}

// mergeOwnerReferences combines live and restored owner references.
// A live ref matches a restored ref by group, kind, and name; apiVersion's version is ignored.
// The live apiVersion is kept. Unmatched live refs are preserved. Unmatched restored refs are appended.
// If any live ref is already controller: true, that ref stays controlling and restored refs are not.
func mergeOwnerReferences(log logrus.FieldLogger, req OwnerPatchRequest, live, restored []metav1.OwnerReference) []metav1.OwnerReference {
	liveControllerKind, liveControllerName := "", ""
	liveHasController := false
	for _, ref := range live {
		if ref.Controller != nil && *ref.Controller {
			liveHasController = true
			liveControllerKind = ref.Kind
			liveControllerName = ref.Name
			break
		}
	}

	restoredByKey := make(map[ownerRefKey]metav1.OwnerReference, len(restored))
	var restoredOrder []ownerRefKey
	for _, ref := range restored {
		key := ownerRefKey{group: ownerRefGroup(ref.APIVersion), kind: ref.Kind, name: ref.Name}
		if _, ok := restoredByKey[key]; ok {
			continue
		}
		restoredByKey[key] = ref
		restoredOrder = append(restoredOrder, key)
	}

	used := make(map[ownerRefKey]bool, len(restoredByKey))
	merged := make([]metav1.OwnerReference, 0, len(live)+len(restored))
	for _, liveRef := range live {
		key := ownerRefKey{group: ownerRefGroup(liveRef.APIVersion), kind: liveRef.Kind, name: liveRef.Name}
		restoredRef, ok := restoredByKey[key]
		if !ok {
			merged = append(merged, liveRef)
			continue
		}
		used[key] = true
		updated := liveRef
		updated.UID = restoredRef.UID
		updated.BlockOwnerDeletion = restoredRef.BlockOwnerDeletion
		updated.Controller = controllerForMerge(log, req, liveHasController, liveRef, restoredRef, liveControllerKind, liveControllerName)
		merged = append(merged, updated)
	}
	for _, key := range restoredOrder {
		if used[key] {
			continue
		}
		ref := restoredByKey[key]
		ref.Controller = controllerForMerge(log, req, liveHasController, metav1.OwnerReference{}, ref, liveControllerKind, liveControllerName)
		merged = append(merged, ref)
	}
	return merged
}

func controllerForMerge(log logrus.FieldLogger, req OwnerPatchRequest, liveHasController bool, liveRef, restoredRef metav1.OwnerReference, liveControllerKind, liveControllerName string) *bool {
	if !liveHasController {
		return restoredRef.Controller
	}
	if liveRef.Controller != nil && *liveRef.Controller {
		return liveRef.Controller
	}
	if restoredRef.Controller != nil && *restoredRef.Controller && log != nil {
		log.Warnf("Live object %s/%s already has controlling owner %s/%s; incoming restored parent %s/%s demoted to non-controlling ownerRef to preserve API server invariant",
			req.Namespace, req.Name, liveControllerKind, liveControllerName, restoredRef.Kind, restoredRef.Name)
	}
	return nil
}

func refsBlockOwnerDeletion(refs []metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
			return true
		}
	}
	return false
}

func clearBlockOwnerDeletion(refs []metav1.OwnerReference) {
	for i := range refs {
		refs[i].BlockOwnerDeletion = nil
	}
}

// quiesceRuleVersion returns the API version used to list objects for a quiesce rule.
// rule.Version is used when set. Otherwise the REST mapper resolves it.
// There is no default version: a mapper failure is returned to the caller.
func quiesceRuleVersion(crClient client.Client, rule QuiesceRule) (string, error) {
	if rule.Version != "" {
		return rule.Version, nil
	}
	if crClient == nil || crClient.RESTMapper() == nil {
		return "", fmt.Errorf("failed to resolve API version for quiesce rule %s/%s: REST mapper is unavailable", rule.Group, rule.Kind)
	}
	mapping, err := crClient.RESTMapper().RESTMapping(schema.GroupKind{Group: rule.Group, Kind: rule.Kind})
	if err != nil {
		return "", fmt.Errorf("failed to resolve API version for quiesce rule %s/%s: %w", rule.Group, rule.Kind, err)
	}
	if mapping == nil || mapping.GroupVersionKind.Version == "" {
		return "", fmt.Errorf("failed to resolve API version for quiesce rule %s/%s: REST mapping returned an empty version", rule.Group, rule.Kind)
	}
	return mapping.GroupVersionKind.Version, nil
}

// UnquiesceObjects finds all live objects tagged with velerov1api.QuiescedByRestoreLabel and removes the pause annotation and label.
func UnquiesceObjects(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restoreName string,
	rules ...QuiesceRule,
) results.Result {
	warnings := results.Result{}
	if crClient == nil || restoreName == "" || len(rules) == 0 {
		return warnings
	}

	matchingLabels := client.MatchingLabels{velerov1api.QuiescedByRestoreLabel: label.GetValidName(restoreName)}

	for _, rule := range rules {
		version, err := quiesceRuleVersion(crClient, rule)
		if err != nil {
			if log != nil {
				log.WithError(err).Warnf("Skipping unquiesce for %s/%s", rule.Group, rule.Kind)
			}
			warnings.Add("", err)
			continue
		}

		gvkList := schema.GroupVersionKind{
			Group:   rule.Group,
			Version: version,
			Kind:    rule.Kind + "List",
		}

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvkList)

		if err := crClient.List(ctx, list, matchingLabels); err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				if log != nil {
					log.Debugf("CRD for %s (%s) not found in cluster, skipping unquiesce", rule.Kind, rule.Group)
				}
				continue
			}
			if log != nil {
				log.WithError(err).Warnf("Failed to list quiesced %s for unquiesce", rule.Kind)
			}
			warnings.Add("", fmt.Errorf("failed to list quiesced %s for unquiesce: %w", rule.Kind, err))
			continue
		}

		metadata := map[string]any{
			labelsKey: map[string]any{
				velerov1api.QuiescedByRestoreLabel: nil,
			},
		}
		annotations := map[string]any{
			velerov1api.QuiescedKeyAnnotation: nil,
		}
		if rule.AnnotationKey != "" {
			annotations[rule.AnnotationKey] = nil
		}
		metadata[annotationsKey] = annotations
		patchObj := map[string]any{
			metadataKey: metadata,
		}
		unquiescePatch, err := json.Marshal(patchObj)
		if err != nil {
			if log != nil {
				log.WithError(err).Warnf("Failed to marshal unquiesce patch for %s", rule.Kind)
			}
			warnings.Add("", fmt.Errorf("failed to marshal unquiesce patch for %s: %w", rule.Kind, err))
			continue
		}

		for _, item := range list.Items {
			patchTarget := &unstructured.Unstructured{}
			itemVersion := item.GroupVersionKind().Version
			if itemVersion == "" {
				itemVersion = version
			}
			patchTarget.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   rule.Group,
				Version: itemVersion,
				Kind:    rule.Kind,
			})
			patchTarget.SetNamespace(item.GetNamespace())
			patchTarget.SetName(item.GetName())

			if err := crClient.Patch(ctx, patchTarget, client.RawPatch(types.MergePatchType, unquiescePatch)); err != nil {
				if log != nil {
					log.WithError(err).Warnf("Failed to unquiesce %s %s/%s", rule.Kind, item.GetNamespace(), item.GetName())
				}
				warnings.Add(item.GetNamespace(), err)
			} else {
				if log != nil {
					log.Infof("Successfully unquiesced %s %s/%s", rule.Kind, item.GetNamespace(), item.GetName())
				}
			}
		}
	}

	return warnings
}
