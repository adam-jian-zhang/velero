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

package velero

import "context"

// SearchProvider is a Velero plugin interface for indexing and querying
// Kubernetes resource metadata extracted from Velero backup tarballs.
type SearchProvider interface {
	// Init initializes the backend with driver-specific configuration.
	// Called once after the plugin process starts, and again after any
	// subprocess restart (with the originally supplied config).
	Init(config map[string]string) error

	// IndexBackup downloads the backup tarball at tarballURL, parses resource
	// metadata, and stores records keyed by backupName. Implementations MUST
	// be idempotent: re-indexing the same backup replaces (or is a no-op for)
	// existing rows for that backup. Implementations SHOULD stream records to
	// storage rather than accumulating them in memory.
	IndexBackup(ctx context.Context, backupName, tarballURL string) error

	// DeleteBackup removes all indexed records for backupName, including the
	// processed_backups marker. MUST be idempotent (no error if absent).
	DeleteBackup(ctx context.Context, backupName string) error

	// Search queries indexed records matching params and returns a page of
	// results.
	Search(ctx context.Context, params SearchParams) (SearchResult, error)

	// Ready reports whether the initial index load (cold-start backfill) has
	// completed and the backend can serve queries.
	Ready(ctx context.Context) (bool, error)

	// ListIndexedBackups returns the names of backups currently present in the
	// index (from processed_backups). Used by SearchIndexController for
	// restart-safe idempotency.
	ListIndexedBackups(ctx context.Context) ([]string, error)

	// MarkReady signals that the server-side cold-start backfill has finished.
	// Built-in providers flip Ready() to true; external plugins may no-op and
	// manage Ready themselves.
	MarkReady(ctx context.Context) error
}

// SearchParams defines the query filters.
type SearchParams struct {
	Name       string            // glob: * any sequence, ? one char; empty = match all
	Namespace  string            // exact match; empty matches all namespaces (incl. cluster-scoped)
	Kind       string            // exact match (e.g. "Deployment")
	APIVersion string            // exact match (e.g. "apps/v1")
	Labels     map[string]string // all entries AND-ed
	BackupName string            // restrict to one backup; empty searches all
	Limit      int               // default 100, max 500
	Offset     int               // pagination offset for REST/gRPC; 0 for CRD
}

// SearchResult is a page of matching resource records.
type SearchResult struct {
	Records    []ResourceRecord
	TotalCount int // total matches before limit/offset
}

// ResourceRecord is a single indexed resource entry.
type ResourceRecord struct {
	BackupName   string
	ResourceName string
	APIVersion   string
	Kind         string
	Namespace    string // empty for cluster-scoped
	Labels       map[string]string
}
