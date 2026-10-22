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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/ownerref"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/util/results"
)

const (
	pendingPatchesKey       = "patches.json.gz"
	pendingRestoreUIDLabel  = "velero.io/pending-patches-restore-uid"
	maxPendingSnapshotBytes = 1 << 20
)

func compressPendingPatches(pending []velerov1.PendingPatchRef) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if err := json.NewEncoder(writer).Encode(pending); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if buf.Len()+len(pendingPatchesKey) > maxPendingSnapshotBytes {
		return nil, fmt.Errorf("pending ownerReference snapshot exceeds 1 MiB compressed-data-plus-key limit")
	}
	return buf.Bytes(), nil
}

func ownsPendingSnapshot(restore *velerov1.Restore, cm *corev1api.ConfigMap) bool {
	if cm.Namespace != restore.Namespace || restore.UID == "" {
		return false
	}
	for _, ref := range cm.OwnerReferences {
		if ref.UID == restore.UID && ref.Name == restore.Name && ref.Kind == "Restore" && ref.APIVersion == velerov1.SchemeGroupVersion.String() && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func pendingSnapshotName(restore *velerov1.Restore, payload []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(restore.UID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(payload)
	suffix := "-pending-patches-" + hex.EncodeToString(h.Sum(nil))[:16]
	prefix := restore.Name
	if len(prefix) > 253-len(suffix) {
		prefix = strings.TrimRight(prefix[:253-len(suffix)], "-.")
	}
	return prefix + suffix
}

func preparePendingSnapshot(ctx context.Context, c client.Client, restore *velerov1.Restore, pending []velerov1.PendingPatchRef) (string, error) {
	if restore.UID == "" {
		return "", fmt.Errorf("cannot create pending snapshot without Restore UID")
	}
	payload, err := compressPendingPatches(pending)
	if err != nil {
		return "", err
	}
	immutable, controller := true, true
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: pendingSnapshotName(restore, payload), Namespace: restore.Namespace,
			Labels:          map[string]string{pendingRestoreUIDLabel: string(restore.UID)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: velerov1.SchemeGroupVersion.String(), Kind: "Restore", Name: restore.Name, UID: restore.UID, Controller: &controller}},
		},
		Immutable: &immutable, BinaryData: map[string][]byte{pendingPatchesKey: payload},
	}
	var lastErr error
	err = wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		candidate := cm.DeepCopy()
		lastErr = c.Create(ctx, candidate)
		if apierrors.IsAlreadyExists(lastErr) {
			existing := &corev1api.ConfigMap{}
			lastErr = c.Get(ctx, client.ObjectKeyFromObject(cm), existing)
			if lastErr == nil && (!ownsPendingSnapshot(restore, existing) || existing.Immutable == nil || !*existing.Immutable || existing.Labels[pendingRestoreUIDLabel] != string(restore.UID) || !bytes.Equal(existing.BinaryData[pendingPatchesKey], payload)) {
				return false, fmt.Errorf("existing pending snapshot %s has different ownership, immutability or payload", cm.Name)
			}
		}
		if lastErr == nil {
			return true, nil
		}
		if IsRetriableOwnerRefError(lastErr) {
			return false, nil
		}
		return false, lastErr
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if lastErr != nil {
			return "", lastErr
		}
		return "", err
	}
	return cm.Name, nil
}

// SavePendingPatches prepares Pass 1 storage. Only this initial pass may drop
// requests that cannot fit/be saved; every dropped request is an error.
func SavePendingPatches(ctx context.Context, c client.Client, restore *velerov1.Restore, pending []velerov1.PendingPatchRef) results.Result {
	errs := results.Result{}
	if len(pending) > ownerref.MaxTotalPendingPatches {
		errs.AddVeleroError(fmt.Errorf("dropped %d ownerReference requests beyond the %d pending limit", len(pending)-ownerref.MaxTotalPendingPatches, ownerref.MaxTotalPendingPatches))
		pending = pending[:ownerref.MaxTotalPendingPatches]
	}
	restore.Status.PendingOwnerRefPatchCount = 0
	restore.Status.PendingPatchesConfigMap = ""
	if err := ReconcilePendingPatchesConfigMap(ctx, c, restore, pending); err != nil {
		errs.AddVeleroError(fmt.Errorf("dropped %d pending ownerReference requests: %w", len(pending), err))
	}
	return errs
}

// ReconcilePendingPatchesConfigMap stages a complete replacement snapshot.
// Neither the old status reference/count nor any existing snapshot is changed on error.
func ReconcilePendingPatchesConfigMap(ctx context.Context, c client.Client, restore *velerov1.Restore, pending []velerov1.PendingPatchRef) error {
	if err := validatePendingPatches(pending); err != nil {
		return err
	}
	name := ""
	if len(pending) > 0 {
		var err error
		name, err = preparePendingSnapshot(ctx, c, restore, pending)
		if err != nil {
			return err
		}
	}
	restore.Status.PendingOwnerRefPatchCount = len(pending)
	restore.Status.PendingPatchesConfigMap = name
	return nil
}

func validatePendingPatches(pending []velerov1.PendingPatchRef) error {
	if len(pending) > ownerref.MaxTotalPendingPatches {
		return fmt.Errorf("pending queue exceeds total count limit")
	}
	for _, req := range pending {
		if req.Name == "" || req.Kind == "" || req.Version == "" || req.RestoredUID == "" || len(req.OwnerReferences) == 0 {
			return fmt.Errorf("invalid pending child request for %s/%s", req.Namespace, req.Name)
		}
		for _, ref := range req.OwnerReferences {
			if ref.UID == "" || ref.Name == "" || ref.Kind == "" || ref.APIVersion == "" {
				return fmt.Errorf("pending child %s/%s has an unresolved ownerReference", req.Namespace, req.Name)
			}
		}
	}
	return nil
}

func LoadPendingPatches(ctx context.Context, c client.Client, restore *velerov1.Restore) ([]velerov1.PendingPatchRef, error) {
	var pending []velerov1.PendingPatchRef
	if restore.Status.PendingPatchesConfigMap != "" {
		cm := &corev1api.ConfigMap{}
		var lastErr error
		err := wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
			lastErr = c.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: restore.Status.PendingPatchesConfigMap}, cm)
			if lastErr == nil {
				return true, nil
			}
			if IsRetriableOwnerRefError(lastErr) {
				return false, nil
			}
			return false, lastErr
		})
		if err != nil {
			return nil, fmt.Errorf("cannot load pending snapshot %s: %w", restore.Status.PendingPatchesConfigMap, err)
		}
		if !ownsPendingSnapshot(restore, cm) {
			return nil, fmt.Errorf("pending snapshot %s is not owned by this Restore", cm.Name)
		}
		if cm.Immutable == nil || !*cm.Immutable || cm.Labels[pendingRestoreUIDLabel] != string(restore.UID) {
			return nil, fmt.Errorf("pending snapshot %s is not immutable or has an invalid ownership label", cm.Name)
		}
		reader, err := gzip.NewReader(bytes.NewReader(cm.BinaryData[pendingPatchesKey]))
		if err != nil {
			return nil, fmt.Errorf("invalid pending snapshot %s: %w", cm.Name, err)
		}
		defer reader.Close()
		decoder := json.NewDecoder(reader)
		if err := decoder.Decode(&pending); err != nil {
			return nil, fmt.Errorf("decode pending snapshot %s: %w", cm.Name, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return nil, fmt.Errorf("pending snapshot %s has trailing data or invalid gzip checksum", cm.Name)
		}
		if len(pending) == 0 {
			return nil, fmt.Errorf("referenced pending snapshot %s is empty", cm.Name)
		}
	}
	if len(pending) != restore.Status.PendingOwnerRefPatchCount {
		return nil, fmt.Errorf("pending snapshot count mismatch: status has %d requests, storage has %d", restore.Status.PendingOwnerRefPatchCount, len(pending))
	}
	if err := validatePendingPatches(pending); err != nil {
		return nil, err
	}
	return pending, nil
}

// CleanupPendingSnapshots requires an uncached API reader. It always preserves
// the reference in durable status, even if the caller's Restore is stale.
func CleanupPendingSnapshots(ctx context.Context, reader client.Reader, c client.Client, restore *velerov1.Restore) error {
	durable := &velerov1.Restore{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(restore), durable); err != nil {
		return err
	}
	if durable.UID != restore.UID || (durable.Status.Phase != velerov1.RestorePhaseCompleted && durable.Status.Phase != velerov1.RestorePhasePartiallyFailed) {
		return nil
	}
	var list corev1api.ConfigMapList
	if err := reader.List(ctx, &list, client.InNamespace(restore.Namespace), client.MatchingLabels{pendingRestoreUIDLabel: string(restore.UID)}); err != nil {
		return err
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.Name == durable.Status.PendingPatchesConfigMap || !ownsPendingSnapshot(durable, cm) {
			continue
		}
		uid := cm.UID
		if err := c.Delete(ctx, cm, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
