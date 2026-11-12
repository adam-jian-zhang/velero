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
package backup

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/resourcepolicies"
	"github.com/velero-io/velero/pkg/util/kube"
)

func (ib *itemBackupper) policyVolumeForPVC(pvc *corev1api.PersistentVolumeClaim) (resourcepolicies.VolumeFilterData, error) {
	pv, err := kube.GetPVForVolumePolicy(pvc, ib.kbClient)
	return resourcepolicies.NewVolumeFilterData(pv, nil, pvc), err
}

func (ib *itemBackupper) policyVolumeForPV(pv *corev1api.PersistentVolume) (resourcepolicies.VolumeFilterData, error) {
	var pvc *corev1api.PersistentVolumeClaim
	if pv.Spec.ClaimRef != nil {
		pvc = new(corev1api.PersistentVolumeClaim)
		if err := ib.kbClient.Get(context.Background(), kbclient.ObjectKey{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name}, pvc); err != nil {
			return resourcepolicies.VolumeFilterData{}, fmt.Errorf("resolve PVC for PV %s: %w", pv.Name, err)
		}
	}
	return resourcepolicies.NewVolumeFilterData(pv, nil, pvc), nil
}

// checkSkippedVolumeExclusions runs before the outer backup paths bypass a
// mover. Warnings are deduplicated across pods, PVC actions, and PV processing.
func (ib *itemBackupper) checkSkippedVolumeExclusions(vfd resourcepolicies.VolumeFilterData, log logrus.FieldLogger) (*resourcepolicies.Action, error) {
	action, err := ib.backupRequest.ResPolicies.GetMatchAction(vfd)
	if err != nil {
		return nil, err
	}
	if action != nil && (action.Type == resourcepolicies.Skip || action.Type == resourcepolicies.Custom) {
		if err := ib.warnUnappliedVolumeExclusions(vfd, log, fmt.Sprintf("selected action %q cannot filter source data", action.Type)); err != nil {
			return nil, err
		}
	}
	return action, nil
}

func (ib *itemBackupper) checkPodVolumeExclusions(pod *corev1api.Pod, volume *corev1api.Volume, log logrus.FieldLogger) error {
	if ib.backupRequest.ResPolicies == nil {
		return nil
	}
	vfd := resourcepolicies.NewVolumeFilterData(nil, volume, nil)
	if volume.PersistentVolumeClaim != nil {
		pvc, err := kube.GetPVCForPodVolume(volume, pod, ib.kbClient)
		if err != nil {
			return err
		}
		vfd, err = ib.policyVolumeForPVC(pvc)
		if err != nil {
			return err
		}
	}
	_, err := ib.checkSkippedVolumeExclusions(vfd, log.WithFields(logrus.Fields{"pod": pod.Namespace + "/" + pod.Name, "podVolume": volume.Name}))
	return err
}

func (ib *itemBackupper) warnUnappliedVolumeExclusions(vfd resourcepolicies.VolumeFilterData, log logrus.FieldLogger, reason string) error {
	patterns, dropped, err := ib.backupRequest.ResPolicies.GetEffectiveExclude(vfd)
	if err != nil {
		return err
	}
	if !dropped && len(patterns) == 0 {
		return nil
	}
	identity := ""
	switch {
	case vfd.PVC != nil:
		identity = "PVC " + vfd.PVC.Namespace + "/" + vfd.PVC.Name
	case vfd.PersistentVolume != nil:
		identity = "PV " + vfd.PersistentVolume.Name
	case vfd.PodVolume != nil:
		// Non-PVC volumes have no identity shared across backup items.
		log.WithField("exclude", patterns).Warnf("Volume policy exclusions were dropped for pod volume %s: %s", vfd.PodVolume.Name, reason)
		return nil
	}
	if _, loaded := ib.backupRequest.exclusionWarnings.LoadOrStore(identity, struct{}{}); loaded {
		return nil
	}
	if dropped {
		action, err := ib.backupRequest.ResPolicies.GetMatchAction(vfd)
		if err != nil {
			return err
		}
		if action != nil {
			log = log.WithField("action", action.Type)
			if action.Type == resourcepolicies.Snapshot {
				mover, err := action.GetDataMover()
				if err != nil {
					return err
				}
				log = log.WithField("dataMover", mover)
			}
		}
		log.Warnf("Volume policy exclusions from global baseline were dropped for %s: %s", identity, reason)
	} else {
		log.WithField("exclude", patterns).Warnf("Volume policy exclude patterns are not applied to %s: %s", identity, reason)
	}
	return nil
}
