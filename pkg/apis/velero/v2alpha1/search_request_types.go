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

package v2alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SearchRequestSpec defines the search filters.
type SearchRequestSpec struct {
	// Query defines the search filters.
	Query SearchQuery `json:"query"`

	// Limit is the maximum number of results returned. Hard capped at 500.
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Maximum=500
	// +kubebuilder:validation:Minimum=1
	Limit int `json:"limit,omitempty"`

	// TTL specifies how long this SearchRequest is retained after completion.
	// Defaults to the server's --search-request-default-ttl (10m).
	// Set to 0 to disable automatic deletion.
	// +optional
	TTL metav1.Duration `json:"ttl,omitempty"`
}

type SearchQuery struct {
	// Name is a glob pattern matched against resource names.
	// Wildcards: * matches any sequence, ? matches one character.
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace filters by exact Kubernetes namespace.
	// Omit to search all namespaces and cluster-scoped resources.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Kind filters by exact resource kind (e.g. "Deployment", "Pod").
	// +optional
	Kind string `json:"kind,omitempty"`

	// APIVersion filters by exact API version (e.g. "apps/v1").
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Labels filters resources where ALL specified key/value pairs match.
	// Multiple entries are AND-ed together.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// BackupName restricts the search to a single named backup.
	// Omit to search across all indexed backups.
	// +optional
	BackupName string `json:"backupName,omitempty"`
}

type SearchRequestStatus struct {
	// Phase is the current lifecycle state.
	// +optional
	Phase SearchRequestPhase `json:"phase,omitempty"`

	// Results holds the matching resource records (capped by spec.limit).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Results []SearchResourceMatch `json:"results,omitempty"`

	// TotalCount is the total number of matching resources before limit.
	// +optional
	TotalCount int `json:"totalCount,omitempty"`

	// Message provides human-readable details when Phase is Failed.
	// +optional
	Message string `json:"message,omitempty"`

	// StartTimestamp is when the Velero server began processing this request.
	// +optional
	StartTimestamp *metav1.Time `json:"startTimestamp,omitempty"`

	// CompletionTimestamp is when processing finished (Processed or Failed).
	// +optional
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`

	// Expiration is when this SearchRequest will be garbage-collected.
	// +optional
	Expiration *metav1.Time `json:"expiration,omitempty"`
}

// +kubebuilder:validation:Enum=New;Processing;Processed;Failed
type SearchRequestPhase string

const (
	SearchRequestPhaseNew        SearchRequestPhase = "New"
	SearchRequestPhaseProcessing SearchRequestPhase = "Processing"
	SearchRequestPhaseProcessed  SearchRequestPhase = "Processed"
	SearchRequestPhaseFailed     SearchRequestPhase = "Failed"
)

type SearchResourceMatch struct {
	BackupName   string            `json:"backupName"`
	ResourceName string            `json:"resourceName"`
	APIVersion   string            `json:"apiVersion"`
	Kind         string            `json:"kind"`
	Namespace    string            `json:"namespace,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SearchRequest is a Velero resource that queries the search index.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=searchrequests,singular=searchrequest,scope=Namespaced
// +kubebuilder:storageversion
type SearchRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SearchRequestSpec   `json:"spec,omitempty"`
	Status            SearchRequestStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SearchRequestList is a list of SearchRequests.
// +kubebuilder:object:root=true
type SearchRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SearchRequest `json:"items"`
}
