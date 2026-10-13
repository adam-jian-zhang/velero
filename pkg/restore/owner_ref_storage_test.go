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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
)

func newOwnerRefPendingList(count, owners int) []velerov1.PendingPatchRef {
	list := make([]velerov1.PendingPatchRef, count)
	for i := range list {
		list[i] = newOwnerRefPending(i)
		list[i].OwnerReferences = make([]metav1.OwnerReference, owners)
		for j := range owners {
			list[i].OwnerReferences[j] = newOwnerRef(types.UID(strconv.Itoa(j)), fmt.Sprintf("parent-%d", j))
		}
	}
	return list
}

func TestOwnerRefPendingPartitionBoundaries(t *testing.T) {
	for _, tc := range []struct{ count, owners, inline int }{
		{100, 4, 100}, {101, 4, 100}, {50, 8, 50}, {51, 8, 50}, {1, 401, 0}, {100, 5, 80},
	} {
		t.Run(fmt.Sprintf("%d-%d", tc.count, tc.owners), func(t *testing.T) {
			pending := newOwnerRefPendingList(tc.count, tc.owners)
			inline, overflow := partitionPendingPatches(pending)
			assert.Len(t, inline, tc.inline)
			assert.Equal(t, pending, append(append([]velerov1.PendingPatchRef{}, inline...), overflow...))
			r := newOwnerRefRestore()
			c := newOwnerRefRestoreClient(t)
			require.NoError(t, ReconcilePendingPatchesConfigMap(context.Background(), c, r, pending))
			loaded, err := LoadPendingPatchesHybrid(context.Background(), c, r)
			require.NoError(t, err)
			assert.Equal(t, pending, loaded)
			assert.Equal(t, tc.inline < tc.count, r.Status.PendingPatchesConfigMap != "")
		})
	}
}

func TestOwnerRefCopyOnWriteCommitCleanupAndPreparationFailure(t *testing.T) {
	ctx := context.Background()
	r := newOwnerRefRestore()
	c := newOwnerRefRestoreClient(t, r)
	pending := newOwnerRefPendingList(102, 1)
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, pending))
	nameA := r.Status.PendingPatchesConfigMap
	require.NoError(t, c.Update(ctx, r))
	durable := r.DeepCopy()
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, pending[1:]))
	nameB := r.Status.PendingPatchesConfigMap
	assert.NotEqual(t, nameA, nameB)
	loaded, err := LoadPendingPatchesHybrid(ctx, c, durable)
	require.NoError(t, err)
	assert.Equal(t, pending, loaded)
	// A preparation failure cannot replace the previous inline list/reference.
	failing := &ownerRefInterceptClient{Client: c, create: func(context.Context, client.Object, ...client.CreateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{}, "snapshot", fmt.Errorf("denied"))
	}}
	before := durable.DeepCopy()
	require.Error(t, ReconcilePendingPatchesConfigMap(ctx, failing, durable, pending[1:]))
	assert.Equal(t, before.Status, durable.Status)
	// Cleanup uses durable B, despite the caller still referring to A.
	r.Status.Phase = velerov1.RestorePhasePartiallyFailed
	require.NoError(t, c.Update(ctx, r))
	require.NoError(t, CleanupPendingSnapshots(ctx, c, c, before))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: nameA}, &corev1api.ConfigMap{})))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: nameB}, &corev1api.ConfigMap{}))
	// Inline migration does not delete B before its reference is durably cleared.
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, pending[:50]))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: nameB}, &corev1api.ConfigMap{}))
	require.NoError(t, c.Update(ctx, r))
	require.NoError(t, CleanupPendingSnapshots(ctx, c, c, r))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: nameB}, &corev1api.ConfigMap{})))
}

func TestOwnerRefSnapshotLoadFailsClosedAndLegacyNameMigration(t *testing.T) {
	ctx := context.Background()
	r := newOwnerRefRestore()
	c := newOwnerRefRestoreClient(t)
	pending := newOwnerRefPendingList(101, 1)
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, pending))
	cm := &corev1api.ConfigMap{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Status.PendingPatchesConfigMap}, cm))
	legacy := cm.DeepCopy()
	legacy.ResourceVersion = ""
	legacy.Name = r.Name + "-pending-patches"
	legacy.Labels = nil
	legacy.Immutable = nil
	require.NoError(t, c.Create(ctx, legacy))
	r.Status.PendingPatchesConfigMap = legacy.Name
	loaded, err := LoadPendingPatchesHybrid(ctx, c, r)
	require.NoError(t, err)
	assert.Equal(t, pending, loaded)
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, pending))
	assert.NotEqual(t, legacy.Name, r.Status.PendingPatchesConfigMap)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"absent", nil}, {"corrupt", []byte("bad gzip")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.Status.PendingPatchesConfigMap = "broken"
			broken := legacy.DeepCopy()
			broken.Name = "broken"
			broken.ResourceVersion = ""
			broken.BinaryData = map[string][]byte{pendingPatchesKey: tc.data}
			if tc.name == "corrupt" {
				require.NoError(t, c.Create(ctx, broken))
				defer c.Delete(ctx, broken)
			}
			_, err := LoadPendingPatchesHybrid(ctx, c, r)
			require.Error(t, err)
			assert.Equal(t, "broken", r.Status.PendingPatchesConfigMap)
			assert.Len(t, r.Status.PendingOwnerRefPatches, 100)
		})
	}
	refs := legacy.OwnerReferences
	refs[0].UID = "foreign"
	legacy.OwnerReferences = refs
	require.NoError(t, c.Update(ctx, legacy))
	r.Status.PendingPatchesConfigMap = legacy.Name
	_, err = LoadPendingPatchesHybrid(ctx, c, r)
	require.ErrorContains(t, err, "owned")
}

func TestOwnerRefTotalCapAndSnapshotFailureAreRestoreErrors(t *testing.T) {
	ctx := context.Background()
	r := newOwnerRefRestore()
	c := newOwnerRefRestoreClient(t)
	errs := SavePendingPatchesHybrid(ctx, c, r, newOwnerRefPendingList(ownerref.MaxTotalPendingPatches+1, 1))
	assert.False(t, errs.IsEmpty())
	loaded, err := LoadPendingPatchesHybrid(ctx, c, r)
	require.NoError(t, err)
	assert.Len(t, loaded, ownerref.MaxTotalPendingPatches)
	before := r.DeepCopy()
	require.Error(t, ReconcilePendingPatchesConfigMap(ctx, c, r, newOwnerRefPendingList(ownerref.MaxTotalPendingPatches+1, 1)))
	assert.Equal(t, before.Status, r.Status)
	failing := &ownerRefInterceptClient{Client: c, create: func(context.Context, client.Object, ...client.CreateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{}, "snapshot", fmt.Errorf("denied"))
	}}
	errs = SavePendingPatchesHybrid(ctx, failing, r, newOwnerRefPendingList(101, 1))
	assert.False(t, errs.IsEmpty())
	assert.Len(t, r.Status.PendingOwnerRefPatches, 100)
	assert.Empty(t, r.Status.PendingPatchesConfigMap)
	r.Name = strings.Repeat("r", 253)
	assert.LessOrEqual(t, len(pendingSnapshotName(r, []byte("payload"))), 253)
}

func TestOwnerRefCompressedSnapshotCapacityPreflight(t *testing.T) {
	pending := newOwnerRefPendingList(101, 120)
	var bytes [128]byte
	for i := range pending {
		for j := range pending[i].OwnerReferences {
			_, err := rand.Read(bytes[:])
			require.NoError(t, err)
			pending[i].OwnerReferences[j].Name = hex.EncodeToString(bytes[:])[:253]
		}
	}
	_, overflow := partitionPendingPatches(pending)
	_, err := compressPendingPatches(overflow)
	require.ErrorContains(t, err, "1 MiB")
	r := newOwnerRefRestore()
	c := newOwnerRefRestoreClient(t)
	errs := SavePendingPatchesHybrid(t.Context(), c, r, pending)
	assert.False(t, errs.IsEmpty())
	assert.Len(t, r.Status.PendingOwnerRefPatches, 3)
	assert.Empty(t, r.Status.PendingPatchesConfigMap)
	// Pass 2 must preserve the old partition rather than applying Pass 1's drop rule.
	r.Status.PendingPatchesConfigMap = "authoritative"
	before := r.DeepCopy()
	require.Error(t, ReconcilePendingPatchesConfigMap(t.Context(), c, r, pending))
	assert.Equal(t, before.Status, r.Status)
	var list corev1api.ConfigMapList
	require.NoError(t, c.List(t.Context(), &list))
	assert.Empty(t, list.Items)
}
