/*
Copyright the Velero contributors.

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

package restoreorder

import (
	"container/heap"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	api "github.com/velero-io/velero/pkg/apis/velero/v1"
)

// CRDs identifies the resource handled by the dedicated first pass.
const CRDs = "customresourcedefinitions.apiextensions.k8s.io"

// Rule retains the origin of an ordering relationship for validation diagnostics.
// An empty Source denotes the corresponding inline Restore spec index.
type Rule struct {
	Before string
	After  string
	Source string
}

func InlineRules(rules []api.ResourceRestoreOrder) []Rule {
	result := make([]Rule, len(rules))
	for i, rule := range rules {
		result[i] = Rule{Before: rule.Before, After: rule.After}
	}
	return result
}

func (r Rule) source(index int) string {
	if r.Source != "" {
		return r.Source
	}
	return fmt.Sprintf("spec.resourceRestoreOrder[%d]", index)
}

// Validate checks names before any archive reads, including endpoints absent from the backup.
func Validate(rules []Rule) error {
	for i, rule := range rules {
		for _, name := range []string{rule.After, rule.Before} {
			resource, group, qualified := strings.Cut(name, ".")
			if len(validation.IsDNS1035Label(resource)) != 0 || (qualified && len(validation.IsDNS1123Subdomain(group)) != 0) {
				return fmt.Errorf("%s: invalid GroupResource %q", rule.source(i), name)
			}
		}
		if rule.After == rule.Before {
			return fmt.Errorf("%s: %s cannot be restored before itself", rule.source(i), rule.After)
		}
	}
	return nil
}

type Plan struct {
	Order            []string
	MissingEndpoints []string
}

type tier int

const (
	crdTier tier = iota
	highTier
	middleTier
	lowTier
)

func (t tier) String() string {
	return [...]string{"CRD", "high", "unlisted", "low"}[t]
}

type position struct {
	tier tier
	rank int
}

// Merge plans the main resource passes. All inputs use canonical GroupResources;
// endpoint syntax validation and server alias resolution happen before this step.
func Merge(archive, high, low []string, rules []Rule) (Plan, error) {
	var groups [4][]string
	archived := make(map[string]bool, len(archive))
	positions := make(map[string]position, len(archive))
	for _, resource := range archive {
		archived[resource] = true
	}
	add := func(t tier, resources []string) {
		for _, resource := range resources {
			if _, exists := positions[resource]; exists || !archived[resource] {
				continue
			}
			positions[resource] = position{tier: t, rank: len(groups[t])}
			groups[t] = append(groups[t], resource)
		}
	}
	add(crdTier, []string{CRDs})
	add(highTier, high)
	add(lowTier, low)
	middle := slices.Clone(archive)
	slices.Sort(middle)
	add(middleTier, middle)

	missing := map[string]bool{}
	followers := make(map[string][]string)
	indegree := make(map[string]int)
	type edge struct{ from, to string }
	ruleIndices := make(map[edge]int)
	for index, rule := range rules {
		afterResource, beforeResource := rule.After, rule.Before
		if afterResource == beforeResource {
			return Plan{}, fmt.Errorf("%s: %s cannot be restored before itself", rule.source(index), afterResource)
		}
		before, hasBefore := positions[beforeResource]
		after, hasAfter := positions[afterResource]
		if !hasBefore {
			missing[beforeResource] = true
		}
		if !hasAfter {
			missing[afterResource] = true
		}
		if !hasBefore || !hasAfter {
			continue
		}

		if before.tier > after.tier ||
			(before.tier == after.tier && before.tier != middleTier && before.rank > after.rank) {
			precedence := fmt.Sprintf("%s before %s", after.tier, before.tier)
			if after.tier == before.tier {
				precedence = fmt.Sprintf("%s list positions %d and %d", after.tier, after.rank+1, before.rank+1)
			}
			return Plan{}, fmt.Errorf("resource restore order cycle: %s -> %s (server %s), %s -> %s (%s)",
				afterResource, beforeResource, precedence, beforeResource, afterResource, rule.source(index))
		}
		if before.tier != middleTier || after.tier != middleTier {
			continue
		}
		key := edge{from: beforeResource, to: afterResource}
		if _, exists := ruleIndices[key]; exists {
			continue
		}
		ruleIndices[key] = index
		followers[beforeResource] = append(followers[beforeResource], afterResource)
		indegree[afterResource]++
	}

	orderedMiddle := make([]string, 0, len(groups[middleTier]))
	ready := &resourceHeap{}
	for _, resource := range groups[middleTier] {
		if indegree[resource] == 0 {
			heap.Push(ready, resource)
		}
	}
	for ready.Len() > 0 {
		resource := heap.Pop(ready).(string)
		orderedMiddle = append(orderedMiddle, resource)
		for _, afterResource := range followers[resource] {
			indegree[afterResource]--
			if indegree[afterResource] == 0 {
				heap.Push(ready, afterResource)
			}
		}
	}
	if len(orderedMiddle) != len(groups[middleTier]) {
		for _, afterResources := range followers {
			slices.Sort(afterResources)
		}
		cycle := findCycle(groups[middleTier], followers, indegree)
		steps := make([]string, 0, len(cycle)-1)
		for i := 1; i < len(cycle); i++ {
			from, to := cycle[i-1], cycle[i]
			steps = append(steps, fmt.Sprintf("%s -> %s (%s)",
				from, to, rules[ruleIndices[edge{from: from, to: to}]].source(ruleIndices[edge{from: from, to: to}])))
		}
		return Plan{}, fmt.Errorf("resource restore order cycle: %s", strings.Join(steps, ", "))
	}

	plan := Plan{Order: slices.Concat(groups[crdTier], groups[highTier], orderedMiddle, groups[lowTier])}
	for resource := range missing {
		plan.MissingEndpoints = append(plan.MissingEndpoints, resource)
	}
	slices.Sort(plan.MissingEndpoints)
	return plan, nil
}

func findCycle(resources []string, followers map[string][]string, indegree map[string]int) []string {
	active := make(map[string]int)
	visited := make(map[string]bool)
	var stack []string
	var visit func(string) []string
	visit = func(resource string) []string {
		if index, exists := active[resource]; exists {
			return append(slices.Clone(stack[index:]), resource)
		}
		if visited[resource] || indegree[resource] == 0 {
			return nil
		}
		active[resource] = len(stack)
		stack = append(stack, resource)
		visited[resource] = true
		for _, afterResource := range followers[resource] {
			if cycle := visit(afterResource); cycle != nil {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		delete(active, resource)
		return nil
	}
	for _, resource := range resources {
		if cycle := visit(resource); cycle != nil {
			return cycle
		}
	}
	return nil
}

type resourceHeap []string

func (h resourceHeap) Len() int           { return len(h) }
func (h resourceHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h resourceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *resourceHeap) Push(value any) {
	*h = append(*h, value.(string))
}

func (h *resourceHeap) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	*h = (*h)[:last]
	return value
}
