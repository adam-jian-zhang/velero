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
package kube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/pkg/test"
)

type policyPVFailingClient struct {
	crclient.Client
	err error
}

func (c policyPVFailingClient) Get(ctx context.Context, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
	if _, ok := obj.(*corev1api.PersistentVolume); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestGetPVForVolumePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, volume    string
		phase           corev1api.PersistentVolumeClaimPhase
		exists, invalid bool
	}{
		{name: "pending without PV", phase: corev1api.ClaimPending},
		{name: "bound without assigned name", phase: corev1api.ClaimBound, invalid: true},
		{name: "assigned pending resolves PV", volume: "pv", phase: corev1api.ClaimPending, exists: true},
		{name: "assigned bound resolves PV", volume: "pv", phase: corev1api.ClaimBound, exists: true},
		{name: "assigned missing PV fails", volume: "pv", phase: corev1api.ClaimBound, invalid: true},
		{name: "lost assigned missing PV fails", volume: "pv", phase: corev1api.ClaimLost, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := test.NewFakeControllerRuntimeClient(t)
			if tc.exists {
				require.NoError(t, cli.Create(t.Context(), &corev1api.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: tc.volume}}))
			}
			pvc := &corev1api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "claim"}, Spec: corev1api.PersistentVolumeClaimSpec{VolumeName: tc.volume}, Status: corev1api.PersistentVolumeClaimStatus{Phase: tc.phase}}
			pv, err := GetPVForVolumePolicy(pvc, cli)
			if tc.invalid {
				require.Error(t, err)
				require.Nil(t, pv)
				return
			}
			require.NoError(t, err)
			if tc.exists {
				require.Equal(t, tc.volume, pv.Name)
			} else {
				require.Nil(t, pv)
			}
		})
	}
	t.Run("lookup failures preserve API cause", func(t *testing.T) {
		for _, cause := range []error{apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "pv", nil), apierrors.NewTimeoutError("PV lookup timed out", 1)} {
			cli := policyPVFailingClient{Client: test.NewFakeControllerRuntimeClient(t), err: cause}
			pvc := &corev1api.PersistentVolumeClaim{Spec: corev1api.PersistentVolumeClaimSpec{VolumeName: "pv"}}
			pv, err := GetPVForVolumePolicy(pvc, cli)
			require.ErrorIs(t, err, cause)
			require.Nil(t, pv)
		}
	})
}
