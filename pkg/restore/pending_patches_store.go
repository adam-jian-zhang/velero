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
	"encoding/json"
	"fmt"
	"io"

	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

const (
	// PendingPatchesDataKey is the key within ConfigMap.BinaryData storing gzipped JSON pending patches.
	PendingPatchesDataKey = "patches.json.gz"

	// PendingPatchesConfigMapSuffix is the naming suffix for ephemeral overflow ConfigMaps.
	PendingPatchesConfigMapSuffix = "pending-patches"

	// MaxOverflowConfigMapBytes is the Kubernetes ConfigMap MaxSecretSize budget for
	// binaryData key plus gzip payload. This design uses a single overflow ConfigMap.
	MaxOverflowConfigMapBytes = 1024 * 1024
)

// overflowSizeLimit is the ConfigMap binaryData budget. Tests may lower it.
var overflowSizeLimit = MaxOverflowConfigMapBytes

// PendingPatchesConfigMapName returns the conventional name for a restore's overflow ConfigMap.
func PendingPatchesConfigMapName(restoreName string) string {
	return fmt.Sprintf("%s-%s", restoreName, PendingPatchesConfigMapSuffix)
}

func overflowConfigMapBytes(gz []byte) int {
	return len(PendingPatchesDataKey) + len(gz)
}

func restoreNamespace(restore *velerov1api.Restore) string {
	if restore != nil && restore.Namespace != "" {
		return restore.Namespace
	}
	return velerov1api.DefaultNamespace
}

// CompressPendingPatches serializes and gzip-compresses a slice of PendingPatchRef.
func CompressPendingPatches(patches []velerov1api.PendingPatchRef) ([]byte, error) {
	if len(patches) == 0 {
		return nil, nil
	}
	rawJSON, err := json.Marshal(patches)
	if err != nil {
		return nil, fmt.Errorf("marshal pending patches: %w", err)
	}

	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	if _, err := gzWriter.Write(rawJSON); err != nil {
		return nil, fmt.Errorf("write compressed pending patches: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("close gzip writer for pending patches: %w", err)
	}
	return buf.Bytes(), nil
}

// DecompressPendingPatches decompresses gzipped JSON data into a slice of PendingPatchRef.
func DecompressPendingPatches(data []byte) ([]velerov1api.PendingPatchRef, error) {
	if len(data) == 0 {
		return nil, nil
	}
	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create gzip reader for pending patches: %w", err)
	}
	defer gzReader.Close()

	rawJSON, err := io.ReadAll(gzReader)
	if err != nil {
		return nil, fmt.Errorf("read compressed pending patches: %w", err)
	}

	var patches []velerov1api.PendingPatchRef
	if err := json.Unmarshal(rawJSON, &patches); err != nil {
		return nil, fmt.Errorf("unmarshal pending patches: %w", err)
	}
	return patches, nil
}

func isRetriableOverflowStoreError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsAlreadyExists(err) {
		return true
	}
	return isRetriablePatchError(err)
}

func circuitBreakOverflow(
	restore *velerov1api.Restore,
	state *ownerref.OwnerRefRelinkState,
	inlinePatches []velerov1api.PendingPatchRef,
	overflowPatches []velerov1api.PendingPatchRef,
	log logrus.FieldLogger,
	warnings *results.Result,
	err error,
) {
	fallbackBlockOverflowRoots(state, overflowPatches)
	restore.Status.PendingOwnerRefPatches = inlinePatches
	restore.Status.PendingPatchesConfigMap = ""
	log.WithError(err).Warn(err.Error())
	warnings.Add(restore.Namespace, err)
}

// SavePendingPatchesHybrid persists pending patches using the hybrid storage model.
// If len(pendingPatches) <= MaxPendingPatches (500), all patches are stored inline in
// restore.Status.PendingOwnerRefPatches and restore.Status.PendingPatchesConfigMap is cleared.
// If len(pendingPatches) > MaxPendingPatches, the top 500 items are stored inline in Status,
// and the overflow items (501 through MaxTotalPendingPatches) are gzipped and stored in a
// single ephemeral ConfigMap. Items beyond MaxTotalPendingPatches, gzip payloads over 1 MiB,
// or ConfigMap write failure after transient retries fall back to the defensive circuit
// breaker: marks root quiesced ancestors of the dropped items as UnquiesceBlocked=true,
// leaves status capped at 500, and records an explicit warning.
func SavePendingPatchesHybrid(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
	pendingPatches []velerov1api.PendingPatchRef,
	state *ownerref.OwnerRefRelinkState,
	log logrus.FieldLogger,
) results.Result {
	warnings := results.Result{}
	if restore == nil {
		return warnings
	}
	if log == nil {
		log = logrus.StandardLogger()
	}

	if len(pendingPatches) > velerov1api.MaxTotalPendingPatches {
		dropped := pendingPatches[velerov1api.MaxTotalPendingPatches:]
		fallbackBlockOverflowRoots(state, dropped)
		err := fmt.Errorf("pending patches exceed MaxTotalPendingPatches (%d); dropped %d items and blocked roots",
			velerov1api.MaxTotalPendingPatches, len(dropped))
		log.Warn(err.Error())
		warnings.Add(restore.Namespace, err)
		pendingPatches = pendingPatches[:velerov1api.MaxTotalPendingPatches]
	}

	total := len(pendingPatches)
	if total <= velerov1api.MaxPendingPatches {
		restore.Status.PendingOwnerRefPatches = pendingPatches
		if restore.Status.PendingPatchesConfigMap != "" && crClient != nil {
			_ = DeletePendingPatchesConfigMap(ctx, crClient, restore)
		}
		restore.Status.PendingPatchesConfigMap = ""
		return warnings
	}

	inlinePatches := pendingPatches[:velerov1api.MaxPendingPatches]
	overflowPatches := pendingPatches[velerov1api.MaxPendingPatches:]

	if crClient == nil {
		circuitBreakOverflow(restore, state, inlinePatches, overflowPatches, log, &warnings,
			fmt.Errorf("no kubernetes client available to create overflow ConfigMap for %d pending patches; overflow dropped and roots blocked", len(overflowPatches)))
		return warnings
	}

	compressedData, err := CompressPendingPatches(overflowPatches)
	if err != nil {
		circuitBreakOverflow(restore, state, inlinePatches, overflowPatches, log, &warnings,
			fmt.Errorf("failed to compress %d overflow pending patches: %w; overflow dropped and roots blocked", len(overflowPatches), err))
		return warnings
	}

	if n := overflowConfigMapBytes(compressedData); n > overflowSizeLimit {
		circuitBreakOverflow(restore, state, inlinePatches, overflowPatches, log, &warnings,
			fmt.Errorf("overflow ConfigMap payload %d bytes exceeds 1MiB limit (%d); overflow dropped and roots blocked", n, overflowSizeLimit))
		return warnings
	}

	cmName, err := writeOverflowConfigMap(ctx, crClient, restore, compressedData)
	if err != nil {
		circuitBreakOverflow(restore, state, inlinePatches, overflowPatches, log, &warnings,
			fmt.Errorf("failed to write overflow ConfigMap for %d patches: %w; overflow dropped and roots blocked", len(overflowPatches), err))
		return warnings
	}

	restore.Status.PendingOwnerRefPatches = inlinePatches
	restore.Status.PendingPatchesConfigMap = cmName
	log.Infof("Saved %d pending patches hybridly: %d in Restore.Status, %d compressed in ConfigMap %s/%s (%d bytes)",
		total, len(inlinePatches), len(overflowPatches), restoreNamespace(restore), cmName, len(compressedData))
	return warnings
}

// LoadPendingPatchesHybrid returns the unified slice of pending patches by loading
// items from Restore.Status.PendingOwnerRefPatches and any overflow items from
// the referenced ConfigMap (Restore.Status.PendingPatchesConfigMap).
// overflowLoadFailed is true when a named overflow ConfigMap could not be loaded
// after transient retries; Pass 2 must not unquiesce or drain the ConfigMap.
func LoadPendingPatchesHybrid(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
) ([]velerov1api.PendingPatchRef, results.Result, bool) {
	warnings := results.Result{}
	if restore == nil {
		return nil, warnings, false
	}

	allPending := append([]velerov1api.PendingPatchRef(nil), restore.Status.PendingOwnerRefPatches...)

	if restore.Status.PendingPatchesConfigMap == "" {
		return allPending, warnings, false
	}

	if crClient == nil {
		warnings.Add(restore.Namespace, fmt.Errorf("cannot load overflow ConfigMap %s: kubernetes client is nil", restore.Status.PendingPatchesConfigMap))
		return allPending, warnings, true
	}

	ns := restoreNamespace(restore)
	cmName := restore.Status.PendingPatchesConfigMap

	var overflowPatches []velerov1api.PendingPatchRef
	err := retry.OnError(retry.DefaultBackoff, isRetriableOverflowStoreError, func() error {
		cm := &corev1api.ConfigMap{}
		if getErr := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cmName}, cm); getErr != nil {
			return getErr
		}
		data, ok := cm.BinaryData[PendingPatchesDataKey]
		if !ok || len(data) == 0 {
			return fmt.Errorf("overflow ConfigMap %s/%s has missing or empty %s", ns, cmName, PendingPatchesDataKey)
		}
		decoded, decErr := DecompressPendingPatches(data)
		if decErr != nil {
			return fmt.Errorf("failed to decompress overflow ConfigMap %s/%s: %w", ns, cmName, decErr)
		}
		overflowPatches = decoded
		return nil
	})
	if err != nil {
		warnings.Add(ns, fmt.Errorf("failed to retrieve overflow ConfigMap %s/%s: %w", ns, cmName, err))
		return allPending, warnings, true
	}

	allPending = append(allPending, overflowPatches...)
	return allPending, warnings, false
}

// ReconcilePendingPatchesConfigMap updates or deletes the overflow ConfigMap based on remaining unresolved patches.
// If remainingPending <= MaxPendingPatches (500), all remaining items are stored inline in
// restore.Status.PendingOwnerRefPatches, restore.Status.PendingPatchesConfigMap is cleared,
// and the overflow ConfigMap is deleted.
// If remainingPending > MaxPendingPatches, top 500 remain in Status and the excess is re-saved
// to the single ConfigMap after the same 1 MiB preflight and transient retries as Pass 1.
// A failed write does not claim overflow was saved (PendingPatchesConfigMap is cleared).
func ReconcilePendingPatchesConfigMap(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
	remainingPending []velerov1api.PendingPatchRef,
) results.Result {
	warnings := results.Result{}
	if restore == nil {
		return warnings
	}

	if len(remainingPending) > velerov1api.MaxTotalPendingPatches {
		dropped := remainingPending[velerov1api.MaxTotalPendingPatches:]
		err := fmt.Errorf("remaining pending patches exceed MaxTotalPendingPatches (%d); dropped %d overflow items",
			velerov1api.MaxTotalPendingPatches, len(dropped))
		warnings.Add(restore.Namespace, err)
		remainingPending = remainingPending[:velerov1api.MaxTotalPendingPatches]
	}

	totalRemaining := len(remainingPending)
	if totalRemaining <= velerov1api.MaxPendingPatches {
		restore.Status.PendingOwnerRefPatches = remainingPending
		if restore.Status.PendingPatchesConfigMap != "" {
			if crClient != nil {
				if err := DeletePendingPatchesConfigMap(ctx, crClient, restore); err != nil {
					warnings.Add(restore.Namespace, fmt.Errorf("failed to delete drained overflow ConfigMap %s: %w", restore.Status.PendingPatchesConfigMap, err))
				}
			}
			restore.Status.PendingPatchesConfigMap = ""
		}
		return warnings
	}

	inlinePatches := remainingPending[:velerov1api.MaxPendingPatches]
	overflowPatches := remainingPending[velerov1api.MaxPendingPatches:]
	restore.Status.PendingOwnerRefPatches = inlinePatches

	if crClient == nil {
		restore.Status.PendingPatchesConfigMap = ""
		warnings.Add(restore.Namespace, fmt.Errorf("cannot update overflow ConfigMap: kubernetes client is nil"))
		return warnings
	}

	compressedData, err := CompressPendingPatches(overflowPatches)
	if err != nil {
		restore.Status.PendingPatchesConfigMap = ""
		warnings.Add(restore.Namespace, fmt.Errorf("failed to compress remaining overflow patches: %w", err))
		return warnings
	}

	if n := overflowConfigMapBytes(compressedData); n > overflowSizeLimit {
		restore.Status.PendingPatchesConfigMap = ""
		warnings.Add(restore.Namespace, fmt.Errorf("overflow ConfigMap payload %d bytes exceeds 1MiB limit (%d); overflow not saved", n, overflowSizeLimit))
		return warnings
	}

	cmName, err := writeOverflowConfigMap(ctx, crClient, restore, compressedData)
	if err != nil {
		restore.Status.PendingPatchesConfigMap = ""
		warnings.Add(restoreNamespace(restore), fmt.Errorf("failed to write overflow ConfigMap: %w", err))
		return warnings
	}

	restore.Status.PendingPatchesConfigMap = cmName
	return warnings
}

func writeOverflowConfigMap(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
	compressedData []byte,
) (string, error) {
	ns := restoreNamespace(restore)
	cmName := restore.Status.PendingPatchesConfigMap
	if cmName == "" {
		cmName = PendingPatchesConfigMapName(restore.Name)
	}

	err := retry.OnError(retry.DefaultBackoff, isRetriableOverflowStoreError, func() error {
		desired := buildOverflowConfigMap(ns, cmName, restore, compressedData)
		existing := &corev1api.ConfigMap{}
		getErr := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cmName}, existing)
		if getErr == nil {
			existing.BinaryData = desired.BinaryData
			if len(desired.OwnerReferences) > 0 {
				existing.OwnerReferences = desired.OwnerReferences
			}
			return crClient.Update(ctx, existing)
		}
		if apierrors.IsNotFound(getErr) {
			return crClient.Create(ctx, desired)
		}
		return getErr
	})
	if err != nil {
		return "", err
	}
	return cmName, nil
}

func buildOverflowConfigMap(ns, cmName string, restore *velerov1api.Restore, compressedData []byte) *corev1api.ConfigMap {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ns,
		},
		BinaryData: map[string][]byte{
			PendingPatchesDataKey: compressedData,
		},
	}
	if restore != nil && restore.UID != "" {
		controller := true
		blockOwnerDeletion := true
		cm.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         velerov1api.SchemeGroupVersion.String(),
				Kind:               "Restore",
				Name:               restore.Name,
				UID:                restore.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			},
		}
	}
	return cm
}

// DeletePendingPatchesConfigMap deletes the ephemeral overflow ConfigMap if referenced by the restore.
func DeletePendingPatchesConfigMap(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
) error {
	if restore == nil || restore.Status.PendingPatchesConfigMap == "" || crClient == nil {
		return nil
	}
	ns := restoreNamespace(restore)
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Status.PendingPatchesConfigMap,
			Namespace: ns,
		},
	}
	err := crClient.Delete(ctx, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func fallbackBlockOverflowRoots(state *ownerref.OwnerRefRelinkState, overflowPatches []velerov1api.PendingPatchRef) {
	if state == nil {
		return
	}
	for _, p := range overflowPatches {
		for _, ref := range p.OwnerReferences {
			ownerGroup := ownerRefGroup(ref.APIVersion)
			state.BlockUnquiesceForTarget(ownerGroup, ref.Kind, p.Namespace, ref.Name)
		}
		for _, root := range p.Targets {
			state.BlockUnquiesceForTarget(root.Group, root.Kind, root.Namespace, root.Name)
		}
	}
}
