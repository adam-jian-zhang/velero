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
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

// InjectQuiesceMetadata inspects the backed-up object and injects pause metadata if applicable.
// If the object was already paused in production (pre-backup), OriginallyQuiesced is marked true
// and no changes are made.
func InjectQuiesceMetadata(
	obj *unstructured.Unstructured,
	rule ownerref.QuiesceRule,
	log logrus.FieldLogger,
) (ownerref.QuiescedObjectRecord, bool) {
	if obj == nil || strings.TrimSpace(rule.AnnotationKey) == "" {
		return ownerref.QuiescedObjectRecord{}, false
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	record := ownerref.QuiescedObjectRecord{
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
	obj.SetAnnotations(annotations)
	record.OriginallyQuiesced = false
	if log != nil {
		log.Infof("Auto-quiesced %s/%s via annotation %s=%q for restore",
			obj.GetNamespace(), obj.GetName(), rule.AnnotationKey, rule.AnnotationValue)
	}
	return record, true
}

// UnquiesceObjects removes injected quiesce annotations on successfully remapped resources.
// Objects that were already paused in production are left untouched.
// Returns warnings for any resources that failed to be unquiesced.
func UnquiesceObjects(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	records []ownerref.QuiescedObjectRecord,
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
			if annotations == nil {
				return nil
			}
			if _, exists := annotations[rec.AnnotationKey]; !exists {
				return nil
			}

			metadata := map[string]any{
				annotationsKey: map[string]any{
					rec.AnnotationKey: nil,
				},
			}
			if rv := liveObj.GetResourceVersion(); rv != "" {
				metadata[resourceVersionKey] = rv
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
