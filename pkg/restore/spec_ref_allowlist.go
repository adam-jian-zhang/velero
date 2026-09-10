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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

// processSpecReferences rewrites allowlisted ObjectReference-like uid/namespace fields
// on already-created objects using the completed uidMap.
func processSpecReferences(
	ctx context.Context,
	log logrus.FieldLogger,
	crClient client.Client,
	state *ownerref.OwnerRefRemapState,
	namespaceMapping map[string]string,
) results.Result {
	warnings := results.Result{}
	if state == nil || crClient == nil {
		return warnings
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	specPaths := state.GetSpecRefPaths()
	if len(specPaths) == 0 {
		return warnings
	}

	var remainingQueue []ownerref.OwnerPatchRequest
	seen := make(map[string]struct{})

	for _, req := range state.GetSpecPatchQueue() {
		gvk := req.GroupVersionKind()
		key := fmt.Sprintf("%s/%s/%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind, req.Namespace, req.Name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		var paths []string
		if scope := state.GetScope(); scope != nil {
			paths = scope.SpecRefPathsFor(gvk)
		} else {
			paths = specRefPathsForGVK(specPaths, gvk)
		}
		if len(paths) == 0 {
			continue
		}

		itemAllResolved := true
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, obj); err != nil {
				return fmt.Errorf("get %s %s/%s for spec-ref remap: %w", gvk.String(), req.Namespace, req.Name, err)
			}

			changed, patchObj, resolved, err := remapSpecRefFields(obj, paths, state, namespaceMapping, log)
			if err != nil {
				return err
			}
			itemAllResolved = resolved
			if !changed || len(patchObj) == 0 {
				return nil
			}

			if rv := obj.GetResourceVersion(); rv != "" {
				meta, ok := patchObj[metadataKey].(map[string]any)
				if !ok {
					meta = map[string]any{}
					patchObj[metadataKey] = meta
				}
				meta[resourceVersionKey] = rv
			}

			patchBytes, err := json.Marshal(patchObj)
			if err != nil {
				return fmt.Errorf("marshal spec-ref patch for %s/%s: %w", req.Namespace, req.Name, err)
			}
			return crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchBytes))
		})

		if err != nil {
			log.WithError(err).Warnf("Failed to patch spec refs on %s/%s", req.Namespace, req.Name)
			warnings.Add(req.Namespace, err)
			remainingQueue = append(remainingQueue, req)
		} else if !itemAllResolved {
			remainingQueue = append(remainingQueue, req)
		}
	}
	state.SetSpecPatchQueue(remainingQueue)

	return warnings
}

func specRefPathsForGVK(entries []ownerref.SpecRefPathEntry, gvk schema.GroupVersionKind) []string {
	var paths []string
	seen := make(map[string]struct{})
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
		for _, p := range e.JSONPaths {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// remapSpecRefFields rewrites allowlisted ref fields on obj and returns a merge-patch
// document containing only those rewritten paths, and whether all encountered spec ref UIDs were resolved.
func remapSpecRefFields(
	obj *unstructured.Unstructured,
	jsonPaths []string,
	state *ownerref.OwnerRefRemapState,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) (bool, map[string]any, bool, error) {
	allResolved := true
	changed := false
	patchObj := map[string]any{}

	for _, path := range jsonPaths {
		segments := splitJSONPath(path)
		if len(segments) == 0 {
			continue
		}

		newRoot, pathChanged, pathResolved, err := remapSpecRefFieldRecursive(obj.Object, segments, state, namespaceMapping, log)
		if err != nil {
			return false, nil, false, err
		}
		if !pathResolved {
			allResolved = false
		}
		if !pathChanged {
			continue
		}
		changed = true
		if m, ok := newRoot.(map[string]any); ok {
			obj.Object = m
		}

		// Determine patch path: if contains array wildcard [*], the patch must replace the whole slice
		wildcardIdx := -1
		for i, s := range segments {
			if s == "[*]" {
				wildcardIdx = i
				break
			}
		}

		patchPath := segments
		if wildcardIdx >= 0 {
			patchPath = segments[:wildcardIdx]
		}

		val, found, err := unstructured.NestedFieldNoCopy(obj.Object, patchPath...)
		if err == nil && found {
			if err := setNestedValue(patchObj, patchPath, val); err != nil {
				return false, nil, false, err
			}
		}
	}

	return changed, patchObj, allResolved, nil
}

// remapSpecRefFieldRecursive recursively traverses maps and slices supporting array wildcard [*].
func remapSpecRefFieldRecursive(
	val any,
	pathSegments []string,
	state *ownerref.OwnerRefRemapState,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) (any, bool, bool, error) {
	if len(pathSegments) == 0 {
		return val, false, true, nil
	}

	head := pathSegments[0]
	tail := pathSegments[1:]

	// 1. Array wildcard [*]
	if head == "[*]" {
		slice, ok := val.([]any)
		if !ok {
			return val, false, true, nil
		}
		changed := false
		allResolved := true
		newSlice := make([]any, len(slice))
		for i, item := range slice {
			newItem, itemChanged, itemResolved, err := remapSpecRefFieldRecursive(item, tail, state, namespaceMapping, log)
			if err != nil {
				return val, false, false, err
			}
			if itemChanged {
				changed = true
			}
			if !itemResolved {
				allResolved = false
			}
			newSlice[i] = newItem
		}
		return newSlice, changed, allResolved, nil
	}

	// 2. Map segment
	m, ok := val.(map[string]any)
	if !ok {
		return val, false, true, nil
	}

	// If tail is empty, head is the final property name pointing to the ObjectReference map
	if len(tail) == 0 {
		childVal, exists := m[head]
		if !exists || childVal == nil {
			return val, false, true, nil
		}
		refMap, ok := childVal.(map[string]any)
		if !ok {
			return val, false, true, nil
		}

		fieldChanged := false
		resolved := true
		if uidStr, ok := refMap[uidKey].(string); ok && uidStr != "" {
			uid := types.UID(uidStr)
			if newUID, found := state.GetNewUID(uid); found {
				refMap[uidKey] = string(newUID)
				if _, hasRV := refMap[resourceVersionKey]; hasRV {
					refMap[resourceVersionKey] = nil
				}
				fieldChanged = true
			} else if state.IsNewUID(uid) {
				// Already remapped in a previous pass
				resolved = true
			} else {
				resolved = false
				if log != nil {
					log.Debugf("Skipping unmapped spec ref uid %q at %s", uidStr, head)
				}
			}
		}
		if ns, ok := refMap[namespaceKey].(string); ok && ns != "" {
			mappedNS := mapNamespace(ns, namespaceMapping)
			if mappedNS != ns {
				refMap[namespaceKey] = mappedNS
				fieldChanged = true
			}
		}
		if fieldChanged {
			m[head] = refMap
			return m, true, resolved, nil
		}
		return val, false, resolved, nil
	}

	// Intermediate map segment
	childVal, exists := m[head]
	if !exists || childVal == nil {
		return val, false, true, nil
	}

	newChild, childChanged, childResolved, err := remapSpecRefFieldRecursive(childVal, tail, state, namespaceMapping, log)
	if err != nil {
		return val, false, false, err
	}
	if childChanged {
		m[head] = newChild
		return m, true, childResolved, nil
	}
	return val, false, childResolved, nil
}

func setNestedValue(root map[string]any, fields []string, value any) error {
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
	path = strings.ReplaceAll(path, "[*]", ".[*]")
	path = strings.ReplaceAll(path, "..[*]", ".[*]")
	parts := strings.Split(path, ".")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
