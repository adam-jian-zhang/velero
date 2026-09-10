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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// LabelQuiescedByRestore tags live resources paused by Velero during restore.
	LabelQuiescedByRestore = "velero.io/quiesced-by-restore"
	// AnnotationQuiescedKey records the pause key (annotation or spec field) that was injected.
	AnnotationQuiescedKey = "velero.io/quiesced-key"
)

// InjectQuiesceMetadata inspects the backed-up object and injects pause metadata and live tracking label if applicable.
// If the object was already paused in production (pre-backup), OriginallyQuiesced is marked true
// and no changes are made.
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

	if _, alreadyPaused := annotations[rule.AnnotationKey]; alreadyPaused {
		record.OriginallyQuiesced = true
		return record, false
	}

	annotations[rule.AnnotationKey] = rule.AnnotationValue
	annotations[AnnotationQuiescedKey] = rule.AnnotationKey
	obj.SetAnnotations(annotations)

	if restoreName != "" {
		labels[LabelQuiescedByRestore] = restoreName
		obj.SetLabels(labels)
	}

	record.OriginallyQuiesced = false
	if log != nil {
		log.Infof("Auto-quiesced %s/%s via annotation %s=%q with tracking label %s=%q for restore",
			obj.GetNamespace(), obj.GetName(), rule.AnnotationKey, rule.AnnotationValue, LabelQuiescedByRestore, restoreName)
	}
	return record, true
}

// CanUnquiesce evaluates whether a quiesced object Q is eligible to be unquiesced.
// An object Q can be unquiesced if and only if NO remaining patch request in pendingPatches:
// 1. targets Q directly, AND
// 2. names Q as an owner reference, AND
// 3. names Q as a spec reference target.
func CanUnquiesce(q velerov1api.QuiescedObjectRef, pendingPatches []velerov1api.PendingPatchRef) bool {
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
				if target.Group == q.Group {
					if q.Namespace == "" || p.Namespace == q.Namespace {
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
		unquiesceWarnings := UnquiesceObjects(ctx, log, crClient, eligible)
		warnings.Merge(&unquiesceWarnings)
	}
	return stillQuiesced, warnings
}

// UnquiesceObjects removes injected quiesce annotations and tracking labels on successfully remapped resources.
// Objects that were already paused in production are left untouched.
// Returns warnings for any resources that failed to be unquiesced.
func UnquiesceObjects(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	records []velerov1api.QuiescedObjectRef,
) results.Result {
	warnings := results.Result{}
	if crClient == nil || len(records) == 0 {
		return warnings
	}

	for _, rec := range records {
		if rec.OriginallyQuiesced || strings.TrimSpace(rec.AnnotationKey) == "" {
			continue // Respect user's intentional production pause or skip invalid keys
		}

		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
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
			hasAnnotation := false
			if annotations != nil {
				_, hasAnnotation = annotations[rec.AnnotationKey]
			}
			hasTrackingLabel := false
			if labels != nil {
				_, hasTrackingLabel = labels[LabelQuiescedByRestore]
			}

			if !hasAnnotation && !hasTrackingLabel {
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
		} else if log != nil {
			log.Infof("Successfully unquiesced %s/%s; controllers may now resume reconciliation", rec.Namespace, rec.Name)
		}
	}
	return warnings
}
