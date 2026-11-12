package resourcepolicies

import (
	corev1api "k8s.io/api/core/v1"
)

// VolumeFilterData bundles the volume data needed for volume policy filtering
type VolumeFilterData struct {
	PersistentVolume *corev1api.PersistentVolume
	PodVolume        *corev1api.Volume
	PVC              *corev1api.PersistentVolumeClaim
}

// VolumeMode returns the actual source mode, defaulting to Filesystem as in the
// Kubernetes API. Prefer the PV when it is available.
func (v VolumeFilterData) VolumeMode() corev1api.PersistentVolumeMode {
	if v.PersistentVolume != nil && v.PersistentVolume.Spec.VolumeMode != nil {
		return *v.PersistentVolume.Spec.VolumeMode
	}
	if v.PVC != nil && v.PVC.Spec.VolumeMode != nil {
		return *v.PVC.Spec.VolumeMode
	}
	return corev1api.PersistentVolumeFilesystem
}

// NewVolumeFilterData constructs a new VolumeFilterData instance.
func NewVolumeFilterData(pv *corev1api.PersistentVolume, podVol *corev1api.Volume, pvc *corev1api.PersistentVolumeClaim) VolumeFilterData {
	return VolumeFilterData{
		PersistentVolume: pv,
		PodVolume:        podVol,
		PVC:              pvc,
	}
}
