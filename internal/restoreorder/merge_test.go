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
	"slices"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	archive := []string{"c", "a", "b", "unrelated", "high2", "high1", "low2", "low1", CRDs}
	high := []string{"not-backed-up", "high1", "high2", "high1", CRDs}
	low := []string{"high2", "low1", "low2", CRDs}
	tests := []struct {
		name      string
		rules     []Rule
		order     []string
		missing   []string
		wantError []string
	}{
		{
			name:  "baseline",
			order: []string{CRDs, "high1", "high2", "a", "b", "c", "unrelated", "low1", "low2"},
		},
		{
			name: "chain plus compatible server edges",
			rules: []Rule{
				{Before: "c", After: "a"}, {Before: "a", After: "b"},
				{Before: "c", After: "a"}, {Before: "high2", After: "c"},
				{Before: "b", After: "low1"}, {Before: "high1", After: "high2"},
			},
			order: []string{CRDs, "high1", "high2", "c", "a", "b", "unrelated", "low1", "low2"},
		},
		{
			name: "absent endpoint does not bridge two edges",
			rules: []Rule{
				{Before: "absent", After: "a"},
				{Before: "c", After: "absent"},
			},
			order:   []string{CRDs, "high1", "high2", "a", "b", "c", "unrelated", "low1", "low2"},
			missing: []string{"absent"},
		},
		{
			name:      "self-ordering is invalid even when absent",
			rules:     []Rule{{Before: "absent", After: "absent"}},
			wantError: []string{"spec.resourceRestoreOrder[0]", "absent cannot be restored before itself"},
		},
		{
			name:      "fixed high list conflict",
			rules:     []Rule{{Before: "high2", After: "high1"}},
			wantError: []string{"high1 -> high2 (server", "high2 -> high1 (spec."},
		},
		{
			name:      "fixed low list conflict",
			rules:     []Rule{{Before: "low2", After: "low1"}},
			wantError: []string{"low1 -> low2 (server", "low2 -> low1 (spec."},
		},
		{
			name:      "tier conflict",
			rules:     []Rule{{Before: "low2", After: "a"}},
			wantError: []string{"a -> low2 (server priority: unlisted before low)", "low2 -> a (spec."},
		},
		{
			name:      "CRD conflict",
			rules:     []Rule{{Before: "a", After: CRDs}},
			wantError: []string{CRDs + " -> a (server priority: CRD before unlisted)", "a -> " + CRDs + " (spec."},
		},
		{
			name: "cycle excludes a blocked afterResource",
			rules: []Rule{
				{Before: "b", After: "a"}, {Before: "c", After: "b"},
				{Before: "a", After: "c"}, {Before: "b", After: "unrelated"},
			},
			wantError: []string{"Resource restore order cycle:\n  a -> c (spec.resourceRestoreOrder[2])\n  c -> b", "c -> b (spec.resourceRestoreOrder[1])", "b -> a (spec.resourceRestoreOrder[0])"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Merge(archive, high, low, tt.rules)
			if len(tt.wantError) != 0 {
				if err == nil {
					t.Fatal("expected validation error")
				}
				for _, want := range tt.wantError {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if strings.Contains(err.Error(), "unrelated") || len(got.Order) != 0 {
					t.Errorf("error includes a blocked afterResource or produces a partial plan: %+v, %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Order, tt.order) || !slices.Equal(got.MissingEndpoints, tt.missing) {
				t.Errorf("got %+v, want %v with missing %v", got, tt.order, tt.missing)
			}
		})
	}
}

func TestMergeTektonPriorityPlacement(t *testing.T) {
	const (
		task        = "tasks.tekton.dev"
		pipeline    = "pipelines.tekton.dev"
		pipelineRun = "pipelineruns.tekton.dev"
	)
	archive := []string{pipelineRun, pipeline, task}
	rules := []Rule{
		{Before: task, After: pipeline},
		{Before: pipeline, After: pipelineRun},
	}
	tests := []struct {
		name      string
		high, low []string
		conflict  string
	}{
		{name: "all unlisted"},
		{name: "all high", high: []string{task, pipeline, pipelineRun}},
		{name: "all low", low: []string{task, pipeline, pipelineRun}},
		{name: "reversed high", high: []string{pipeline, task, pipelineRun}, conflict: "spec.resourceRestoreOrder[0]"},
		{name: "reversed low", low: []string{task, pipelineRun, pipeline}, conflict: "spec.resourceRestoreOrder[1]"},
		{name: "high and low", high: []string{task, pipeline}, low: []string{pipelineRun}},
		{name: "across all tiers", high: []string{task}, low: []string{pipelineRun}},
		{name: "high and unlisted", high: []string{task}},
		{name: "low beforeResource, high afterResource", high: []string{pipeline}, low: []string{task, pipelineRun}, conflict: "spec.resourceRestoreOrder[0]"},
		{name: "unlisted beforeResource, high afterResource", high: []string{pipeline}, conflict: "spec.resourceRestoreOrder[0]"},
		{name: "low beforeResource, unlisted afterResource", low: []string{task}, conflict: "spec.resourceRestoreOrder[0]"},
		{name: "task in both lists", high: []string{task}, low: []string{pipeline, task, pipelineRun}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Merge(archive, tt.high, tt.low, rules)
			if tt.conflict != "" {
				if err == nil || !strings.Contains(err.Error(), tt.conflict) {
					t.Fatalf("got %v, want conflict for %s", err, tt.conflict)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{task, pipeline, pipelineRun}
			if !slices.Equal(got.Order, want) {
				t.Errorf("got %v, want %v", got.Order, want)
			}
		})
	}
}

func TestMergeAgainstAllLegalOrders(t *testing.T) {
	archive := []string{CRDs, "high", "a", "b", "c", "low"}
	// The tier constraints permit exactly these orders, in baseline
	// lexicographic rank order. Find the first that satisfies the user rules.
	middles := [][]string{{"a", "b", "c"}, {"a", "c", "b"}, {"b", "a", "c"}, {"b", "c", "a"}, {"c", "a", "b"}, {"c", "b", "a"}}
	var candidates [][]string
	for _, middle := range middles {
		candidates = append(candidates, slices.Concat([]string{CRDs, "high"}, middle, []string{"low"}))
	}
	var edges []Rule
	for _, resource := range archive {
		for _, beforeResource := range archive {
			if resource != beforeResource {
				edges = append(edges, Rule{Before: beforeResource, After: resource})
			}
		}
	}
	for _, first := range edges {
		for _, second := range edges {
			var expected []string
			for _, order := range candidates {
				if slices.Index(order, first.Before) < slices.Index(order, first.After) &&
					slices.Index(order, second.Before) < slices.Index(order, second.After) {
					expected = order
					break
				}
			}
			for _, rules := range [][]Rule{{first, second}, {second, first}} {
				got, err := Merge(archive, []string{"high"}, []string{"low"}, rules)
				if (err != nil) != (expected == nil) || !slices.Equal(got.Order, expected) {
					t.Fatalf("rules %+v: got %v with error %v, want %v", rules, got.Order, err, expected)
				}
			}
		}
	}
}

func TestValidateEndpoints(t *testing.T) {
	for _, name := range []string{"", "pods.", ".example.org", "widgets.Bad", "widgets.a..b", "pods/status", "a=b", "*"} {
		if err := Validate([]Rule{{Before: "configmaps", After: name}}); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	if err := Validate([]Rule{{Before: "configmaps", After: "tasks.tekton.dev"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMergeEitherDirection(t *testing.T) {
	archive := []string{"parents.example.org", "children.example.org"}
	for _, order := range [][]string{archive, {archive[1], archive[0]}, archive} {
		got, err := Merge(archive, nil, nil, []Rule{{Before: order[0], After: order[1]}})
		if err != nil || !slices.Equal(got.Order, order) {
			t.Fatalf("restore %s before %s: got %v, %v", order[0], order[1], got.Order, err)
		}
	}
}
