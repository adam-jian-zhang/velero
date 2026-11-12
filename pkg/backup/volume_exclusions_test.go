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
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"

	"github.com/velero-io/velero/internal/resourcepolicies"
	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/features"
	biav2 "github.com/velero-io/velero/pkg/plugin/velero/backupitemaction/v2"
	"github.com/velero-io/velero/pkg/test"
)

func TestBackupWarnsBeforeSkippingVolumePaths(t *testing.T) {
	features.NewFeatureFlagSet("EnableCSI")
	t.Cleanup(func() { features.NewFeatureFlagSet("") })
	for _, action := range []string{"skip", "custom"} {
		for _, path := range []string{"pod", "pvc", "pv", "all"} {
			t.Run(action+"/"+path, func(t *testing.T) {
				log, hook := logtest.NewNullLogger()
				pool := StartItemBlockWorkerPool(t.Context(), 1, log)
				defer pool.Stop()
				h := newHarness(t, pool)
				pv := builder.ForPersistentVolume("pv-1").ClaimRef("ns", "pvc-1").Result()
				pvc := builder.ForPersistentVolumeClaim("ns", "pvc-1").VolumeName("pv-1").Phase(corev1api.ClaimBound).Result()
				pod := builder.ForPod("ns", "pod-1").Volumes(builder.ForVolume("data").PersistentVolumeClaimSource("pvc-1").Result()).Result()
				global := builder.ForConfigMap("velero", "global").Data("policy", "version: v1\nvolumePolicies:\n- conditions: {}\n  action: {type: fs-backup}\n  exclude: ['*.log']\n").Result()
				local := builder.ForConfigMap("velero", "local").Data("policy", fmt.Sprintf("version: v1\nvolumePolicies:\n- conditions: {}\n  action: {type: %s}\n  exclude: []\n", action)).Result()
				cli := test.NewFakeControllerRuntimeClient(t, pv, pvc, pod, global, local)
				h.backupper.kbClient = cli
				backup := defaultBackup().ResourcePolicies("local").Result()
				policies, err := resourcepolicies.GetResourcePoliciesFromBackupWithGlobal(*backup, cli, "global", "velero", log)
				require.NoError(t, err)
				req := &Request{Backup: backup, ResPolicies: policies, SkippedVolumeTracker: NewSkipVolumeTracker(), BackedUpItems: NewBackedUpItemsMap(), WorkerPool: pool}
				if path == "pod" || path == "all" {
					h.addItems(t, test.Pods(pod))
				}
				if path == "pvc" || path == "all" {
					h.addItems(t, test.PVCs(pvc))
				}
				if path == "pv" || path == "all" {
					h.addItems(t, test.PVs(pv))
				}
				recording := new(recordResourcesAction).WithName(csiBIAPluginName).ForResource("persistentvolumeclaims")
				require.NoError(t, h.backupper.Backup(log, req, new(bytes.Buffer), []biav2.BackupItemAction{recording}, nil, nil))
				warnings := 0
				for _, entry := range hook.AllEntries() {
					if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "exclusions from global baseline were dropped") {
						warnings++
						require.Contains(t, entry.Message, "ns/pvc-1")
						require.Contains(t, entry.Message, action)
					}
				}
				require.Equal(t, 1, warnings, "the actual skip path must warn exactly once per PVC")
				require.Empty(t, req.PodVolumeBackups)
			})
		}
	}
}

func TestBackupWarnsForUnfilteredNativeSnapshot(t *testing.T) {
	log, hook := logtest.NewNullLogger()
	pool := StartItemBlockWorkerPool(t.Context(), 1, log)
	defer pool.Stop()
	h := newHarness(t, pool)
	pv := builder.ForPersistentVolume("pv-1").Result()
	h.addItems(t, test.PVs(pv))
	p := &resourcepolicies.Policies{}
	require.NoError(t, p.BuildPolicy(&resourcepolicies.ResourcePolicies{Version: "v1", VolumePolicies: []resourcepolicies.VolumePolicy{{Action: resourcepolicies.Action{Type: resourcepolicies.Snapshot}, Exclude: []string{"*.tmp"}}}}))
	req := &Request{Backup: defaultBackup().Result(), ResPolicies: p, SnapshotLocations: []*velerov1.VolumeSnapshotLocation{newSnapshotLocation("velero", "default", "default")}, SkippedVolumeTracker: NewSkipVolumeTracker(), BackedUpItems: NewBackedUpItemsMap(), WorkerPool: pool}
	getter := volumeSnapshotterGetter{"default": new(fakeVolumeSnapshotter).WithVolume("pv-1", "vol-1", "", "type-1", 100, false)}
	require.NoError(t, h.backupper.Backup(log, req, new(bytes.Buffer), nil, nil, getter))
	require.Len(t, req.VolumeSnapshots.Get(), 1)
	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "native snapshots do not filter") {
			found = true
			require.Contains(t, entry.Message, "pv-1")
			require.Equal(t, []string{"*.tmp"}, entry.Data["exclude"])
		}
	}
	require.True(t, found)
}
