//go:build integration

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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
)

func TestOwnerRefAPISchemaAndOptimisticOwnerRefPatches(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "v1", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1.AddToScheme(scheme))
	c, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := context.Background()
	ns := &corev1api.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ownerref-test"}}
	require.NoError(t, c.Create(ctx, ns))
	parent := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: ns.Name}}
	child := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: ns.Name}}
	require.NoError(t, c.Create(ctx, parent))
	require.NoError(t, c.Create(ctx, child))
	stale := child.DeepCopy()
	child.Annotations = map[string]string{"concurrent": "update"}
	require.NoError(t, c.Update(ctx, child))
	updated := stale.DeepCopy()
	updated.Annotations = map[string]string{"other": "update"}
	assert.True(t, apierrors.IsConflict(c.Patch(ctx, updated, client.MergeFromWithOptions(stale, client.MergeFromWithOptimisticLock{}))))
	r := &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{Name: "restore", Namespace: ns.Name}, Spec: velerov1.RestoreSpec{BackupName: "backup", OwnerRefConfigMap: &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: "scope"}}}
	state := ownerref.NewState(ownerref.NewScope())
	state.Register("backup-parent", ownerref.UIDMappingRecord{
		LiveUID: parent.UID, GroupKind: schema.GroupKind{Kind: "ConfigMap"}, APIVersion: "v1",
		Name: parent.Name, Namespace: parent.Namespace, Scope: ownerref.ScopeNamespaced,
	})
	state.Enqueue(ownerref.OwnerPatchRequest{
		Version: "v1", Kind: "ConfigMap", Name: child.Name, Namespace: child.Namespace,
		RestoredUID: child.UID, Scope: ownerref.ScopeNamespaced,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: parent.Name, UID: "backup-parent"}},
	})
	require.NoError(t, c.Create(ctx, r))
	warnings, errs := ApplyOwnerRefRelinking(ctx, c, r, state)
	assert.True(t, warnings.IsEmpty(), "%+v", warnings)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Equal(t, 1, r.Status.OwnerRefsRelinked)
	require.NoError(t, c.Update(ctx, r))
	loaded := &velerov1.Restore{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(r), loaded))
	assert.Equal(t, r.Spec.OwnerRefConfigMap, loaded.Spec.OwnerRefConfigMap)
	assert.Equal(t, 1, loaded.Status.OwnerRefsRelinked)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(child), child))
	require.Len(t, child.OwnerReferences, 1)
	assert.Equal(t, parent.UID, child.OwnerReferences[0].UID)
	assert.Equal(t, "update", child.Annotations["concurrent"])
	snapshots := &corev1api.ConfigMapList{}
	require.NoError(t, c.List(ctx, snapshots, client.InNamespace(ns.Name)))
	assert.Len(t, snapshots.Items, 2, "single-stage relinking must not create snapshot ConfigMaps")
}
