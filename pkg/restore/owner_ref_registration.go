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
	"fmt"
	"os"

	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/archive"
)

// Namespaces bypass restoreItem, including those ensured for additional items.
// Register parent mappings only; child scope always excludes Namespace relinking.
// Read source metadata from the archive because getNamespace strips UID and owners.
func (ctx *restoreContext) registerOwnerRefNamespace(path string, desired *corev1api.Namespace, created bool) error {
	key := path + "/" + desired.Name
	if ctx.ownerRefRelink == nil || ctx.ownerRefNamespaces[key] {
		return nil
	}
	if _, err := ctx.fileSystem.Stat(path); os.IsNotExist(err) {
		// A synthesized namespace has no backup identity to register.
		return nil
	} else if err != nil {
		return fmt.Errorf("read ownerReference source namespace %s: %w", desired.Name, err)
	}
	backup, err := archive.Unmarshal(ctx.fileSystem, path)
	if err != nil {
		return fmt.Errorf("decode ownerReference source namespace %s: %w", desired.Name, err)
	}
	getCtx, cancel := OwnerRefStageContext(context.Background(), ctx.resourceTimeout)
	defer cancel()
	live, err := ctx.namespaceClient.Get(getCtx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get namespace %s for ownerReference registration: %w", desired.Name, err)
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(live)
	if err != nil {
		return fmt.Errorf("convert namespace %s for ownerReference registration: %w", desired.Name, err)
	}
	liveObject := &unstructured.Unstructured{Object: content}
	// Typed clients omit TypeMeta; discovery still needs the Namespace GVK.
	liveObject.SetGroupVersionKind(corev1api.SchemeGroupVersion.WithKind("Namespace"))
	if created || ctx.restore.Spec.ExistingResourcePolicy == velerov1.ResourcePolicyTypeUpdate ||
		(equality.Semantic.DeepEqual(desired.Labels, live.Labels) &&
			equality.Semantic.DeepEqual(desired.Annotations, live.Annotations) &&
			equality.Semantic.DeepEqual(desired.Spec, live.Spec)) {
		ctx.registerOwnerRefObject(backup, liveObject, liveObject.GroupVersionKind(), created)
	}
	if ctx.ownerRefNamespaces == nil {
		ctx.ownerRefNamespaces = make(map[string]bool)
	}
	ctx.ownerRefNamespaces[key] = true
	return nil
}

func (ctx *restoreContext) registerOwnerRefObject(backup, live *unstructured.Unstructured, fallback schema.GroupVersionKind, created bool) {
	if ctx.ownerRefRelink == nil {
		return
	}
	gvk := live.GroupVersionKind()
	if gvk.Empty() {
		gvk = fallback
	}
	record := ownerref.UIDMappingRecord{
		LiveUID: live.GetUID(), GroupKind: gvk.GroupKind(),
		APIVersion: gvk.GroupVersion().String(),
		Name:       live.GetName(), Namespace: live.GetNamespace(),
	}
	// The successful restore request establishes the target version. Discovery
	// supplies other served versions without reading the parent again. If it
	// is unavailable, the known target version remains a safe fallback.
	record.ServedAPIVersions = []string{record.APIVersion}
	if ctx.kbClient != nil {
		if mappings, err := ctx.kbClient.RESTMapper().RESTMappings(gvk.GroupKind()); err == nil {
			for _, mapping := range mappings {
				record.ServedAPIVersions = append(record.ServedAPIVersions, mapping.GroupVersionKind.GroupVersion().String())
			}
		}
	}
	if ctx.discoveryHelper != nil {
		if _, resource, err := ctx.discoveryHelper.KindFor(gvk); err == nil {
			record.Namespaced = resource.Namespaced
			record.ScopeKnown = true
		}
	}
	ctx.ownerRefRelink.Register(backup.GetUID(), record)
	if !created || !ctx.ownerRefRelink.Scope.IsInScope(gvk) || len(backup.GetOwnerReferences()) == 0 {
		return
	}
	ctx.ownerRefRelink.Enqueue(ownerref.OwnerPatchRequest{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
		Name: live.GetName(), Namespace: live.GetNamespace(), RestoredUID: live.GetUID(),
		Namespaced: record.Namespaced, ScopeKnown: record.ScopeKnown,
		OwnerReferences: backup.GetOwnerReferences(),
	})
}
