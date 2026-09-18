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
)

// PendingPatchesConfigMapName returns the conventional name for a restore's overflow ConfigMap.
func PendingPatchesConfigMapName(restoreName string) string {
	return fmt.Sprintf("%s-%s", restoreName, PendingPatchesConfigMapSuffix)
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

// SavePendingPatchesHybrid persists pending patches using the hybrid storage model.
// If len(pendingPatches) <= MaxPendingPatches (500), all patches are stored inline in
// restore.Status.PendingOwnerRefPatches and restore.Status.PendingPatchesConfigMap is cleared.
// If len(pendingPatches) > MaxPendingPatches, the top 500 items are stored inline in Status,
// and the overflow items (501+) are gzipped and stored in an ephemeral ConfigMap in the
// restore's namespace with an OwnerReference pointing to the Restore CR.
// If ConfigMap creation/update fails, it falls back to the defensive circuit breaker:
// marks root quiesced ancestors of the overflow items as UnquiesceBlocked=true in state,
// leaves status capped at 500, and records a warning.
func SavePendingPatchesHybrid(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
	pendingPatches []velerov1api.PendingPatchRef,
	state *ownerref.OwnerRefRemapState,
	log logrus.FieldLogger,
) results.Result {
	warnings := results.Result{}
	if restore == nil {
		return warnings
	}
	if log == nil {
		log = logrus.StandardLogger()
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

	// Overflow condition (> 500 patches)
	inlinePatches := pendingPatches[:velerov1api.MaxPendingPatches]
	overflowPatches := pendingPatches[velerov1api.MaxPendingPatches:]

	if crClient == nil {
		// No client available: fallback to cap and block roots
		fallbackBlockOverflowRoots(state, overflowPatches)
		restore.Status.PendingOwnerRefPatches = inlinePatches
		restore.Status.PendingPatchesConfigMap = ""
		err := fmt.Errorf("no kubernetes client available to create overflow ConfigMap for %d pending patches; overflow dropped and roots blocked", len(overflowPatches))
		log.Warn(err.Error())
		warnings.Add(restore.Namespace, err)
		return warnings
	}

	compressedData, err := CompressPendingPatches(overflowPatches)
	if err != nil {
		fallbackBlockOverflowRoots(state, overflowPatches)
		restore.Status.PendingOwnerRefPatches = inlinePatches
		restore.Status.PendingPatchesConfigMap = ""
		wrapErr := fmt.Errorf("failed to compress %d overflow pending patches: %w; overflow dropped and roots blocked", len(overflowPatches), err)
		log.WithError(err).Warn(wrapErr.Error())
		warnings.Add(restore.Namespace, wrapErr)
		return warnings
	}

	cmName := PendingPatchesConfigMapName(restore.Name)
	ns := restore.Namespace
	if ns == "" {
		ns = velerov1api.DefaultNamespace
	}

	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ns,
		},
		BinaryData: map[string][]byte{
			PendingPatchesDataKey: compressedData,
		},
	}

	if restore.UID != "" {
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

	existingCM := &corev1api.ConfigMap{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cmName}, existingCM); err == nil {
		// Update existing
		existingCM.BinaryData = cm.BinaryData
		if len(cm.OwnerReferences) > 0 {
			existingCM.OwnerReferences = cm.OwnerReferences
		}
		if updateErr := crClient.Update(ctx, existingCM); updateErr != nil {
			fallbackBlockOverflowRoots(state, overflowPatches)
			restore.Status.PendingOwnerRefPatches = inlinePatches
			restore.Status.PendingPatchesConfigMap = ""
			wrapErr := fmt.Errorf("failed to update overflow ConfigMap %s/%s for %d patches: %w; overflow dropped and roots blocked", ns, cmName, len(overflowPatches), updateErr)
			log.WithError(updateErr).Warn(wrapErr.Error())
			warnings.Add(ns, wrapErr)
			return warnings
		}
	} else if apierrors.IsNotFound(err) {
		// Create new
		if createErr := crClient.Create(ctx, cm); createErr != nil {
			fallbackBlockOverflowRoots(state, overflowPatches)
			restore.Status.PendingOwnerRefPatches = inlinePatches
			restore.Status.PendingPatchesConfigMap = ""
			wrapErr := fmt.Errorf("failed to create overflow ConfigMap %s/%s for %d patches: %w; overflow dropped and roots blocked", ns, cmName, len(overflowPatches), createErr)
			log.WithError(createErr).Warn(wrapErr.Error())
			warnings.Add(ns, wrapErr)
			return warnings
		}
	} else {
		// Unexpected GET error
		fallbackBlockOverflowRoots(state, overflowPatches)
		restore.Status.PendingOwnerRefPatches = inlinePatches
		restore.Status.PendingPatchesConfigMap = ""
		wrapErr := fmt.Errorf("failed to check existing overflow ConfigMap %s/%s: %w; overflow dropped and roots blocked", ns, cmName, err)
		log.WithError(err).Warn(wrapErr.Error())
		warnings.Add(ns, wrapErr)
		return warnings
	}

	restore.Status.PendingOwnerRefPatches = inlinePatches
	restore.Status.PendingPatchesConfigMap = cmName
	log.Infof("Saved %d pending patches hybridly: %d in Restore.Status, %d compressed in ConfigMap %s/%s (%d bytes)",
		total, len(inlinePatches), len(overflowPatches), ns, cmName, len(compressedData))
	return warnings
}

// LoadPendingPatchesHybrid returns the unified slice of pending patches by loading
// items from Restore.Status.PendingOwnerRefPatches and any overflow items from
// the referenced ConfigMap (Restore.Status.PendingPatchesConfigMap).
func LoadPendingPatchesHybrid(
	ctx context.Context,
	crClient client.Client,
	restore *velerov1api.Restore,
) ([]velerov1api.PendingPatchRef, results.Result) {
	warnings := results.Result{}
	if restore == nil {
		return nil, warnings
	}

	allPending := append([]velerov1api.PendingPatchRef(nil), restore.Status.PendingOwnerRefPatches...)

	if restore.Status.PendingPatchesConfigMap == "" {
		return allPending, warnings
	}

	if crClient == nil {
		warnings.Add(restore.Namespace, fmt.Errorf("cannot load overflow ConfigMap %s: kubernetes client is nil", restore.Status.PendingPatchesConfigMap))
		return allPending, warnings
	}

	ns := restore.Namespace
	if ns == "" {
		ns = velerov1api.DefaultNamespace
	}
	cmName := restore.Status.PendingPatchesConfigMap

	cm := &corev1api.ConfigMap{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cmName}, cm); err != nil {
		warnings.Add(ns, fmt.Errorf("failed to retrieve overflow ConfigMap %s/%s: %w", ns, cmName, err))
		return allPending, warnings
	}

	data, ok := cm.BinaryData[PendingPatchesDataKey]
	if !ok || len(data) == 0 {
		warnings.Add(ns, fmt.Errorf("overflow ConfigMap %s/%s has missing or empty %s", ns, cmName, PendingPatchesDataKey))
		return allPending, warnings
	}

	overflowPatches, err := DecompressPendingPatches(data)
	if err != nil {
		warnings.Add(ns, fmt.Errorf("failed to decompress overflow ConfigMap %s/%s: %w", ns, cmName, err))
		return allPending, warnings
	}

	allPending = append(allPending, overflowPatches...)
	return allPending, warnings
}

// ReconcilePendingPatchesConfigMap updates or deletes the overflow ConfigMap based on remaining unresolved patches.
// If remainingPending <= MaxPendingPatches (500), all remaining items are stored inline in
// restore.Status.PendingOwnerRefPatches, restore.Status.PendingPatchesConfigMap is cleared,
// and the overflow ConfigMap is deleted.
// If remainingPending > MaxPendingPatches, top 500 remain in Status and the excess is re-saved to the ConfigMap.
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

	// Still > 500 remaining patches
	inlinePatches := remainingPending[:velerov1api.MaxPendingPatches]
	overflowPatches := remainingPending[velerov1api.MaxPendingPatches:]
	restore.Status.PendingOwnerRefPatches = inlinePatches

	if crClient == nil {
		warnings.Add(restore.Namespace, fmt.Errorf("cannot update overflow ConfigMap: kubernetes client is nil"))
		return warnings
	}

	compressedData, err := CompressPendingPatches(overflowPatches)
	if err != nil {
		warnings.Add(restore.Namespace, fmt.Errorf("failed to compress remaining overflow patches: %w", err))
		return warnings
	}

	ns := restore.Namespace
	if ns == "" {
		ns = velerov1api.DefaultNamespace
	}
	cmName := restore.Status.PendingPatchesConfigMap
	if cmName == "" {
		cmName = PendingPatchesConfigMapName(restore.Name)
	}

	cm := &corev1api.ConfigMap{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cmName}, cm); err == nil {
		cm.BinaryData = map[string][]byte{
			PendingPatchesDataKey: compressedData,
		}
		if updateErr := crClient.Update(ctx, cm); updateErr != nil {
			warnings.Add(ns, fmt.Errorf("failed to update overflow ConfigMap %s/%s: %w", ns, cmName, updateErr))
		} else {
			restore.Status.PendingPatchesConfigMap = cmName
		}
	} else if apierrors.IsNotFound(err) {
		cm = &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
			},
			BinaryData: map[string][]byte{
				PendingPatchesDataKey: compressedData,
			},
		}
		if restore.UID != "" {
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
		if createErr := crClient.Create(ctx, cm); createErr != nil {
			warnings.Add(ns, fmt.Errorf("failed to recreate overflow ConfigMap %s/%s: %w", ns, cmName, createErr))
		} else {
			restore.Status.PendingPatchesConfigMap = cmName
		}
	} else {
		warnings.Add(ns, fmt.Errorf("failed to check overflow ConfigMap %s/%s: %w", ns, cmName, err))
	}

	return warnings
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
	ns := restore.Namespace
	if ns == "" {
		ns = velerov1api.DefaultNamespace
	}
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

func fallbackBlockOverflowRoots(state *ownerref.OwnerRefRemapState, overflowPatches []velerov1api.PendingPatchRef) {
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
