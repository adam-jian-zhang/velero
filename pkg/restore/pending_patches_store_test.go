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
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

func makeTestPendingPatches(count int) []velerov1api.PendingPatchRef {
	isController := true
	blockOwnerDeletion := true
	patches := make([]velerov1api.PendingPatchRef, count)
	for i := 0; i < count; i++ {
		patches[i] = velerov1api.PendingPatchRef{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Machine",
			Namespace: "default",
			Name:      fmt.Sprintf("machine-%04d", i),
			PatchType: "ownerRef",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "cluster.x-k8s.io/v1beta1",
					Kind:               "MachineSet",
					Name:               fmt.Sprintf("ms-%02d", i%5),
					UID:                types.UID(fmt.Sprintf("uid-ms-%02d", i%5)),
					Controller:         &isController,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			},
			Targets: []velerov1api.TargetRef{
				{
					Group:     "cluster.x-k8s.io",
					Kind:      "Cluster",
					Namespace: "default",
					Name:      "prod-cluster",
				},
			},
		}
	}
	return patches
}

func TestCompressAndDecompressPendingPatches(t *testing.T) {
	// 1. Empty slice
	data, err := CompressPendingPatches(nil)
	require.NoError(t, err)
	assert.Nil(t, data)

	res, err := DecompressPendingPatches(nil)
	require.NoError(t, err)
	assert.Nil(t, res)

	// 2. Realistic dataset
	patches := makeTestPendingPatches(100)
	compressed, err := CompressPendingPatches(patches)
	require.NoError(t, err)
	require.NotEmpty(t, compressed)

	// Decompress and verify identity
	decompressed, err := DecompressPendingPatches(compressed)
	require.NoError(t, err)
	require.Len(t, decompressed, 100)
	assert.Equal(t, patches[0].Name, decompressed[0].Name)
	assert.Equal(t, patches[99].Name, decompressed[99].Name)
}

func TestSavePendingPatchesHybrid_UnderLimit(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	patches := makeTestPendingPatches(50)
	warnings := SavePendingPatchesHybrid(context.Background(), fakeClient, restore, patches, nil, logrus.StandardLogger())
	assert.Empty(t, warnings.Namespaces)

	// All 50 patches inline in Status, no overflow ConfigMap
	assert.Len(t, restore.Status.PendingOwnerRefPatches, 50)
	assert.Empty(t, restore.Status.PendingPatchesConfigMap)

	// Verify no ConfigMap was created
	cmList := &corev1api.ConfigMapList{}
	require.NoError(t, fakeClient.List(context.Background(), cmList))
	assert.Empty(t, cmList.Items)
}

func TestSavePendingPatchesHybrid_OverLimitAndLoad(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	// 525 patches: 500 inline, 25 in ConfigMap
	patches := makeTestPendingPatches(525)
	warnings := SavePendingPatchesHybrid(context.Background(), fakeClient, restore, patches, nil, logrus.StandardLogger())
	assert.Empty(t, warnings.Namespaces)

	assert.Len(t, restore.Status.PendingOwnerRefPatches, 500)
	assert.Equal(t, "test-restore-pending-patches", restore.Status.PendingPatchesConfigMap)

	// Verify ConfigMap exists in fakeClient with ownerReference
	cm := &corev1api.ConfigMap{}
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "velero", Name: "test-restore-pending-patches"}, cm)
	require.NoError(t, err)
	require.NotEmpty(t, cm.BinaryData[PendingPatchesDataKey])
	require.Len(t, cm.OwnerReferences, 1)
	assert.Equal(t, "test-restore", cm.OwnerReferences[0].Name)
	assert.Equal(t, "restore-uid-123", string(cm.OwnerReferences[0].UID))

	// Decompress and verify overflow contains items 500..524
	overflow, err := DecompressPendingPatches(cm.BinaryData[PendingPatchesDataKey])
	require.NoError(t, err)
	require.Len(t, overflow, 25)
	assert.Equal(t, "machine-0500", overflow[0].Name)
	assert.Equal(t, "machine-0524", overflow[24].Name)

	// Test LoadPendingPatchesHybrid reconstructs all 525 items
	loaded, loadWarnings, overflowLoadFailed := LoadPendingPatchesHybrid(context.Background(), fakeClient, restore)
	assert.Empty(t, loadWarnings.Namespaces)
	assert.False(t, overflowLoadFailed)
	require.Len(t, loaded, 525)
	assert.Equal(t, "machine-0000", loaded[0].Name)
	assert.Equal(t, "machine-0524", loaded[524].Name)
}

func TestReconcilePendingPatchesConfigMap(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	// 1. Initial save with 520 patches (500 status, 20 overflow CM)
	patches := makeTestPendingPatches(520)
	_ = SavePendingPatchesHybrid(context.Background(), fakeClient, restore, patches, nil, logrus.StandardLogger())
	assert.Equal(t, "test-restore-pending-patches", restore.Status.PendingPatchesConfigMap)

	// 2. Pass 2 retries and now only 15 items remain failed (<= 500)
	remainingDrained := makeTestPendingPatches(15)
	warnings := ReconcilePendingPatchesConfigMap(context.Background(), fakeClient, restore, remainingDrained)
	assert.Empty(t, warnings.Namespaces)

	// Verify ConfigMap was deleted and status cleared
	assert.Empty(t, restore.Status.PendingPatchesConfigMap)
	assert.Len(t, restore.Status.PendingOwnerRefPatches, 15)

	cm := &corev1api.ConfigMap{}
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "velero", Name: "test-restore-pending-patches"}, cm)
	require.Error(t, err) // Not found

	// 3. Reconcile where remaining items still exceed 500 (e.g. 510 items)
	remainingLarge := makeTestPendingPatches(510)
	warnings = ReconcilePendingPatchesConfigMap(context.Background(), fakeClient, restore, remainingLarge)
	assert.Empty(t, warnings.Namespaces)

	assert.Equal(t, "test-restore-pending-patches", restore.Status.PendingPatchesConfigMap)
	assert.Len(t, restore.Status.PendingOwnerRefPatches, 500)

	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "velero", Name: "test-restore-pending-patches"}, cm)
	require.NoError(t, err)
	reloadedOverflow, err := DecompressPendingPatches(cm.BinaryData[PendingPatchesDataKey])
	require.NoError(t, err)
	assert.Len(t, reloadedOverflow, 10)
}

type failingCreateClient struct {
	client.Client
}

func (f *failingCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return errors.New("simulated API server quota exceeded")
}

func (f *failingCreateClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return f.Client.Get(ctx, key, obj, opts...)
}

func TestSavePendingPatchesHybrid_FallbackCircuitBreaker(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	failingClient := &failingCreateClient{Client: baseClient}

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
		},
	}

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	state.RecordQuiescedObject(velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "prod-cluster",
	})

	// 510 patches
	patches := makeTestPendingPatches(510)

	warnings := SavePendingPatchesHybrid(context.Background(), failingClient, restore, patches, state, logrus.StandardLogger())

	// Warning must be recorded
	assert.NotEmpty(t, warnings.Namespaces)

	// Status capped at 500, no ConfigMap reference
	assert.Len(t, restore.Status.PendingOwnerRefPatches, 500)
	assert.Empty(t, restore.Status.PendingPatchesConfigMap)

	// The overflow items had target "prod-cluster". Verify state marked it UnquiesceBlocked=true
	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.True(t, quiesced[0].UnquiesceBlocked)
}

func TestSavePendingPatchesHybrid_PreflightSizeLimit(t *testing.T) {
	orig := overflowSizeLimit
	overflowSizeLimit = 1
	defer func() { overflowSizeLimit = orig }()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	state.RecordQuiescedObject(velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "prod-cluster",
	})

	patches := makeTestPendingPatches(510)
	warnings := SavePendingPatchesHybrid(context.Background(), fakeClient, restore, patches, state, logrus.StandardLogger())
	require.NotEmpty(t, warnings.Namespaces)
	assert.Contains(t, warnings.Namespaces["velero"][0], "1MiB")

	assert.Len(t, restore.Status.PendingOwnerRefPatches, 500)
	assert.Empty(t, restore.Status.PendingPatchesConfigMap)

	cmList := &corev1api.ConfigMapList{}
	require.NoError(t, fakeClient.List(context.Background(), cmList))
	assert.Empty(t, cmList.Items)

	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.True(t, quiesced[0].UnquiesceBlocked)
}

func TestSavePendingPatchesHybrid_TotalPendingCap(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	state.RecordQuiescedObject(velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "prod-cluster",
	})

	patches := makeTestPendingPatches(velerov1api.MaxTotalPendingPatches + 1)
	warnings := SavePendingPatchesHybrid(context.Background(), fakeClient, restore, patches, state, logrus.StandardLogger())
	require.NotEmpty(t, warnings.Namespaces)
	assert.Contains(t, warnings.Namespaces["velero"][0], "MaxTotalPendingPatches")

	assert.Len(t, restore.Status.PendingOwnerRefPatches, velerov1api.MaxPendingPatches)
	assert.Equal(t, "test-restore-pending-patches", restore.Status.PendingPatchesConfigMap)

	loaded, loadWarnings, overflowLoadFailed := LoadPendingPatchesHybrid(context.Background(), fakeClient, restore)
	assert.Empty(t, loadWarnings.Namespaces)
	assert.False(t, overflowLoadFailed)
	assert.Len(t, loaded, velerov1api.MaxTotalPendingPatches)

	quiesced := state.GetQuiescedObjects()
	require.Len(t, quiesced, 1)
	assert.True(t, quiesced[0].UnquiesceBlocked)
}

type flakyCreateClient struct {
	client.Client
	creates int
}

func (f *flakyCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	f.creates++
	if f.creates == 1 {
		return apierrors.NewTooManyRequests("rate limited", 1)
	}
	return f.Client.Create(ctx, obj, opts...)
}

func TestSavePendingPatchesHybrid_RetryCreateSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	flaky := &flakyCreateClient{Client: baseClient}

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	patches := makeTestPendingPatches(510)
	warnings := SavePendingPatchesHybrid(context.Background(), flaky, restore, patches, nil, logrus.StandardLogger())
	assert.Empty(t, warnings.Namespaces)
	assert.Equal(t, "test-restore-pending-patches", restore.Status.PendingPatchesConfigMap)
	assert.GreaterOrEqual(t, flaky.creates, 2)

	cm := &corev1api.ConfigMap{}
	err := baseClient.Get(context.Background(), client.ObjectKey{Namespace: "velero", Name: "test-restore-pending-patches"}, cm)
	require.NoError(t, err)
}

type alwaysRetriableCreateClient struct {
	client.Client
}

func (f *alwaysRetriableCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return apierrors.NewTooManyRequests("rate limited", 1)
}

func TestSavePendingPatchesHybrid_RetryExhaustedCircuitBreaks(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	failingClient := &alwaysRetriableCreateClient{Client: baseClient}

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
		},
	}

	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	state.RecordQuiescedObject(velerov1api.QuiescedObjectRef{
		Group:     "cluster.x-k8s.io",
		Version:   "v1beta1",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "prod-cluster",
	})

	patches := makeTestPendingPatches(510)
	warnings := SavePendingPatchesHybrid(context.Background(), failingClient, restore, patches, state, logrus.StandardLogger())
	assert.NotEmpty(t, warnings.Namespaces)
	assert.Len(t, restore.Status.PendingOwnerRefPatches, 500)
	assert.Empty(t, restore.Status.PendingPatchesConfigMap)
	assert.True(t, state.GetQuiescedObjects()[0].UnquiesceBlocked)
}

type flakyGetClient struct {
	client.Client
	gets int
}

func (f *flakyGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	f.gets++
	if f.gets == 1 {
		return apierrors.NewServiceUnavailable("unavailable")
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

func TestLoadPendingPatchesHybrid_RetryGetSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
			UID:       "restore-uid-123",
		},
	}

	patches := makeTestPendingPatches(510)
	_ = SavePendingPatchesHybrid(context.Background(), baseClient, restore, patches, nil, logrus.StandardLogger())

	flaky := &flakyGetClient{Client: baseClient}
	loaded, warnings, overflowLoadFailed := LoadPendingPatchesHybrid(context.Background(), flaky, restore)
	assert.Empty(t, warnings.Namespaces)
	assert.False(t, overflowLoadFailed)
	assert.Len(t, loaded, 510)
	assert.GreaterOrEqual(t, flaky.gets, 2)
}

func TestLoadPendingPatchesHybrid_MissingConfigMapFailClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))
	require.NoError(t, velerov1api.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "velero",
		},
		Status: velerov1api.RestoreStatus{
			PendingOwnerRefPatches:  makeTestPendingPatches(2),
			PendingPatchesConfigMap: "test-restore-pending-patches",
		},
	}

	loaded, warnings, overflowLoadFailed := LoadPendingPatchesHybrid(context.Background(), fakeClient, restore)
	assert.NotEmpty(t, warnings.Namespaces)
	assert.True(t, overflowLoadFailed)
	assert.Len(t, loaded, 2)
}
