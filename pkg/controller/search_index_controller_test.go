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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	veleromocks "github.com/vmware-tanzu/velero/pkg/plugin/velero/mocks"
)

func TestIsIndexablePhase(t *testing.T) {
	assert.True(t, isIndexablePhase(velerov1api.BackupPhaseCompleted))
	assert.True(t, isIndexablePhase(velerov1api.BackupPhasePartiallyFailed))
	assert.False(t, isIndexablePhase(velerov1api.BackupPhaseFailed))
	assert.False(t, isIndexablePhase(velerov1api.BackupPhaseDeleting))
}

func TestSearchIndexReconciler_NotFoundDeletesOrphan(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, velerov1api.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	sp := veleromocks.NewSearchProvider(t)
	sp.On("ListIndexedBackups", context.Background()).Return([]string{"gone"}, nil).Once()
	sp.On("DeleteBackup", context.Background(), "gone").Return(nil).Once()
	sp.On("MarkReady", context.Background()).Return(nil).Maybe()

	r := NewSearchIndexReconciler(c, logrus.New(), nil, nil, sp, "velero", 2, time.Hour, nil)
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "velero", Name: "gone"}})
	require.NoError(t, err)
	assert.False(t, r.alreadyIndexed("gone"))
}

func TestSearchIndexReconciler_SkipsAlreadyIndexed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, velerov1api.AddToScheme(scheme))
	b := &velerov1api.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "velero"},
		Status:     velerov1api.BackupStatus{Phase: velerov1api.BackupPhaseCompleted},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b).Build()

	sp := veleromocks.NewSearchProvider(t)
	sp.On("ListIndexedBackups", context.Background()).Return([]string{"b1"}, nil).Once()
	sp.On("MarkReady", context.Background()).Return(nil).Once()

	r := NewSearchIndexReconciler(c, logrus.New(), nil, nil, sp, "velero", 2, time.Hour, nil)
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	require.NoError(t, err)
	assert.True(t, r.backfillDone.Load())
}

func TestSearchIndexReconciler_IgnoresDeleting(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, velerov1api.AddToScheme(scheme))
	b := &velerov1api.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "velero"},
		Status:     velerov1api.BackupStatus{Phase: velerov1api.BackupPhaseDeleting},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b).Build()

	sp := veleromocks.NewSearchProvider(t)
	sp.On("ListIndexedBackups", context.Background()).Return([]string{}, nil).Once()
	sp.On("MarkReady", context.Background()).Return(nil).Once()

	r := NewSearchIndexReconciler(c, logrus.New(), nil, nil, sp, "velero", 2, time.Hour, nil)
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	require.NoError(t, err)
	sp.AssertNotCalled(t, "DeleteBackup", context.Background(), "b1")
}
