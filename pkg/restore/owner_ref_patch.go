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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

// ApplyOwnerRefRemapping runs Phase 1B ownerRef patches then spec-ref remapping.
// Soft-fails per object; returns warnings via results.Result.
func ApplyOwnerRefRemapping(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	restore *velerov1api.Restore,
	state *OwnerRefRemapState,
) results.Result {
	warnings := results.Result{}
	if state == nil || !state.Enabled || !state.ResourceDAGPresent {
		return warnings
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	var namespaceMapping map[string]string
	if restore != nil {
		namespaceMapping = restore.Spec.NamespaceMapping
	}

	for _, req := range state.OwnerPatchQueue {
		if err := patchOwnerReferences(ctx, log, crClient, state, req, namespaceMapping); err != nil {
			log.WithError(err).Warnf("Failed to patch ownerReferences for %s/%s", req.Namespace, req.Name)
			warnings.Add(req.Namespace, err)
		}
	}

	specWarnings := processSpecReferences(ctx, log, crClient, state, namespaceMapping)
	warnings.Merge(&specWarnings)
	return warnings
}

func patchOwnerReferences(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *OwnerRefRemapState,
	req OwnerPatchRequest,
	namespaceMapping map[string]string,
) error {
	var remapped []metav1.OwnerReference

	state.uidMapLock.RLock()
	// req.Namespace is already the live/target namespace from Phase 1A — do not remap it again.
	childTargetNS := req.Namespace

	for i, ref := range req.OriginalOwnerRefs {
		newUID, ok := state.UIDMap[ref.UID]
		if !ok {
			log.Warnf("Skipping unmapped ownerReference %s/%s for %s/%s",
				ref.Kind, ref.Name, req.Namespace, req.Name)
			continue
		}

		ownerSrcNS := ""
		if i < len(req.OwnerRefSourceNS) {
			ownerSrcNS = req.OwnerRefSourceNS[i]
		}
		if ownerSrcNS != "" {
			ownerTargetNS := mapNamespace(ownerSrcNS, namespaceMapping)
			if ownerTargetNS != childTargetNS {
				log.Warnf("Skipping ownerRef %s/%s for %s/%s: namespace mapping splits owner/owned",
					ref.Kind, ref.Name, req.Namespace, req.Name)
				continue
			}
		}

		ref.UID = newUID
		remapped = append(remapped, ref)
	}
	state.uidMapLock.RUnlock()

	if len(remapped) == 0 {
		return nil
	}

	return patchObjectOwnerRefs(ctx, crClient, req, remapped)
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

func patchObjectOwnerRefs(ctx context.Context, crClient client.Client, req OwnerPatchRequest, remapped []metav1.OwnerReference) error {
	if crClient == nil {
		return fmt.Errorf("nil client")
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

	obj.SetOwnerReferences(remapped)

	patch, err := generateOwnerRefMergePatch(remapped)
	if err != nil {
		return fmt.Errorf("marshal ownerRef patch for %s/%s: %w", req.Namespace, req.Name, err)
	}
	if len(patch) == 0 || string(patch) == "{}" {
		return nil
	}
	if err := crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch ownerReferences on %s/%s: %w", req.Namespace, req.Name, err)
	}
	return nil
}

func generateOwnerRefMergePatch(remapped []metav1.OwnerReference) ([]byte, error) {
	patchObj := map[string]any{
		"metadata": map[string]any{
			"ownerReferences": remapped,
		},
	}
	return json.Marshal(patchObj)
}
