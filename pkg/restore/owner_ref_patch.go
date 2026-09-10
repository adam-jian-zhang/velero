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
// Unresolved or failed owner patch requests remain in state.OwnerPatchQueue for subsequent passes.
// Soft-fails per object; returns warnings via results.Result.
func ApplyOwnerRefRemapping(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restore *velerov1api.Restore,
	state *ownerref.OwnerRefRemapState,
) results.Result {
	warnings := results.Result{}
	if state == nil || !state.Enabled || crClient == nil {
		return warnings
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	var namespaceMapping map[string]string
	if restore != nil {
		namespaceMapping = restore.Spec.NamespaceMapping
	}

	var remainingQueue []ownerref.OwnerPatchRequest

	for _, req := range state.GetOwnerPatchQueue() {
		fullyPatched, err := patchOwnerReferences(ctx, log, crClient, state, req, namespaceMapping)
		if err != nil {
			log.WithError(err).Warnf("Failed to patch ownerReferences for %s/%s", req.Namespace, req.Name)
			warnings.Add(req.Namespace, err)
			remainingQueue = append(remainingQueue, req)
		} else if !fullyPatched {
			// Some parents could not be resolved yet
			remainingQueue = append(remainingQueue, req)
		}
	}
	state.SetOwnerPatchQueue(remainingQueue)

	specWarnings := processSpecReferences(ctx, log, crClient, state, namespaceMapping)
	warnings.Merge(&specWarnings)
	return warnings
}

func patchOwnerReferences(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *ownerref.OwnerRefRemapState,
	req ownerref.OwnerPatchRequest,
	namespaceMapping map[string]string,
) (bool, error) {
	var remapped []metav1.OwnerReference
	allResolved := true

	childTargetNS := req.Namespace

	for i, ref := range req.OriginalOwnerRefs {
		newUID, ok := state.GetNewUID(ref.UID)
		if !ok {
			log.Debugf("Skipping unmapped ownerReference %s/%s for %s/%s; parent not yet restored",
				ref.Kind, ref.Name, req.Namespace, req.Name)
			allResolved = false
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
		return allResolved, nil
	}

	if err := patchObjectOwnerRefs(ctx, crClient, req, remapped); err != nil {
		return false, err
	}

	return allResolved, nil
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

		// Additive union/merge: do not clobber live ownerReferences on adopted objects
		mergedRefs := mergeOwnerReferences(obj.GetOwnerReferences(), remapped)
		if reflect.DeepEqual(obj.GetOwnerReferences(), mergedRefs) {
			return nil
		}

		patchBytes, err := generateOwnerRefMergePatch(obj.GetResourceVersion(), mergedRefs)
		if err != nil {
			return fmt.Errorf("marshal ownerRef patch for %s/%s: %w", req.Namespace, req.Name, err)
		}
		return crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchBytes))
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
// can have Controller: true.
func mergeOwnerReferences(live, remapped []metav1.OwnerReference) []metav1.OwnerReference {
	out := append([]metav1.OwnerReference(nil), live...)
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
			out[idx].Controller = r.Controller
			out[idx].BlockOwnerDeletion = r.BlockOwnerDeletion
			if r.Controller != nil && *r.Controller {
				for i := range out {
					if i != idx {
						out[i].Controller = nil
					}
				}
			}
		} else {
			if r.Controller != nil && *r.Controller {
				for i := range out {
					out[i].Controller = nil
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
