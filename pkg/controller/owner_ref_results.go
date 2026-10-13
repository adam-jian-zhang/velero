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
	"fmt"

	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/persistence"
	"github.com/velero-io/velero/pkg/util/results"
)

// updateOwnerRefResults makes the results write replay-safe independently of the
// subsequent Restore status commit. Messages uploaded by an earlier attempt
// remain authoritative even when that attempt's error no longer reproduces.
func updateOwnerRefResults(store persistence.BackupStore, restore *velerov1.Restore, warnings, errs *results.Result) error {
	previous, err := store.GetRestoreResults(restore.Name)
	if err != nil {
		return fmt.Errorf("error getting restore results: %w", err)
	}
	mergedWarnings := mergeOwnerRefResults(previous["warnings"], *warnings)
	mergedErrors := mergeOwnerRefResults(previous["errors"], *errs)
	if err := putResults(restore, map[string]results.Result{"warnings": mergedWarnings, "errors": mergedErrors}, store); err != nil {
		return fmt.Errorf("error putting restore results: %w", err)
	}
	// Never erase existing error counts if an earlier results upload was lost.
	restore.Status.Warnings = max(restore.Status.Warnings, ownerRefResultCount(mergedWarnings))
	restore.Status.Errors = max(restore.Status.Errors, ownerRefResultCount(mergedErrors))
	return nil
}

func mergeOwnerRefResults(previous, incoming results.Result) results.Result {
	merged := results.Result{
		Velero:  mergeOwnerRefMessages(previous.Velero, incoming.Velero),
		Cluster: mergeOwnerRefMessages(previous.Cluster, incoming.Cluster),
	}
	for ns, messages := range previous.Namespaces {
		if merged.Namespaces == nil {
			merged.Namespaces = make(map[string][]string)
		}
		merged.Namespaces[ns] = append([]string(nil), messages...)
	}
	for ns, messages := range incoming.Namespaces {
		if merged.Namespaces == nil {
			merged.Namespaces = make(map[string][]string)
		}
		merged.Namespaces[ns] = mergeOwnerRefMessages(merged.Namespaces[ns], messages)
	}
	return merged
}

func mergeOwnerRefMessages(previous, incoming []string) []string {
	merged := append([]string(nil), previous...)
	seen := make(map[string]bool, len(previous)+len(incoming))
	for _, message := range previous {
		seen[message] = true
	}
	for _, message := range incoming {
		if !seen[message] {
			merged = append(merged, message)
			seen[message] = true
		}
	}
	return merged
}

func ownerRefResultCount(result results.Result) int {
	count := len(result.Velero) + len(result.Cluster)
	for _, messages := range result.Namespaces {
		count += len(messages)
	}
	return count
}
