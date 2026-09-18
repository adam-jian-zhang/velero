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
	"strings"

	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/label"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

const (
	// LabelQuiescedByRestore tags live resources paused by Velero during restore.
	LabelQuiescedByRestore = "velero.io/quiesced-by-restore"
	// AnnotationQuiescedKey records the pause key (annotation or spec field) that was injected.
	AnnotationQuiescedKey = "velero.io/quiesced-key"
)

// InjectQuiesceMetadata injects a pause annotation and live tracking label on create.
// If the object was already paused in the backup, no changes are made and injected is false.
// Already-paused objects must not be recorded in Restore.Status.QuiescedObjects.
func InjectQuiesceMetadata(
	obj *unstructured.Unstructured,
	rule ownerref.QuiesceRule,
	restoreName string,
	log logrus.FieldLogger,
) (velerov1api.QuiescedObjectRef, bool) {
	if obj == nil || strings.TrimSpace(rule.AnnotationKey) == "" {
		return velerov1api.QuiescedObjectRef{}, false
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	if _, alreadyPaused := annotations[rule.AnnotationKey]; alreadyPaused {
		return velerov1api.QuiescedObjectRef{}, false
	}

	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	record := velerov1api.QuiescedObjectRef{
		Group:         obj.GroupVersionKind().Group,
		Version:       obj.GroupVersionKind().Version,
		Kind:          obj.GroupVersionKind().Kind,
		Namespace:     obj.GetNamespace(),
		Name:          obj.GetName(),
		AnnotationKey: rule.AnnotationKey,
	}

	annotations[rule.AnnotationKey] = rule.AnnotationValue
	annotations[AnnotationQuiescedKey] = rule.AnnotationKey
	obj.SetAnnotations(annotations)

	if restoreName != "" {
		validRestoreName := label.GetValidName(restoreName)
		labels[LabelQuiescedByRestore] = validRestoreName
		obj.SetLabels(labels)
	}

	if log != nil {
		log.Infof("Auto-quiesced %s/%s via annotation %s=%q with tracking label %s=%q for restore",
			obj.GetNamespace(), obj.GetName(), rule.AnnotationKey, rule.AnnotationValue, LabelQuiescedByRestore, label.GetValidName(restoreName))
	}
	return record, true
}

// CanUnquiesce evaluates whether a quiesced object Q is eligible to be unquiesced.
// An object Q can be unquiesced if and only if:
//  1. Q.UnquiesceBlocked is false (not blocked by a non-retriable child patch failure), AND
//  2. NO remaining patch request in pendingPatches:
//     a. targets Q directly, AND
//     b. names Q as an owner reference, AND
//     c. names Q as a spec reference target or transitive quiesced root (in p.Targets).
func CanUnquiesce(q velerov1api.QuiescedObjectRef, pendingPatches []velerov1api.PendingPatchRef) bool {
	if q.UnquiesceBlocked {
		return false
	}
	for _, p := range pendingPatches {
		// 1. Is Q itself still pending a patch?
		if p.Group == q.Group && p.Kind == q.Kind && p.Namespace == q.Namespace && p.Name == q.Name {
			return false
		}
		// 2. Does this pending patch name Q as an owner reference?
		for _, owner := range p.OwnerReferences {
			ownerGroup := ownerRefGroup(owner.APIVersion)
			if ownerGroup == q.Group && owner.Kind == q.Kind && owner.Name == q.Name {
				if q.Namespace == "" || p.Namespace == q.Namespace {
					return false
				}
			}
		}
		// 3. Does this pending patch name Q as a spec reference target?
		for _, target := range p.Targets {
			if target.Kind == q.Kind && target.Name == q.Name {
				if target.Group == "" || target.Group == q.Group {
					targetNS := target.Namespace
					if targetNS == "" {
						targetNS = p.Namespace
					}
					if q.Namespace == "" || targetNS == q.Namespace {
						return false
					}
				}
			}
		}
	}
	return true
}

// UnquiesceEligibleObjects partitions quiesced records, unpauses eligible objects independently,
// and returns the remaining slice of objects that must stay paused.
func UnquiesceEligibleObjects(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	records []velerov1api.QuiescedObjectRef,
	pendingPatches []velerov1api.PendingPatchRef,
) ([]velerov1api.QuiescedObjectRef, results.Result) {
	var eligible []velerov1api.QuiescedObjectRef
	var stillQuiesced []velerov1api.QuiescedObjectRef

	for _, rec := range records {
		if CanUnquiesce(rec, pendingPatches) {
			eligible = append(eligible, rec)
		} else {
			stillQuiesced = append(stillQuiesced, rec)
		}
	}

	warnings := results.Result{}
	if len(eligible) > 0 {
		failed, unquiesceWarnings := UnquiesceObjects(ctx, log, crClient, eligible)
		warnings.Merge(&unquiesceWarnings)
		stillQuiesced = append(stillQuiesced, failed...)
	}
	return stillQuiesced, warnings
}

// UnquiesceObjects removes Velero-injected pause annotations and tracking labels.
// It only strips a pause when the tracking label or velero.io/quiesced-key is present,
// so originally-paused production objects are left untouched even if listed by mistake.
// Objects that fail to unpause are returned so they remain in Restore.Status.QuiescedObjects.
func UnquiesceObjects(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	records []velerov1api.QuiescedObjectRef,
) ([]velerov1api.QuiescedObjectRef, results.Result) {
	warnings := results.Result{}
	var failed []velerov1api.QuiescedObjectRef
	if crClient == nil || len(records) == 0 {
		return failed, warnings
	}

	for _, rec := range records {
		if ctx.Err() != nil {
			if log != nil {
				log.WithError(ctx.Err()).Warnf("Context canceled or timed out; retaining %s/%s in quiescedObjects", rec.Namespace, rec.Name)
			}
			warnings.Add(rec.Namespace, ctx.Err())
			failed = append(failed, rec)
			continue
		}

		if strings.TrimSpace(rec.AnnotationKey) == "" {
			if log != nil {
				log.Warnf("Cannot unquiesce %s/%s: empty annotationKey in record", rec.Namespace, rec.Name)
			}
			warnings.Add(rec.Namespace, fmt.Errorf("cannot unquiesce %s/%s: missing pause annotation key", rec.Namespace, rec.Name))
			failed = append(failed, rec)
			continue
		}

		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			liveObj := &unstructured.Unstructured{}
			liveObj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   rec.Group,
				Version: rec.Version,
				Kind:    rec.Kind,
			})
			key := client.ObjectKey{Namespace: rec.Namespace, Name: rec.Name}
			if err := crClient.Get(ctx, key, liveObj); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}

			annotations := liveObj.GetAnnotations()
			labels := liveObj.GetLabels()
			hasTrackingLabel := labels != nil && labels[LabelQuiescedByRestore] != ""
			hasQuiescedKey := annotations != nil && annotations[AnnotationQuiescedKey] != ""
			// Not a Velero-injected pause: leave the object alone.
			if !hasTrackingLabel && !hasQuiescedKey {
				return nil
			}

			metadata := map[string]any{
				annotationsKey: map[string]any{
					rec.AnnotationKey:     nil,
					AnnotationQuiescedKey: nil,
				},
				"labels": map[string]any{
					LabelQuiescedByRestore: nil,
				},
			}
			patchObj := map[string]any{
				metadataKey: metadata,
			}
			patchBytes, err := json.Marshal(patchObj)
			if err != nil {
				return fmt.Errorf("marshal unquiesce patch for %s/%s: %w", rec.Namespace, rec.Name, err)
			}
			return crClient.Patch(ctx, liveObj, client.RawPatch(types.MergePatchType, patchBytes))
		})

		if err != nil {
			if log != nil {
				log.WithError(err).Warnf("Failed to unquiesce %s/%s after restore remapping", rec.Namespace, rec.Name)
			}
			warnings.Add(rec.Namespace, fmt.Errorf("failed to unquiesce %s/%s: %w", rec.Namespace, rec.Name, err))
			failed = append(failed, rec)
		} else if log != nil {
			log.Infof("Successfully unquiesced %s/%s; controllers may now resume reconciliation", rec.Namespace, rec.Name)
		}
	}
	return failed, warnings
}

// leftoverFallbackVersions is a best-effort list of common CRD API versions used only when a
// RESTMapper is unavailable or cannot resolve a GVK (e.g. the type is not registered in the test
// scheme). In a live cluster the RESTMapper discovers the actually-served version, so CRDs at
// unusual versions (v1alpha3, v1gamma1, ...) are still found.
var leftoverFallbackVersions = []string{"v1beta1", "v1", "v1beta2", "v1alpha1", "v1alpha2"}

// CatchLeftoverPausedObjects lists objects still carrying velero.io/quiesced-by-restore=<restore-name>
// for configured quiesce GVKs. Used in Finalizing when Status.QuiescedObjects is empty after a
// partial persist. Labels cannot reconstruct PendingOwnerRefPatches or uidMap.
//
// restMapper, when non-nil, is preferred over the hardcoded fallback version list: it resolves the
// GVK to the API server's preferred served version, so CRDs served at non-standard versions are
// still discovered. When restMapper is nil or returns NoMatchError, the fallback version list is
// used so this remains best-effort safe in tests and degraded environments.
func CatchLeftoverPausedObjects(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restMapper meta.RESTMapper,
	restoreName string,
	rules []ownerref.QuiesceRule,
) []velerov1api.QuiescedObjectRef {
	if crClient == nil || restoreName == "" || len(rules) == 0 {
		return nil
	}

	validRestoreName := label.GetValidName(restoreName)
	seen := make(map[string]struct{})
	var leftover []velerov1api.QuiescedObjectRef

	for _, rule := range rules {
		if strings.TrimSpace(rule.Kind) == "" {
			continue
		}
		versions := resolvedVersionsFor(restMapper, rule.Group, rule.Kind, log)
		for _, version := range versions {
			if ctx.Err() != nil {
				return leftover
			}
			list := &unstructured.UnstructuredList{}
			gvk := schema.GroupVersionKind{Group: rule.Group, Version: version, Kind: rule.Kind}
			list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
			if err := crClient.List(ctx, list, client.MatchingLabels{LabelQuiescedByRestore: validRestoreName}); err != nil {
				if ctx.Err() != nil {
					return leftover
				}
				continue
			}
			for i := range list.Items {
				obj := &list.Items[i]
				objGVK := obj.GroupVersionKind()
				if objGVK.Empty() {
					objGVK = schema.GroupVersionKind{Group: rule.Group, Version: version, Kind: rule.Kind}
				}
				if objGVK.Group != rule.Group || objGVK.Kind != rule.Kind {
					continue
				}
				key := fmt.Sprintf("%s/%s/%s/%s", objGVK.Group, objGVK.Kind, obj.GetNamespace(), obj.GetName())
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				annKey := rule.AnnotationKey
				if anns := obj.GetAnnotations(); anns != nil {
					if k := anns[AnnotationQuiescedKey]; k != "" {
						annKey = k
					}
				}
				leftover = append(leftover, velerov1api.QuiescedObjectRef{
					Group:         objGVK.Group,
					Version:       objGVK.Version,
					Kind:          objGVK.Kind,
					Namespace:     obj.GetNamespace(),
					Name:          obj.GetName(),
					AnnotationKey: annKey,
				})
			}
		}
	}

	if log != nil && len(leftover) > 0 {
		log.Infof("Layer 1 leftover-pause catch-up found %d objects still paused by restore %s", len(leftover), restoreName)
	}
	return leftover
}

// resolvedVersionsFor returns the API versions to try when listing objects of (group, kind).
// When restMapper resolves the GVK, only the preferred served version is returned (Kubernetes serves
// all stored objects of a kind at the requested version). On NoMatchError or a nil mapper, the
// best-effort fallback list is returned so discovery gaps do not silently skip leftover pauses.
func resolvedVersionsFor(restMapper meta.RESTMapper, group, kind string, log logrus.FieldLogger) []string {
	if restMapper != nil {
		if mapping, err := restMapper.RESTMapping(schema.GroupKind{Group: group, Kind: kind}); err == nil {
			if v := mapping.GroupVersionKind.Version; v != "" {
				return []string{v}
			}
		} else if log != nil && !meta.IsNoMatchError(err) {
			log.WithError(err).Debugf("RESTMapper lookup for %s/%s failed; falling back to version list", group, kind)
		}
	}
	return leftoverFallbackVersions
}

// LoadQuiesceRulesForCatchUp returns baseline ∪ per-restore ConfigMap quiesce rules for leftover-pause catch-up.
func LoadQuiesceRulesForCatchUp(ctx context.Context, crClient client.Client, restore *velerov1api.Restore, serverConfigMap string) []ownerref.QuiesceRule {
	scope := ownerref.NewScope()
	if crClient == nil || restore == nil {
		return scope.QuiesceRules
	}

	// 1. Load baseline ConfigMap (serverConfigMap or fallback to velero-ownerref-config) if present
	baselineCMName := strings.TrimSpace(serverConfigMap)
	if baselineCMName == "" {
		baselineCMName = ownerref.DefaultConfigMapName
	}

	baselineCM := &corev1api.ConfigMap{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: baselineCMName}, baselineCM); err == nil {
		// Catch-up pause cleanup is best-effort; ignore parse error here.
		_ = scope.MergeConfigMap(baselineCM)
	}

	// 2. Merge per-restore ConfigMap if specified and different from baseline
	if restore.Spec.OwnerRefConfigMap != nil && strings.TrimSpace(restore.Spec.OwnerRefConfigMap.Name) != "" {
		restoreCMName := strings.TrimSpace(restore.Spec.OwnerRefConfigMap.Name)
		if restoreCMName != baselineCMName {
			restoreCM := &corev1api.ConfigMap{}
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: restoreCMName}, restoreCM); err == nil {
				// Catch-up pause cleanup is best-effort; ignore parse error here.
				_ = scope.MergeConfigMap(restoreCM)
			}
		}
	}

	return scope.QuiesceRules
}
