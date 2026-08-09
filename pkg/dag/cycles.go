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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// DetectCycles returns diagnostic warning strings for cycles in the ownership graph.
// Cycles must not fail backup or restore; this is supportability only.
func DetectCycles(d *ResourceDAG) []string {
	if d == nil || len(d.Edges) == 0 {
		return nil
	}

	// Build adjacency: parent -> children (ownership direction used for cycle walk).
	// Also walk child -> parent so either encoding of a cycle is found.
	childrenOf := make(map[types.UID][]types.UID)
	parentsOf := make(map[types.UID][]types.UID)
	for _, e := range d.Edges {
		childrenOf[e.ParentUID] = append(childrenOf[e.ParentUID], e.ChildUID)
		parentsOf[e.ChildUID] = append(parentsOf[e.ChildUID], e.ParentUID)
	}

	var warnings []string
	seenCycles := make(map[string]struct{})

	visit := func(adj map[types.UID][]types.UID) {
		const (
			white = 0
			gray  = 1
			black = 2
		)
		color := make(map[types.UID]int)
		var path []types.UID

		var dfs func(u types.UID)
		dfs = func(u types.UID) {
			color[u] = gray
			path = append(path, u)
			for _, v := range adj[u] {
				switch color[v] {
				case white:
					dfs(v)
				case gray:
					// cycle: from v to end of path
					cycleUIDs := collectCycle(path, v)
					key := cycleKey(cycleUIDs)
					if _, ok := seenCycles[key]; !ok {
						seenCycles[key] = struct{}{}
						warnings = append(warnings, fmt.Sprintf("cycle detected involving %s", strings.Join(uidsToStrings(cycleUIDs), ", ")))
					}
				}
			}
			path = path[:len(path)-1]
			color[u] = black
		}

		for u := range adj {
			if color[u] == white {
				dfs(u)
			}
		}
		// Also start from nodes that only appear as leaves / parents not in adj keys as sources.
		for u := range d.Nodes {
			if color[u] == white {
				dfs(u)
			}
		}
	}

	visit(childrenOf)
	visit(parentsOf)
	return warnings
}

func collectCycle(path []types.UID, start types.UID) []types.UID {
	idx := -1
	for i, u := range path {
		if u == start {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []types.UID{start}
	}
	out := append([]types.UID{}, path[idx:]...)
	out = append(out, start)
	return out
}

func cycleKey(uids []types.UID) string {
	if len(uids) == 0 {
		return ""
	}
	// Normalize by rotating to lexicographically smallest UID (excluding closing duplicate).
	core := uids
	if len(uids) > 1 && uids[0] == uids[len(uids)-1] {
		core = uids[:len(uids)-1]
	}
	if len(core) == 0 {
		return ""
	}
	minIdx := 0
	for i := 1; i < len(core); i++ {
		if string(core[i]) < string(core[minIdx]) {
			minIdx = i
		}
	}
	rotated := append(append([]types.UID{}, core[minIdx:]...), core[:minIdx]...)
	return strings.Join(uidsToStrings(rotated), ",")
}

func uidsToStrings(uids []types.UID) []string {
	out := make([]string, len(uids))
	for i, u := range uids {
		out[i] = string(u)
	}
	return out
}
