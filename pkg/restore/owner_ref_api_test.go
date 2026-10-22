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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

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
	req := velerov1.PendingPatchRef{Version: "v1", Kind: "ConfigMap", Name: child.Name, Namespace: child.Namespace, RestoredUID: child.UID, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: parent.Name, UID: parent.UID}}}
	require.NoError(t, c.Create(ctx, r))
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, []velerov1.PendingPatchRef{req}))
	require.NoError(t, c.Update(ctx, r))
	loaded := &velerov1.Restore{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(r), loaded))
	require.Equal(t, 1, loaded.Status.PendingOwnerRefPatchCount)
	require.Equal(t, r.Status.PendingPatchesConfigMap, loaded.Status.PendingPatchesConfigMap)
	invalid := loaded.DeepCopy()
	invalid.Status.PendingOwnerRefPatchCount = -1
	assert.True(t, apierrors.IsInvalid(c.Update(ctx, invalid)), "the CRD must reject negative pending counts")
	saved, err := LoadPendingPatches(ctx, c, loaded)
	require.NoError(t, err)
	require.Equal(t, []velerov1.PendingPatchRef{req}, saved)
	warnings, errs, pending := RetryPendingPatches(ctx, c, r, saved)
	assert.True(t, warnings.IsEmpty())
	assert.True(t, errs.IsEmpty())
	assert.Empty(t, pending)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(child), child))
	require.Len(t, child.OwnerReferences, 1)
	assert.Equal(t, parent.UID, child.OwnerReferences[0].UID)
	assert.Equal(t, "update", child.Annotations["concurrent"])
	// The API server must reject replaying a stale terminal status commit.
	old := loaded.DeepCopy()
	loaded.Annotations = map[string]string{"changed": "true"}
	require.NoError(t, c.Update(ctx, loaded))
	terminal := old.DeepCopy()
	terminal.Status.Phase = velerov1.RestorePhaseCompleted
	assert.True(t, apierrors.IsConflict(c.Patch(ctx, terminal, client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{}))))
}
