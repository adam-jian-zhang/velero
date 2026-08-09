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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/pkg/dag"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

// processSpecReferences rewrites allowlisted ObjectReference-like uid/namespace fields
// on already-created objects using the completed uidMap.
func processSpecReferences(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *OwnerRefRemapState,
	namespaceMapping map[string]string,
) results.Result {
	warnings := results.Result{}
	if state == nil || crClient == nil || len(state.SpecRefPaths) == 0 {
		return warnings
	}

	seen := make(map[string]struct{})

	tryRemap := func(gvk schema.GroupVersionKind, ns, name string) {
		key := gvk.Group + "/" + gvk.Version + "/" + gvk.Kind + "/" + ns + "/" + name
		if _, ok := seen[key]; ok {
			return
		}
		paths := specRefPathsFor(state.SpecRefPaths, gvk)
		if len(paths) == 0 {
			return
		}
		seen[key] = struct{}{}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			wrapped := fmt.Errorf("get %s %s/%s for spec-ref remap: %w", gvk.String(), ns, name, err)
			log.WithError(wrapped).Warn("Skipping spec-ref remap")
			warnings.Add(ns, wrapped)
			return
		}

		changed, patchObj, err := remapSpecRefFields(obj, paths, state, namespaceMapping, log)
		if err != nil {
			warnings.Add(ns, err)
			return
		}
		if !changed {
			return
		}

		patch, err := json.Marshal(patchObj)
		if err != nil {
			warnings.Add(ns, fmt.Errorf("marshal spec-ref patch for %s/%s: %w", ns, name, err))
			return
		}
		if err := crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
			wrapped := fmt.Errorf("patch spec refs on %s/%s: %w", ns, name, err)
			log.WithError(wrapped).Warn("Failed to patch spec refs")
			warnings.Add(ns, wrapped)
		}
	}

	for _, req := range state.OwnerPatchQueue {
		gvk := schema.GroupVersionKind{Group: req.Group, Version: req.Version, Kind: req.Kind}
		tryRemap(gvk, req.Namespace, req.Name)
	}

	if state.ResourceDAG != nil {
		for uid, node := range state.ResourceDAG.Nodes {
			gvk := schema.FromAPIVersionAndKind(node.APIVersion, node.Kind)
			state.uidMapLock.RLock()
			_, mapped := state.UIDMap[uid]
			state.uidMapLock.RUnlock()
			if !mapped {
				continue
			}
			ns := mapNamespace(node.Namespace, namespaceMapping)
			tryRemap(gvk, ns, node.Name)
		}
	}

	return warnings
}

func specRefPathsFor(entries []dag.SpecRefPathEntry, gvk schema.GroupVersionKind) []string {
	var paths []string
	for _, e := range entries {
		if e.Group != gvk.Group {
			continue
		}
		if e.Version != "" && e.Version != gvk.Version {
			continue
		}
		if e.Kind != gvk.Kind {
			continue
		}
		paths = append(paths, e.JSONPaths...)
	}
	return paths
}

// remapSpecRefFields rewrites allowlisted ref fields on obj and returns a merge-patch
// document containing only those rewritten paths.
func remapSpecRefFields(
	obj *unstructured.Unstructured,
	jsonPaths []string,
	state *OwnerRefRemapState,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) (bool, map[string]any, error) {
	changed := false
	patchObj := map[string]any{}

	for _, path := range jsonPaths {
		fields := splitJSONPath(path)
		if len(fields) == 0 {
			continue
		}
		val, found, err := unstructured.NestedFieldNoCopy(obj.Object, fields...)
		if err != nil || !found || val == nil {
			continue
		}
		refMap, ok := val.(map[string]any)
		if !ok {
			continue
		}

		fieldChanged := false
		if uidStr, ok := refMap["uid"].(string); ok && uidStr != "" {
			state.uidMapLock.RLock()
			newUID, mapped := state.UIDMap[types.UID(uidStr)]
			state.uidMapLock.RUnlock()
			if mapped {
				refMap["uid"] = string(newUID)
				fieldChanged = true
			} else {
				log.Warnf("Skipping unmapped spec ref uid at %s on %s/%s", path, obj.GetNamespace(), obj.GetName())
			}
		}
		if ns, ok := refMap["namespace"].(string); ok && ns != "" {
			mappedNS := mapNamespace(ns, namespaceMapping)
			if mappedNS != ns {
				refMap["namespace"] = mappedNS
				fieldChanged = true
			}
		}
		if !fieldChanged {
			continue
		}
		if err := unstructured.SetNestedField(obj.Object, refMap, fields...); err != nil {
			return changed, patchObj, err
		}
		if err := setNestedMapValue(patchObj, fields, refMap); err != nil {
			return changed, patchObj, err
		}
		changed = true
	}
	return changed, patchObj, nil
}

func setNestedMapValue(root map[string]any, fields []string, value map[string]any) error {
	if len(fields) == 0 {
		return fmt.Errorf("empty field path")
	}
	cur := root
	for i := 0; i < len(fields)-1; i++ {
		next, ok := cur[fields[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[fields[i]] = next
		}
		cur = next
	}
	cur[fields[len(fields)-1]] = value
	return nil
}

func splitJSONPath(path string) []string {
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return nil
	}
	return strings.Split(path, ".")
}
