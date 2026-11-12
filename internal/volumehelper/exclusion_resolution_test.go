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
package volumehelper

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/velero-io/velero/internal/resourcepolicies"
	"github.com/velero-io/velero/pkg/kuberesource"
	velerotest "github.com/velero-io/velero/pkg/test"
)

func TestBoundPVLookupFailureDoesNotChangeExclusions(t *testing.T) {
	pv := &corev1api.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "review-pv"}, Spec: corev1api.PersistentVolumeSpec{StorageClassName: "database"}}
	pvc := &corev1api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "review-pvc", Namespace: "default"}, Spec: corev1api.PersistentVolumeClaimSpec{VolumeName: pv.Name, StorageClassName: ptr.To("database")}, Status: corev1api.PersistentVolumeClaimStatus{Phase: corev1api.ClaimBound}}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pvc)
	require.NoError(t, err)
	client := velerotest.NewFakeControllerRuntimeClient(t, pv, pvc)
	policies := &resourcepolicies.Policies{}
	require.NoError(t, policies.BuildPolicy(&resourcepolicies.ResourcePolicies{Version: "v1", VolumePolicies: []resourcepolicies.VolumePolicy{
		{Conditions: map[string]any{"storageClass": []string{"database"}}, Action: resourcepolicies.Action{Type: resourcepolicies.Snapshot}},
		{Conditions: map[string]any{}, Action: resourcepolicies.Action{Type: resourcepolicies.Snapshot}, Exclude: []string{"*.log"}},
	}}))
	helper := NewVolumeHelperImpl(policies, ptr.To(true), logrus.StandardLogger(), client, false, false, nil)
	before, _, err := helper.GetEffectiveExclude(&unstructured.Unstructured{Object: obj}, kuberesource.PersistentVolumeClaims)
	require.NoError(t, err)
	require.Empty(t, before)
	require.NoError(t, client.Delete(context.Background(), pv))
	after, _, err := helper.GetEffectiveExclude(&unstructured.Unstructured{Object: obj}, kuberesource.PersistentVolumeClaims)
	require.ErrorContains(t, err, "review-pv")
	require.Empty(t, after)
	_, err = helper.ShouldPerformSnapshot(&unstructured.Unstructured{Object: obj}, kuberesource.PersistentVolumeClaims)
	require.ErrorContains(t, err, "review-pv")
	action, patterns, dropped, err := helper.ResolveVolumePolicy(&unstructured.Unstructured{Object: obj}, kuberesource.PersistentVolumeClaims, true)
	require.ErrorContains(t, err, "review-pv")
	require.Nil(t, action)
	require.Empty(t, patterns)
	require.False(t, dropped)
}
