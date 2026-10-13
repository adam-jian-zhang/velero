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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/util/results"
)

var errOwnerRefParentUnavailable = errors.New("ownerReference parent unavailable")

func isRetriableOwnerRefRequestError(err error) bool {
	return IsRetriableOwnerRefError(err) || errors.Is(err, errOwnerRefParentUnavailable)
}

type ownerRefParentKey struct {
	schema.GroupKind
	client.ObjectKey
}

type ownerRefParentLookup struct {
	uid types.UID
	err error
}

type ownerRefParentMapping struct {
	mappings []*meta.RESTMapping
	err      error
}

// refreshPendingOwnerRefParents makes at most one GET per distinct parent for
// this execution, caching failures too. Child retries reuse this snapshot. Only
// references already in pending work participate; Pass 1 successes are absent.
func refreshPendingOwnerRefParents(ctx context.Context, c client.Client, pending []velerov1.PendingPatchRef, warnings *results.Result) ([]velerov1.PendingPatchRef, []error, ownerRefServedVersions) {
	refreshed := make([]velerov1.PendingPatchRef, len(pending))
	failures := make([]error, len(pending))
	mappings := make(map[schema.GroupKind]ownerRefParentMapping)
	versions := make(ownerRefServedVersions)
	parents := make(map[ownerRefParentKey]ownerRefParentLookup)
	warned := make(map[ownerRefParentKey]bool)
	for i, original := range pending {
		refreshed[i] = original
		refreshed[i].OwnerReferences = ownerref.CopyReferences(original.OwnerReferences)
		for j := range refreshed[i].OwnerReferences {
			ref := &refreshed[i].OwnerReferences[j]
			key, uid, apiVersion, err := refreshOwnerRefParent(ctx, c, original.Namespace, *ref, mappings, parents)
			if err != nil {
				// Any permanent failure takes precedence over transient failures
				// on other owners. Still refresh the other references for persistence.
				if failures[i] == nil || isRetriableOwnerRefRequestError(failures[i]) {
					failures[i] = err
				}
				continue
			}
			if ref.UID != uid && !warned[key] {
				warnings.Add(original.Namespace, fmt.Errorf("refreshing ownerReference parent %s %s: previous UID %s, current UID %s", key.GroupKind, key.ObjectKey, ref.UID, uid))
				warned[key] = true
			}
			ref.UID = uid
			ref.APIVersion = apiVersion
		}
	}
	for _, resolved := range mappings {
		if resolved.err == nil {
			for _, mapping := range resolved.mappings {
				versions[mapping.GroupVersionKind] = true
			}
		}
	}
	return refreshed, failures, versions
}

func refreshOwnerRefParent(ctx context.Context, c client.Client, childNamespace string, ref metav1.OwnerReference, mappings map[schema.GroupKind]ownerRefParentMapping, parents map[ownerRefParentKey]ownerRefParentLookup) (ownerRefParentKey, types.UID, string, error) {
	var key ownerRefParentKey
	if err := ctx.Err(); err != nil {
		return key, "", "", err
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return key, "", "", fmt.Errorf("resolve ownerReference %s: %w", ownerIdentity(ref), err)
	}
	gvk := gv.WithKind(ref.Kind)
	resolved, found := mappings[gvk.GroupKind()]
	if !found {
		resolved.mappings, resolved.err = c.RESTMapper().RESTMappings(gvk.GroupKind())
		if resolved.err == nil && len(resolved.mappings) == 0 {
			resolved.err = &meta.NoKindMatchError{GroupKind: gvk.GroupKind()}
		}
		mappings[gvk.GroupKind()] = resolved
	}
	if resolved.err != nil {
		return key, "", "", fmt.Errorf("resolve scope of ownerReference %s: %w", ownerIdentity(ref), resolved.err)
	}
	// Retain a served recorded version, even when another version is preferred.
	// A legacy/unserved reference uses the first destination mapping instead.
	mapping := resolved.mappings[0]
	for _, candidate := range resolved.mappings {
		if candidate.GroupVersionKind == gvk {
			mapping = candidate
			break
		}
	}
	apiVersion := mapping.GroupVersionKind.GroupVersion().String()
	key = ownerRefParentKey{GroupKind: gvk.GroupKind(), ObjectKey: client.ObjectKey{Name: ref.Name}}
	switch mapping.Scope.Name() {
	case meta.RESTScopeNameNamespace:
		if childNamespace == "" {
			return key, "", "", fmt.Errorf("namespaced ownerReference %s cannot own a cluster-scoped child", ownerIdentity(ref))
		}
		key.Namespace = childNamespace
	case meta.RESTScopeNameRoot:
	default:
		return key, "", "", fmt.Errorf("ownerReference %s has unknown scope", ownerIdentity(ref))
	}
	if previous, found := parents[key]; found {
		return key, previous.uid, apiVersion, previous.err
	}
	if err := ctx.Err(); err != nil {
		return key, "", "", err
	}
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(mapping.GroupVersionKind)
	err = c.Get(ctx, key.ObjectKey, parent)
	if apierrors.IsNotFound(err) {
		err = fmt.Errorf("%w: %w", errOwnerRefParentUnavailable, err)
	} else if err == nil && (parent.GetDeletionTimestamp() != nil || parent.GetUID() == "") {
		err = fmt.Errorf("%w: %s %s is deleting or has no UID", errOwnerRefParentUnavailable, key.GroupKind, key.ObjectKey)
	}
	if err != nil {
		err = fmt.Errorf("refresh ownerReference %s: %w", ownerIdentity(ref), err)
	}
	parents[key] = ownerRefParentLookup{uid: parent.GetUID(), err: err}
	return key, parent.GetUID(), apiVersion, err
}
