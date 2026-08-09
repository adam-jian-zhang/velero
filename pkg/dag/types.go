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

package dag

import (
	"k8s.io/apimachinery/pkg/types"
)

// ResourceNode represents an individual K8s API object in the dependency graph.
type ResourceNode struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// ResourceEdge represents a directed ownership edge: parent owns child.
type ResourceEdge struct {
	ParentUID types.UID `json:"parentUID"`
	ChildUID  types.UID `json:"childUID"`
}

// ResourceDAG is stored as velero-owner-dag.json in the backup archive root.
// Naming is historical; the structure is a directed dependency graph, not a
// guaranteed DAG. See Warnings for cycle / skip diagnostics.
type ResourceDAG struct {
	Version  string                     `json:"version"` // "v1"
	Nodes    map[types.UID]ResourceNode `json:"nodes"`
	Edges    []ResourceEdge             `json:"edges"`
	Warnings []string                   `json:"warnings,omitempty"`
}

const ResourceDAGVersion = "v1"
