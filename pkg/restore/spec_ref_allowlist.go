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
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
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
) (results.Result, []velerov1api.PendingPatchRef, int) {
	warnings := results.Result{}
	if state == nil || crClient == nil {
		return warnings, nil, 0
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	specPaths := state.GetSpecRefPaths()
	if len(specPaths) == 0 {
		return warnings, nil, 0
	}

	var pendingPatches []velerov1api.PendingPatchRef
	var remainingQueue []ownerref.OwnerPatchRequest
	seen := make(map[string]struct{})
	specRefsRemapped := 0

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

		var lastPatchBytes []byte
		var targets []velerov1api.TargetRef
		patched := false

		err := retry.OnError(retry.DefaultBackoff, isRetriablePatchError, func() error {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, obj); err != nil {
				return fmt.Errorf("get %s %s/%s for spec-ref remap: %w", gvk.String(), req.Namespace, req.Name, err)
			}

			changed, patchObj, collectedTargets, _, err := remapSpecRefFieldsWithTargets(obj, paths, state, namespaceMapping, log)
			if err != nil {
				return err
			}
			targets = collectedTargets
			if !changed || len(patchObj) == 0 {
				return nil
			}

			patchBytes, err := json.Marshal(patchObj)
			if err != nil {
				return fmt.Errorf("marshal spec-ref patch for %s/%s: %w", req.Namespace, req.Name, err)
			}
			lastPatchBytes = patchBytes
			err = crClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchBytes))
			if err == nil {
				patched = true
			}
			return err
		})

		if err != nil {
			log.WithError(err).Warnf("Failed to patch spec refs on %s/%s", req.Namespace, req.Name)
			warnings.Add(req.Namespace, err)
			allTargets := append([]velerov1api.TargetRef(nil), targets...)
			for _, root := range state.ResolveQuiescedRoots(req) {
				found := false
				for _, existing := range allTargets {
					if existing.Group == root.Group && existing.Kind == root.Kind && existing.Namespace == root.Namespace && existing.Name == root.Name {
						found = true
						break
					}
				}
				if !found {
					allTargets = append(allTargets, root)
				}
			}
			if isRetriablePatchError(err) && len(lastPatchBytes) > 0 {
				remainingQueue = append(remainingQueue, req)
				pendingPatches = append(pendingPatches, velerov1api.PendingPatchRef{
					Group:         gvk.Group,
					Version:       gvk.Version,
					Kind:          gvk.Kind,
					Namespace:     req.Namespace,
					Name:          req.Name,
					PatchType:     "specRef",
					SpecPatchJSON: string(lastPatchBytes),
					Targets:       allTargets,
				})
			} else {
				// Non-retriable error or Get failed without patch payload:
				// do NOT enqueue to pendingPatches with empty SpecPatchJSON (prevents etcd bloat and Pass 2 permanent failure).
				// Instead, mark targets as unquiesceBlocked so parents remain safely paused.
				for _, target := range allTargets {
					targetNS := target.Namespace
					if targetNS == "" {
						targetNS = req.Namespace
					}
					state.BlockUnquiesceForTarget(target.Group, target.Kind, targetNS, target.Name)
				}
				if len(allTargets) == 0 {
					// If Get failed before targets could be parsed, block unquiescing for any quiesced object
					// in this child's namespace to prevent unquiescing a parent that might have been referenced.
					state.BlockUnquiesceForNamespace(req.Namespace)
				}
			}
		} else if patched {
			specRefsRemapped++
		}
	}
	state.SetSpecPatchQueue(remainingQueue)

	return warnings, pendingPatches, specRefsRemapped
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
		for _, p := range e.Paths {
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
	paths []string,
	state *ownerref.OwnerRefRemapState,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) (bool, map[string]any, bool, error) {
	changed, patchObj, _, allResolved, err := remapSpecRefFieldsWithTargets(obj, paths, state, namespaceMapping, log)
	return changed, patchObj, allResolved, err
}

func remapSpecRefFieldsWithTargets(
	obj *unstructured.Unstructured,
	paths []string,
	state *ownerref.OwnerRefRemapState,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) (bool, map[string]any, []velerov1api.TargetRef, bool, error) {
	allResolved := true
	changed := false
	patchObj := map[string]any{}
	var targets []velerov1api.TargetRef

	for _, path := range paths {
		segments := splitDottedPath(path)
		if len(segments) == 0 {
			continue
		}

		newRoot, pathChanged, pathResolved, pathTargets, err := remapSpecRefFieldRecursiveWithTargets(obj.Object, segments, state, namespaceMapping, log)
		if err != nil {
			return false, nil, nil, false, err
		}
		if !pathResolved {
			allResolved = false
		}
		targets = append(targets, pathTargets...)
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
				return false, nil, nil, false, err
			}
		}
	}

	return changed, patchObj, targets, allResolved, nil
}

func remapSpecRefFieldRecursiveWithTargets(
	val any,
	pathSegments []string,
	state *ownerref.OwnerRefRemapState,
	namespaceMapping map[string]string,
	log logrus.FieldLogger,
) (any, bool, bool, []velerov1api.TargetRef, error) {
	if len(pathSegments) == 0 {
		return val, false, true, nil, nil
	}

	head := pathSegments[0]
	tail := pathSegments[1:]

	// 1. Array wildcard [*]
	if head == "[*]" {
		slice, ok := val.([]any)
		if !ok {
			return val, false, true, nil, nil
		}
		changed := false
		allResolved := true
		var targets []velerov1api.TargetRef
		newSlice := make([]any, len(slice))
		for i, item := range slice {
			newItem, itemChanged, itemResolved, itemTargets, err := remapSpecRefFieldRecursiveWithTargets(item, tail, state, namespaceMapping, log)
			if err != nil {
				return val, false, false, nil, err
			}
			targets = append(targets, itemTargets...)
			if itemChanged {
				changed = true
			}
			if !itemResolved {
				allResolved = false
			}
			newSlice[i] = newItem
		}
		return newSlice, changed, allResolved, targets, nil
	}

	// 2. Map segment
	m, ok := val.(map[string]any)
	if !ok {
		return val, false, true, nil, nil
	}

	// If tail is empty, head is the final property name pointing to the ObjectReference map
	if len(tail) == 0 {
		childVal, exists := m[head]
		if !exists || childVal == nil {
			return val, false, true, nil, nil
		}
		refMap, ok := childVal.(map[string]any)
		if !ok {
			return val, false, true, nil, nil
		}

		fieldChanged := false
		resolved := true
		var targets []velerov1api.TargetRef

		targetKind, _ := refMap["kind"].(string)
		targetName, _ := refMap["name"].(string)
		targetAPIVersion, _ := refMap["apiVersion"].(string)
		targetNS, _ := refMap[namespaceKey].(string)
		if targetNS != "" {
			targetNS = mapNamespace(targetNS, namespaceMapping)
		}
		if targetKind != "" && targetName != "" {
			targets = append(targets, velerov1api.TargetRef{
				Group:     ownerRefGroup(targetAPIVersion),
				Kind:      targetKind,
				Namespace: targetNS,
				Name:      targetName,
			})
		}

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
			return m, true, resolved, targets, nil
		}
		return val, false, resolved, targets, nil
	}

	// Intermediate map segment
	childVal, exists := m[head]
	if !exists || childVal == nil {
		return val, false, true, nil, nil
	}

	newChild, childChanged, childResolved, targets, err := remapSpecRefFieldRecursiveWithTargets(childVal, tail, state, namespaceMapping, log)
	if err != nil {
		return val, false, false, nil, err
	}
	if childChanged {
		m[head] = newChild
		return m, true, childResolved, targets, nil
	}
	return val, false, childResolved, targets, nil
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

func splitDottedPath(path string) []string {
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
