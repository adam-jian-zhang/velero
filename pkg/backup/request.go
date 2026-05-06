/*
Copyright 2020 the Velero contributors.

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
	"sync"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/vmware-tanzu/velero/internal/hook"
	"github.com/vmware-tanzu/velero/internal/resourcepolicies"
	"github.com/vmware-tanzu/velero/internal/volume"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/itemoperation"
	"github.com/vmware-tanzu/velero/pkg/plugin/framework"
	"github.com/vmware-tanzu/velero/pkg/util/collections"
)

type itemKey struct {
	resource  string
	namespace string
	name      string
}

// ResolvedResourceFilter holds the materialized filter state for one kind-group
// within a namespace.
type ResolvedResourceFilter struct {
	LabelSelector    labels.Selector
	OrLabelSelectors []labels.Selector
	NameIE           *collections.IncludesExcludes
}

// ResolvedNamespaceFilter holds the materialized filter state for a namespace.
// ResourceFilterMap is keyed by the resolved group-resource string.
type ResolvedNamespaceFilter struct {
	ResourceFilterMap map[string]*ResolvedResourceFilter
	Empty             bool
	SkipEntirely      bool
}

type SynchronizedVSList struct {
	sync.Mutex
	VolumeSnapshotList []*volume.Snapshot
}

func (s *SynchronizedVSList) Add(vs *volume.Snapshot) {
	s.Lock()
	defer s.Unlock()
	s.VolumeSnapshotList = append(s.VolumeSnapshotList, vs)
}

func (s *SynchronizedVSList) Get() []*volume.Snapshot {
	s.Lock()
	defer s.Unlock()
	return s.VolumeSnapshotList
}

// Request is a request for a backup, with all references to other objects
// materialized (e.g. backup/snapshot locations, includes/excludes, etc.)
type Request struct {
	*velerov1api.Backup
	StorageLocation           *velerov1api.BackupStorageLocation
	SnapshotLocations         []*velerov1api.VolumeSnapshotLocation
	NamespaceIncludesExcludes *collections.NamespaceIncludesExcludes
	ResourceIncludesExcludes  collections.IncludesExcludesInterface
	ResourceHooks             []hook.ResourceHook
	ResolvedActions           []framework.BackupItemResolvedActionV2
	ResolvedItemBlockActions  []framework.ItemBlockResolvedAction
	VolumeSnapshots           SynchronizedVSList
	PodVolumeBackups          []*velerov1api.PodVolumeBackup
	BackedUpItems             *backedUpItemsMap
	itemOperationsList        *[]*itemoperation.BackupOperation
	ResPolicies               *resourcepolicies.Policies
	SkippedPVTracker          *skipPVTracker
	VolumesInformation        volume.BackupVolumesInformation
	WorkerPool                *ItemBlockWorkerPool

	// ClusterScopedFilterMap holds resolved global filters for cluster-scoped resources.
	// Key is the resolved group-resource string.
	ClusterScopedFilterMap map[string]*ResolvedResourceFilter

	// NamespacedFilterMap holds resolved per-namespace filters.
	// Key is either an exact namespace name or a glob pattern.
	NamespacedFilterMap map[string]*ResolvedNamespaceFilter

	// NamespacedFilterPatterns preserves the order of patterns for first-match semantics
	NamespacedFilterPatterns []string
}

// BackupVolumesInformation contains the information needs by generating
// the backup BackupVolumeInfo array.

// GetItemOperationsList returns ItemOperationsList, initializing it if necessary
func (r *Request) GetItemOperationsList() *[]*itemoperation.BackupOperation {
	if r.itemOperationsList == nil {
		list := []*itemoperation.BackupOperation{}
		r.itemOperationsList = &list
	}
	return r.itemOperationsList
}

// BackupResourceList returns the list of backed up resources grouped by the API
// Version and Kind
func (r *Request) BackupResourceList() map[string][]string {
	return r.BackedUpItems.ResourceMap()
}

func (r *Request) FillVolumesInformation() {
	skippedPVMap := make(map[string]string)

	for _, skippedPV := range r.SkippedPVTracker.Summary() {
		skippedPVMap[skippedPV.Name] = skippedPV.SerializeSkipReasons()
	}

	r.VolumesInformation.SkippedPVs = skippedPVMap
	r.VolumesInformation.NativeSnapshots = r.VolumeSnapshots.Get()
	r.VolumesInformation.PodVolumeBackups = r.PodVolumeBackups
	r.VolumesInformation.BackupOperations = *r.GetItemOperationsList()
	r.VolumesInformation.BackupName = r.Backup.Name
}

func (r *Request) StopWorkerPool() {
	r.WorkerPool.Stop()
}

// GetNamespaceFilter returns the resolved filter for a namespace, or nil
// if the namespace should use global filters. Uses first-match semantics
// when multiple patterns could match the same namespace.
func (r *Request) GetNamespaceFilter(namespace string) *ResolvedNamespaceFilter {
	if r.NamespacedFilterMap == nil {
		return nil
	}

	return r.NamespacedFilterMap[namespace]
}
