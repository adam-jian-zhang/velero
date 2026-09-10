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

package restore

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/ownerref"
)

func TestInjectQuiesceMetadata(t *testing.T) {
	rule := ownerref.QuiesceRule{
		Group:           "cluster.x-k8s.io",
		Kind:            "Cluster",
		AnnotationKey:   "cluster.x-k8s.io/paused",
		AnnotationValue: "",
	}

	// 1. Unpaused object from backup -> should be auto-quiesced
	objUnpaused := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-1",
				"namespace": "default",
			},
		},
	}
	rec, quiesced := InjectQuiesceMetadata(objUnpaused, rule, logrus.StandardLogger())
	assert.True(t, quiesced)
	assert.False(t, rec.OriginallyQuiesced)
	assert.Equal(t, "cluster.x-k8s.io/paused", rec.AnnotationKey)
	ann := objUnpaused.GetAnnotations()
	assert.Contains(t, ann, "cluster.x-k8s.io/paused")

	// 2. Pre-paused object from backup -> should preserve pause without unquiesce eligibility
	objPrePaused := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-2",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "true",
				},
			},
		},
	}
	recPre, quiescedPre := InjectQuiesceMetadata(objPrePaused, rule, logrus.StandardLogger())
	assert.False(t, quiescedPre)
	assert.True(t, recPre.OriginallyQuiesced)

	// 3. Rule with empty annotation key -> should NOT inject anything
	emptyRule := ownerref.QuiesceRule{
		Group:         "cluster.x-k8s.io",
		Kind:          "Cluster",
		AnnotationKey: "   ",
	}
	_, quiescedEmpty := InjectQuiesceMetadata(objUnpaused, emptyRule, logrus.StandardLogger())
	assert.False(t, quiescedEmpty)
}

func TestUnquiesceObjects(t *testing.T) {
	scheme := runtime.NewScheme()

	clusterAutoQuiesced := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-auto",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "",
					"keep.me":                 "val",
				},
			},
		},
	}

	clusterPrePaused := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-pre",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "intentional-pause",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterAutoQuiesced, clusterPrePaused).Build()

	records := []ownerref.QuiescedObjectRecord{
		{
			Group:              "cluster.x-k8s.io",
			Version:            "v1beta1",
			Kind:               "Cluster",
			Namespace:          "default",
			Name:               "cluster-auto",
			AnnotationKey:      "cluster.x-k8s.io/paused",
			OriginallyQuiesced: false,
		},
		{
			Group:              "cluster.x-k8s.io",
			Version:            "v1beta1",
			Kind:               "Cluster",
			Namespace:          "default",
			Name:               "cluster-pre",
			AnnotationKey:      "cluster.x-k8s.io/paused",
			OriginallyQuiesced: true,
		},
	}

	warnings := UnquiesceObjects(context.Background(), logrus.StandardLogger(), fakeClient, records)
	assert.True(t, warnings.IsEmpty())

	// Check cluster-auto: paused annotation should be removed, other annotations kept
	liveAuto := &unstructured.Unstructured{}
	liveAuto.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cluster-auto"}, liveAuto)
	require.NoError(t, err)
	assert.NotContains(t, liveAuto.GetAnnotations(), "cluster.x-k8s.io/paused")
	assert.Equal(t, "val", liveAuto.GetAnnotations()["keep.me"])

	// Check cluster-pre: intentional pause must be preserved!
	livePre := &unstructured.Unstructured{}
	livePre.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"})
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cluster-pre"}, livePre)
	require.NoError(t, err)
	assert.Equal(t, "intentional-pause", livePre.GetAnnotations()["cluster.x-k8s.io/paused"])
}
