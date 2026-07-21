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

	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	veleromocks "github.com/vmware-tanzu/velero/pkg/plugin/velero/mocks"
)

func searchRequestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, velerov2alpha1.AddToScheme(scheme))
	return scheme
}

func TestSearchRequestReconciler_NotReadyRequeues(t *testing.T) {
	scheme := searchRequestScheme(t)
	sr := &velerov2alpha1.SearchRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "velero", CreationTimestamp: metav1.Now()},
		Spec:       velerov2alpha1.SearchRequestSpec{Query: velerov2alpha1.SearchQuery{Kind: "Pod"}, Limit: 10},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sr).WithObjects(sr).Build()

	sp := veleromocks.NewSearchProvider(t)
	sp.On("Ready", context.Background()).Return(false, nil)

	r := NewSearchRequestReconciler(c, logrus.New(), sp, 10*time.Minute, 5*time.Minute, 1024*1024, nil)
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "velero", Name: "s1"}})
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, res.RequeueAfter)

	updated := &velerov2alpha1.SearchRequest{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sr), updated))
	assert.Empty(t, updated.Status.Phase)
}

func TestSearchRequestReconciler_ProcessesWhenReady(t *testing.T) {
	scheme := searchRequestScheme(t)
	sr := &velerov2alpha1.SearchRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "velero", CreationTimestamp: metav1.Now()},
		Spec:       velerov2alpha1.SearchRequestSpec{Query: velerov2alpha1.SearchQuery{Kind: "Pod"}, Limit: 10},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sr).WithObjects(sr).Build()

	sp := veleromocks.NewSearchProvider(t)
	sp.On("Ready", context.Background()).Return(true, nil)
	sp.On("Search", context.Background(), velero.SearchParams{Kind: "Pod", Limit: 10}).Return(velero.SearchResult{
		Records: []velero.ResourceRecord{
			{BackupName: "b1", ResourceName: "p1", Kind: "Pod", Namespace: "ns"},
		},
		TotalCount: 1,
	}, nil)

	r := NewSearchRequestReconciler(c, logrus.New(), sp, 10*time.Minute, 5*time.Minute, 1024*1024, nil)
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "velero", Name: "s1"}})
	require.NoError(t, err)

	updated := &velerov2alpha1.SearchRequest{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sr), updated))
	assert.Equal(t, velerov2alpha1.SearchRequestPhaseProcessed, updated.Status.Phase)
	assert.Equal(t, 1, updated.Status.TotalCount)
	require.Len(t, updated.Status.Results, 1)
}

func TestClampLimit(t *testing.T) {
	assert.Equal(t, 100, clampLimit(0))
	assert.Equal(t, 500, clampLimit(999))
	assert.Equal(t, 50, clampLimit(50))
}
