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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

func TestOwnerRefPendingSnapshotRoundTrip(t *testing.T) {
	for _, tc := range []struct{ count, owners int }{
		{0, 0}, {1, 1}, {100, 4}, {101, 4}, {50, 8}, {51, 8}, {1, 401}, {100, 5},
	} {
		t.Run(fmt.Sprintf("%d-%d", tc.count, tc.owners), func(t *testing.T) {
			pending := newOwnerRefPendingList(tc.count, tc.owners)
			r := newOwnerRefRestore()
			c := newOwnerRefRestoreClient(t)
			require.NoError(t, ReconcilePendingPatchesConfigMap(t.Context(), c, r, pending))
			loaded, err := LoadPendingPatches(t.Context(), c, r)
			require.NoError(t, err)
			assert.Equal(t, tc.count, r.Status.PendingOwnerRefPatchCount)
			assert.Equal(t, tc.count > 0, r.Status.PendingPatchesConfigMap != "")
			if tc.count == 0 {
				assert.Empty(t, loaded)
			} else {
				assert.Equal(t, pending, loaded)
			}
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
	loaded, err := LoadPendingPatches(ctx, c, durable)
	require.NoError(t, err)
	assert.Equal(t, pending, loaded)
	// A preparation failure cannot replace the previous count/reference.
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
	// Draining work does not delete B before its reference is durably cleared.
	require.NoError(t, ReconcilePendingPatchesConfigMap(ctx, c, r, nil))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: nameB}, &corev1api.ConfigMap{}))
	require.NoError(t, c.Update(ctx, r))
	require.NoError(t, CleanupPendingSnapshots(ctx, c, c, r))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: nameB}, &corev1api.ConfigMap{})))
}

func TestOwnerRefSnapshotLoadFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "empty", "foreign", "mutable", "wrong label", "wrong count", "zero count", "count without snapshot", "invalid request", "trailing data"} {
		t.Run(mode, func(t *testing.T) {
			r := newOwnerRefRestore()
			c := newOwnerRefRestoreClient(t)
			pending := newOwnerRefPendingList(1, 1)
			require.NoError(t, ReconcilePendingPatchesConfigMap(t.Context(), c, r, pending))
			cm := &corev1api.ConfigMap{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: r.Namespace, Name: r.Status.PendingPatchesConfigMap}, cm))
			switch mode {
			case "missing":
				require.NoError(t, c.Delete(t.Context(), cm))
			case "corrupt":
				cm.BinaryData[pendingPatchesKey] = []byte("bad gzip")
			case "empty":
				payload, err := compressPendingPatches(nil)
				require.NoError(t, err)
				cm.BinaryData[pendingPatchesKey] = payload
			case "foreign":
				cm.OwnerReferences[0].UID = "foreign"
			case "mutable":
				cm.Immutable = nil
			case "wrong label":
				cm.Labels[pendingRestoreUIDLabel] = "foreign"
			case "wrong count":
				r.Status.PendingOwnerRefPatchCount = 2
			case "zero count":
				r.Status.PendingOwnerRefPatchCount = 0
			case "count without snapshot":
				r.Status.PendingPatchesConfigMap = ""
			case "invalid request":
				pending[0].RestoredUID = ""
				payload, err := compressPendingPatches(pending)
				require.NoError(t, err)
				cm.BinaryData[pendingPatchesKey] = payload
			case "trailing data":
				payload, err := compressPendingPatches(pending)
				require.NoError(t, err)
				cm.BinaryData[pendingPatchesKey] = append(cm.BinaryData[pendingPatchesKey], payload...)
			}
			// The fake client permits simulating malformed storage even when immutable.
			if mode != "missing" {
				require.NoError(t, c.Update(t.Context(), cm))
			}
			before := r.DeepCopy()
			_, err := LoadPendingPatches(t.Context(), c, r)
			require.Error(t, err)
			assert.Equal(t, before.Status, r.Status)
		})
	}
}

func TestOwnerRefTotalCapAndSnapshotFailureAreRestoreErrors(t *testing.T) {
	ctx := context.Background()
	r := newOwnerRefRestore()
	c := newOwnerRefRestoreClient(t)
	errs := SavePendingPatches(ctx, c, r, newOwnerRefPendingList(ownerref.MaxTotalPendingPatches+1, 1))
	assert.False(t, errs.IsEmpty())
	loaded, err := LoadPendingPatches(ctx, c, r)
	require.NoError(t, err)
	assert.Len(t, loaded, ownerref.MaxTotalPendingPatches)
	before := r.DeepCopy()
	require.Error(t, ReconcilePendingPatchesConfigMap(ctx, c, r, newOwnerRefPendingList(ownerref.MaxTotalPendingPatches+1, 1)))
	assert.Equal(t, before.Status, r.Status)
	failing := &ownerRefInterceptClient{Client: c, create: func(context.Context, client.Object, ...client.CreateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{}, "snapshot", fmt.Errorf("denied"))
	}}
	errs = SavePendingPatches(ctx, failing, r, newOwnerRefPendingList(101, 1))
	assert.False(t, errs.IsEmpty())
	assert.Zero(t, r.Status.PendingOwnerRefPatchCount)
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
	_, err := compressPendingPatches(pending)
	require.ErrorContains(t, err, "1 MiB")
	r := newOwnerRefRestore()
	c := newOwnerRefRestoreClient(t)
	errs := SavePendingPatches(t.Context(), c, r, pending)
	assert.False(t, errs.IsEmpty())
	assert.Zero(t, r.Status.PendingOwnerRefPatchCount)
	assert.Empty(t, r.Status.PendingPatchesConfigMap)
	// Pass 2 must preserve the old snapshot reference/count rather than applying Pass 1's drop rule.
	r.Status.PendingPatchesConfigMap = "authoritative"
	r.Status.PendingOwnerRefPatchCount = 1
	before := r.DeepCopy()
	require.Error(t, ReconcilePendingPatchesConfigMap(t.Context(), c, r, pending))
	assert.Equal(t, before.Status, r.Status)
	var list corev1api.ConfigMapList
	require.NoError(t, c.List(t.Context(), &list))
	assert.Empty(t, list.Items)
}

// Count storage calls separately from the existing Restore commit calls.
func TestOwnerRefSnapshotStorageAPICalls(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		initial, remaining, creates, reads, deletes int
	}{
		{name: "empty"},
		{name: "single drained", initial: 1, creates: 1, reads: 1, deletes: 1},
		{name: "many drained", initial: 101, creates: 1, reads: 1, deletes: 1},
		{name: "changed remainder", initial: 2, remaining: 1, creates: 2, reads: 1, deletes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOwnerRefRestore()
			base := newOwnerRefRestoreClient(t, r)
			creates, reads, deletes, restoreReads, lists := 0, 0, 0, 0, 0
			c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					creates++
					return c.Create(ctx, obj, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1api.ConfigMap); ok {
						reads++
					} else {
						restoreReads++
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					return c.Delete(ctx, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					return c.List(ctx, list, opts...)
				},
			})
			pending := newOwnerRefPendingList(tc.initial, 1)
			errs := SavePendingPatches(t.Context(), c, r, pending)
			require.True(t, errs.IsEmpty(), "%+v", errs)
			require.NoError(t, c.Update(t.Context(), r))
			loaded, err := LoadPendingPatches(t.Context(), c, r)
			require.NoError(t, err)
			require.Len(t, loaded, tc.initial)
			require.NoError(t, ReconcilePendingPatchesConfigMap(t.Context(), c, r, loaded[:tc.remaining]))
			r.Status.Phase = velerov1.RestorePhaseCompleted
			if tc.remaining > 0 {
				r.Status.Phase = velerov1.RestorePhasePartiallyFailed
			}
			require.NoError(t, c.Update(t.Context(), r))
			require.NoError(t, CleanupPendingSnapshots(t.Context(), c, c, r))
			assert.Equal(t, tc.creates, creates)
			assert.Equal(t, tc.reads, reads)
			assert.Equal(t, tc.deletes, deletes)
			assert.Equal(t, 1, restoreReads)
			assert.Equal(t, 1, lists)
			assert.Equal(t, tc.remaining, r.Status.PendingOwnerRefPatchCount)
			if tc.remaining > 0 {
				retained, err := LoadPendingPatches(t.Context(), base, r)
				require.NoError(t, err)
				assert.Equal(t, loaded[:tc.remaining], retained)
			}
		})
	}
}

func TestOwnerRefSnapshotReuseValidatesExistingObject(t *testing.T) {
	for _, mode := range []string{"valid", "owner", "immutable", "label", "payload"} {
		t.Run(mode, func(t *testing.T) {
			r := newOwnerRefRestore()
			c := newOwnerRefRestoreClient(t)
			pending := newOwnerRefPendingList(1, 1)
			require.NoError(t, ReconcilePendingPatchesConfigMap(t.Context(), c, r, pending))
			cm := &corev1api.ConfigMap{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: r.Namespace, Name: r.Status.PendingPatchesConfigMap}, cm))
			switch mode {
			case "owner":
				cm.OwnerReferences[0].UID = "foreign"
			case "immutable":
				cm.Immutable = nil
			case "label":
				cm.Labels = nil
			case "payload":
				cm.BinaryData[pendingPatchesKey] = []byte("different")
			}
			require.NoError(t, c.Update(t.Context(), cm))
			before := r.DeepCopy()
			err := ReconcilePendingPatchesConfigMap(t.Context(), c, r, pending)
			if mode == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			assert.Equal(t, before.Status, r.Status)
		})
	}
}
