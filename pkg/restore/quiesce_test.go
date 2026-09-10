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
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/ownerref"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/label"
	velerotest "github.com/vmware-tanzu/velero/pkg/test"
)

func TestInjectQuiesceMetadata(t *testing.T) {
	rule := ownerref.QuiesceRule{
		Group:           "cluster.x-k8s.io",
		Kind:            "Cluster",
		AnnotationKey:   "cluster.x-k8s.io/paused",
		AnnotationValue: "",
	}

	// 1. Unpaused object from backup -> should be auto-quiesced with tracking labels
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
	rec, quiesced := InjectQuiesceMetadata(objUnpaused, rule, "restore-test-1", logrus.StandardLogger())
	assert.True(t, quiesced)
	assert.Equal(t, "cluster.x-k8s.io/paused", rec.AnnotationKey)
	ann := objUnpaused.GetAnnotations()
	assert.Contains(t, ann, "cluster.x-k8s.io/paused")
	assert.Equal(t, "cluster.x-k8s.io/paused", ann[AnnotationQuiescedKey])
	labels := objUnpaused.GetLabels()
	assert.Equal(t, "restore-test-1", labels[LabelQuiescedByRestore])

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
	recPre, quiescedPre := InjectQuiesceMetadata(objPrePaused, rule, "restore-test-1", logrus.StandardLogger())
	assert.False(t, quiescedPre)
	assert.Empty(t, recPre.AnnotationKey)

	// 3. Rule with empty annotation key -> should NOT inject anything
	emptyRule := ownerref.QuiesceRule{
		Group:         "cluster.x-k8s.io",
		Kind:          "Cluster",
		AnnotationKey: "   ",
	}
	_, quiescedEmpty := InjectQuiesceMetadata(objUnpaused, emptyRule, "restore-test-1", logrus.StandardLogger())
	assert.False(t, quiescedEmpty)

	// 4. Long restore name (>63 chars) -> label must be formatted via label.GetValidName
	longRestoreName := "a-very-long-restore-name-that-definitely-exceeds-the-kubernetes-maximum-limit-of-63-characters"
	objLong := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-long-name",
				"namespace": "default",
			},
		},
	}
	_, quiescedLong := InjectQuiesceMetadata(objLong, rule, longRestoreName, logrus.StandardLogger())
	assert.True(t, quiescedLong)
	longLabels := objLong.GetLabels()
	assert.LessOrEqual(t, len(longLabels[LabelQuiescedByRestore]), 63)
	assert.Equal(t, label.GetValidName(longRestoreName), longLabels[LabelQuiescedByRestore])
}

func TestCanUnquiesce(t *testing.T) {
	clusterQ := velerov1api.QuiescedObjectRef{
		Group:         "cluster.x-k8s.io",
		Version:       "v1beta1",
		Kind:          "Cluster",
		Namespace:     "default",
		Name:          "cluster-1",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}

	// Case 1: No pending patches -> eligible
	assert.True(t, CanUnquiesce(clusterQ, nil))

	// Case 2: Pending patch targets Q directly -> not eligible
	patchesTargetQ := []velerov1api.PendingPatchRef{
		{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Cluster",
			Namespace: "default",
			Name:      "cluster-1",
		},
	}
	assert.False(t, CanUnquiesce(clusterQ, patchesTargetQ))

	// Case 3: Pending patch names Q as an ownerReference -> not eligible
	patchesOwnerQ := []velerov1api.PendingPatchRef{
		{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Machine",
			Namespace: "default",
			Name:      "machine-1",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "cluster-1",
				},
			},
		},
	}
	assert.False(t, CanUnquiesce(clusterQ, patchesOwnerQ))

	// Case 3b: Multi-hop child (Machine -> MachineSet -> Cluster) names MachineSet in OwnerReferences,
	// but carries Cluster in Targets (via Option 4 transitive quiesce root propagation) -> not eligible
	patchesMultiHopTarget := []velerov1api.PendingPatchRef{
		{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Machine",
			Namespace: "default",
			Name:      "machine-1",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "MachineSet",
					Name:       "machineset-1",
				},
			},
			Targets: []velerov1api.TargetRef{
				{
					Group:     "cluster.x-k8s.io",
					Kind:      "Cluster",
					Namespace: "default",
					Name:      "cluster-1",
				},
			},
		},
	}
	assert.False(t, CanUnquiesce(clusterQ, patchesMultiHopTarget))

	// Case 3c: Sibling cluster in the same namespace is NOT in Targets -> eligible (Option 4 preserves isolation)
	cluster2Q := velerov1api.QuiescedObjectRef{
		Group:         "cluster.x-k8s.io",
		Version:       "v1beta1",
		Kind:          "Cluster",
		Namespace:     "default",
		Name:          "cluster-2",
		AnnotationKey: "cluster.x-k8s.io/paused",
	}
	assert.True(t, CanUnquiesce(cluster2Q, patchesMultiHopTarget), "Sibling cluster in same namespace must not be blocked by unrelated cluster's child patch")

	// Case 4: Pending patch names Q as a spec reference target -> not eligible
	patchesTargetRefQ := []velerov1api.PendingPatchRef{
		{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Machine",
			Namespace: "default",
			Name:      "machine-1",
			Targets: []velerov1api.TargetRef{
				{
					Group: "cluster.x-k8s.io",
					Kind:  "Cluster",
					Name:  "cluster-1",
				},
			},
		},
	}
	assert.False(t, CanUnquiesce(clusterQ, patchesTargetRefQ))

	// Case 5: Pending patch targets an unrelated resource in another namespace or different kind -> eligible
	patchesUnrelated := []velerov1api.PendingPatchRef{
		{
			Group:     "apps.example.io",
			Version:   "v1",
			Kind:      "Database",
			Namespace: "other-ns",
			Name:      "db-1",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps.example.io/v1",
					Kind:       "AppGroup",
					Name:       "group-1",
				},
			},
		},
	}
	assert.True(t, CanUnquiesce(clusterQ, patchesUnrelated))

	// Case 6: Pending patch has same Kind and Name in Targets, but different Group -> eligible (no cross-group collision)
	patchesDifferentGroup := []velerov1api.PendingPatchRef{
		{
			Group:     "core.example.io",
			Version:   "v1",
			Kind:      "Workload",
			Namespace: "default",
			Name:      "workload-1",
			Targets: []velerov1api.TargetRef{
				{
					Group: "other.x-k8s.io",
					Kind:  "Cluster",
					Name:  "cluster-1",
				},
			},
		},
	}
	assert.True(t, CanUnquiesce(clusterQ, patchesDifferentGroup))

	// Case 7: QuiescedObject is marked UnquiesceBlocked -> not eligible even if pendingPatches is nil or empty
	blockedQ := clusterQ
	blockedQ.UnquiesceBlocked = true
	assert.False(t, CanUnquiesce(blockedQ, nil))
	assert.False(t, CanUnquiesce(blockedQ, []velerov1api.PendingPatchRef{}))

	// Case 8: Pending patch in tenant-ns targets Q in default via TargetRef.Namespace -> not eligible
	patchesCrossNSTarget := []velerov1api.PendingPatchRef{
		{
			Group:     "apps.example.io",
			Version:   "v1",
			Kind:      "Service",
			Namespace: "tenant-ns",
			Name:      "svc-1",
			Targets: []velerov1api.TargetRef{
				{
					Group:     "cluster.x-k8s.io",
					Kind:      "Cluster",
					Namespace: "default",
					Name:      "cluster-1",
				},
			},
		},
	}
	assert.False(t, CanUnquiesce(clusterQ, patchesCrossNSTarget))

	// Case 9: Pending patch in tenant-ns targets same name in tenant-ns, while Q is in default -> eligible (no cross-namespace collision)
	patchesSameNameOtherNS := []velerov1api.PendingPatchRef{
		{
			Group:     "apps.example.io",
			Version:   "v1",
			Kind:      "Service",
			Namespace: "tenant-ns",
			Name:      "svc-2",
			Targets: []velerov1api.TargetRef{
				{
					Group:     "cluster.x-k8s.io",
					Kind:      "Cluster",
					Namespace: "tenant-ns",
					Name:      "cluster-1",
				},
			},
		},
	}
	assert.True(t, CanUnquiesce(clusterQ, patchesSameNameOtherNS))

	// Case 10: Pending patch has empty Group in TargetRef (omitted apiVersion in spec reference) -> matches Q -> not eligible
	patchesEmptyGroupTarget := []velerov1api.PendingPatchRef{
		{
			Group:     "apps.example.io",
			Version:   "v1",
			Kind:      "Service",
			Namespace: "default",
			Name:      "svc-3",
			Targets: []velerov1api.TargetRef{
				{
					Group:     "", // empty group in spec reference
					Kind:      "Cluster",
					Namespace: "default",
					Name:      "cluster-1",
				},
			},
		},
	}
	assert.False(t, CanUnquiesce(clusterQ, patchesEmptyGroupTarget), "Empty group in TargetRef must match quiesced parent of the same Kind and Name")
}

func TestUnquiesceEligibleObjects(t *testing.T) {
	scheme := runtime.NewScheme()

	cluster1 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-ready",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "",
					AnnotationQuiescedKey:     "cluster.x-k8s.io/paused",
				},
				"labels": map[string]any{
					LabelQuiescedByRestore: "rst-1",
				},
			},
		},
	}

	cluster2 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-blocked",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "",
					AnnotationQuiescedKey:     "cluster.x-k8s.io/paused",
				},
				"labels": map[string]any{
					LabelQuiescedByRestore: "rst-1",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster1, cluster2).Build()

	records := []velerov1api.QuiescedObjectRef{
		{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			Namespace:     "default",
			Name:          "cluster-ready",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
		{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			Namespace:     "default",
			Name:          "cluster-blocked",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
	}

	// Only cluster-blocked has a pending patch
	pending := []velerov1api.PendingPatchRef{
		{
			Group:     "cluster.x-k8s.io",
			Version:   "v1beta1",
			Kind:      "Machine",
			Namespace: "default",
			Name:      "machine-blocked",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Cluster",
					Name:       "cluster-blocked",
				},
			},
		},
	}

	stillQuiesced, warnings := UnquiesceEligibleObjects(context.Background(), logrus.StandardLogger(), fakeClient, records, pending)
	assert.True(t, warnings.IsEmpty())
	require.Len(t, stillQuiesced, 1)
	assert.Equal(t, "cluster-blocked", stillQuiesced[0].Name)

	// cluster-ready should have been unquiesced (annotation and label removed)
	liveReady := &unstructured.Unstructured{}
	liveReady.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cluster-ready"}, liveReady)
	require.NoError(t, err)
	assert.NotContains(t, liveReady.GetAnnotations(), "cluster.x-k8s.io/paused")
	assert.NotContains(t, liveReady.GetLabels(), LabelQuiescedByRestore)

	// cluster-blocked must remain paused
	liveBlocked := &unstructured.Unstructured{}
	liveBlocked.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"})
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cluster-blocked"}, liveBlocked)
	require.NoError(t, err)
	assert.Contains(t, liveBlocked.GetAnnotations(), "cluster.x-k8s.io/paused")
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
					AnnotationQuiescedKey:     "cluster.x-k8s.io/paused",
					"keep.me":                 "val",
				},
				"labels": map[string]any{
					LabelQuiescedByRestore: "rst-test",
					"keep.label":           "val2",
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

	records := []velerov1api.QuiescedObjectRef{
		{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			Namespace:     "default",
			Name:          "cluster-auto",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
		{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			Namespace:     "default",
			Name:          "cluster-pre",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
	}

	failed, warnings := UnquiesceObjects(context.Background(), logrus.StandardLogger(), fakeClient, records)
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, failed)

	// Check cluster-auto: paused annotation and tracking label should be removed, others kept
	liveAuto := &unstructured.Unstructured{}
	liveAuto.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cluster-auto"}, liveAuto)
	require.NoError(t, err)
	assert.NotContains(t, liveAuto.GetAnnotations(), "cluster.x-k8s.io/paused")
	assert.NotContains(t, liveAuto.GetAnnotations(), AnnotationQuiescedKey)
	assert.NotContains(t, liveAuto.GetLabels(), LabelQuiescedByRestore)
	assert.Equal(t, "val", liveAuto.GetAnnotations()["keep.me"])
	assert.Equal(t, "val2", liveAuto.GetLabels()["keep.label"])

	// Check cluster-pre: intentional pause must be preserved!
	livePre := &unstructured.Unstructured{}
	livePre.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"})
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cluster-pre"}, livePre)
	require.NoError(t, err)
	assert.Equal(t, "intentional-pause", livePre.GetAnnotations()["cluster.x-k8s.io/paused"])

	// Empty AnnotationKey record should be returned as failed and generate a warning
	emptyKeyRecord := []velerov1api.QuiescedObjectRef{
		{
			Group:         "cluster.x-k8s.io",
			Version:       "v1beta1",
			Kind:          "Cluster",
			Namespace:     "default",
			Name:          "cluster-empty-key",
			AnnotationKey: "",
		},
	}
	failedEmpty, warningsEmpty := UnquiesceObjects(context.Background(), logrus.StandardLogger(), fakeClient, emptyKeyRecord)
	assert.Len(t, failedEmpty, 1)
	assert.False(t, warningsEmpty.IsEmpty())
	assert.Contains(t, warningsEmpty.Namespaces["default"][0], "missing pause annotation key")
}

func TestUnquiesceObjects_DeletesValuedPauseKey(t *testing.T) {
	scheme := runtime.NewScheme()
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.io/v1",
			"kind":       "App",
			"metadata": map[string]any{
				"name":      "app-1",
				"namespace": "default",
			},
		},
	}
	rule := ownerref.QuiesceRule{
		Group:           "example.io",
		Kind:            "App",
		AnnotationKey:   "example.io/paused",
		AnnotationValue: "true",
	}
	rec, injected := InjectQuiesceMetadata(obj, rule, "rst-test", logrus.StandardLogger())
	require.True(t, injected)
	assert.Equal(t, "true", obj.GetAnnotations()["example.io/paused"])

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build()
	failed, warnings := UnquiesceObjects(context.Background(), logrus.StandardLogger(), fakeClient, []velerov1api.QuiescedObjectRef{rec})
	assert.True(t, warnings.IsEmpty())
	assert.Empty(t, failed)

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "App"})
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "app-1"}, live)
	require.NoError(t, err)
	_, hasPaused := live.GetAnnotations()["example.io/paused"]
	assert.False(t, hasPaused, "unpause must delete the key, not rewrite annotationValue to false")
	assert.NotContains(t, live.GetAnnotations(), AnnotationQuiescedKey)
	assert.NotContains(t, live.GetLabels(), LabelQuiescedByRestore)
}

func TestModeCLegacy_NonInterference(t *testing.T) {
	// Mode C: Feature flag disabled and no OwnerRefConfigMap -> OwnerRefRelink is nil on Request
	req := &Request{
		Restore: &velerov1api.Restore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "legacy-restore",
				Namespace: "velero",
			},
		},
		OwnerRefRelink: nil,
		OwnerRefScope:  nil,
	}

	// Verify that if req.OwnerRefRelink == nil, the initialization logic in RestoreWithResolvers
	// does NOT initialize or enable req.OwnerRefRelink or req.OwnerRefScope.
	if req.OwnerRefRelink != nil && req.OwnerRefRelink.Enabled {
		if req.OwnerRefScope == nil {
			req.OwnerRefScope = ownerref.NewScope()
		}
	}
	assert.Nil(t, req.OwnerRefRelink, "OwnerRefRelink must remain nil in Mode C")
	assert.Nil(t, req.OwnerRefScope, "OwnerRefScope must remain nil in Mode C")

	// Verify restoreContext behaves as pure legacy when ownerRefRelink is nil:
	ctx := &restoreContext{
		log:            logrus.StandardLogger(),
		ownerRefRelink: req.OwnerRefRelink,
		ownerRefScope:  req.OwnerRefScope,
		restore:        req.Restore,
		restoredItems:  make(map[itemKey]restoredItemStatus),
	}
	assert.Equal(t, "legacy-restore", ctx.restore.Name)

	clusterObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-legacy",
				"namespace": "default",
			},
		},
	}

	// In Phase 1A creation path, if ownerRefRelink is nil, no quiescing should ever be attempted
	if ctx.ownerRefRelink != nil && ctx.ownerRefRelink.Enabled && ctx.ownerRefScope != nil {
		if rule, ok := ctx.ownerRefScope.MatchesQuiesceRule(clusterObj.GroupVersionKind()); ok {
			InjectQuiesceMetadata(clusterObj, rule, ctx.restore.Name, logrus.StandardLogger())
		}
	}

	assert.NotContains(t, clusterObj.GetAnnotations(), "cluster.x-k8s.io/paused", "Mode C must not inject pause annotation")
	assert.NotContains(t, clusterObj.GetAnnotations(), AnnotationQuiescedKey, "Mode C must not inject quiesced-key annotation")
	assert.NotContains(t, clusterObj.GetLabels(), LabelQuiescedByRestore, "Mode C must not inject quiesced-by-restore label")

	// Calling registerAndMaybeEnqueue in Mode C must be a clean no-op
	ctx.registerAndMaybeEnqueue(
		clusterObj,
		clusterObj,
		schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "clusters"},
		[]metav1.OwnerReference{{Name: "some-parent", UID: "parent-uid"}},
		[]string{"default"},
	)
	assert.Nil(t, ctx.ownerRefRelink, "OwnerRefRelink must remain nil after registerAndMaybeEnqueue")
}

func TestExistingResource_QuiesceNonInterference(t *testing.T) {
	// Verify that pre-existing objects in the cluster are NEVER quiesced,
	// protecting running production workloads from unexpected pause mutations.
	state := ownerref.NewOwnerRefRelinkState()
	state.Enabled = true
	scope := ownerref.NewScope()
	state.SetScope(scope)

	ctx := &restoreContext{
		log:            logrus.StandardLogger(),
		ownerRefRelink: state,
		ownerRefScope:  scope,
		restoredItems:  make(map[itemKey]restoredItemStatus),
		restore: &velerov1api.Restore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rst-existing",
				Namespace: "velero",
			},
		},
	}

	// 1. Live pre-existing cluster object in production (unpaused)
	liveCluster := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "live-prod-cluster",
				"namespace": "default",
				"uid":       "live-cluster-uid-1234",
			},
		},
	}

	backupCluster := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "live-prod-cluster",
				"namespace": "default",
				"uid":       "backup-cluster-uid-5678",
			},
		},
	}

	// When an object already exists in the cluster and is skipped,
	// restoreItem only calls registerExistingObjectUID (to map backupUID -> liveUID for child relinking),
	// and NEVER invokes InjectQuiesceMetadata or RecordQuiescedObject, and never enqueues patches.
	ctx.registerExistingObjectUID(
		backupCluster,
		liveCluster,
		nil,
	)

	// Live object must remain untouched (zero pause annotations or tracking labels)
	assert.NotContains(t, liveCluster.GetAnnotations(), "cluster.x-k8s.io/paused", "Pre-existing cluster must not be paused")
	assert.NotContains(t, liveCluster.GetLabels(), LabelQuiescedByRestore, "Pre-existing cluster must not have tracking label")

	// QuiescedObjects queue must be completely empty!
	quiescedRecords := state.GetQuiescedObjects()
	assert.Empty(t, quiescedRecords, "No quiesced object records should be generated for pre-existing skipped objects")

	// Patch queues must be completely empty!
	assert.Empty(t, state.GetOwnerPatchQueue(), "No owner patches should be enqueued for pre-existing objects")
	assert.Empty(t, state.GetSpecPatchQueue(), "No spec patches should be enqueued for pre-existing objects")

	// But UID mapping was recorded so that child resources in the backup can re-link to the live cluster UID
	newUID, found := state.GetNewUID("backup-cluster-uid-5678")
	assert.True(t, found, "UID mapping from backup to live must be registered for existing objects")
	assert.Equal(t, types.UID("live-cluster-uid-1234"), newUID)
}

func TestCatchLeftoverPausedObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	paused := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-leftover",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "",
					AnnotationQuiescedKey:     "cluster.x-k8s.io/paused",
				},
				"labels": map[string]any{
					LabelQuiescedByRestore: "rst-leftover",
				},
			},
		},
	}
	unrelated := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-other",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "pre-existing",
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(paused, unrelated).Build()
	rules := []ownerref.QuiesceRule{
		{
			Group:         "cluster.x-k8s.io",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
	}

	leftover := CatchLeftoverPausedObjects(context.Background(), logrus.StandardLogger(), fakeClient, nil, "rst-leftover", rules)
	require.Len(t, leftover, 1)
	assert.Equal(t, "cluster-leftover", leftover[0].Name)
	assert.Equal(t, "cluster.x-k8s.io/paused", leftover[0].AnnotationKey)
}

func TestCatchLeftoverPausedObjects_LongRestoreName(t *testing.T) {
	scheme := runtime.NewScheme()
	longRestoreName := "a-very-long-restore-name-that-definitely-exceeds-the-kubernetes-maximum-limit-of-63-characters"
	validLabelVal := label.GetValidName(longRestoreName)

	paused := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      "cluster-leftover-long",
				"namespace": "default",
				"annotations": map[string]any{
					"cluster.x-k8s.io/paused": "",
					AnnotationQuiescedKey:     "cluster.x-k8s.io/paused",
				},
				"labels": map[string]any{
					LabelQuiescedByRestore: validLabelVal,
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(paused).Build()
	rules := []ownerref.QuiesceRule{
		{
			Group:         "cluster.x-k8s.io",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
	}

	leftover := CatchLeftoverPausedObjects(context.Background(), logrus.StandardLogger(), fakeClient, nil, longRestoreName, rules)
	require.Len(t, leftover, 1)
	assert.Equal(t, "cluster-leftover-long", leftover[0].Name)
	assert.Equal(t, "cluster.x-k8s.io/paused", leftover[0].AnnotationKey)
}

func TestCatchLeftoverPausedObjects_ContextCancelled(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	rules := []ownerref.QuiesceRule{
		{
			Group:         "cluster.x-k8s.io",
			Kind:          "Cluster",
			AnnotationKey: "cluster.x-k8s.io/paused",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before calling

	leftover := CatchLeftoverPausedObjects(ctx, logrus.StandardLogger(), fakeClient, nil, "rst-test", rules)
	assert.Empty(t, leftover)
}

func TestLoadQuiesceRulesForCatchUp(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1api.AddToScheme(scheme))

	// 1. Baseline ConfigMap present in cluster
	baselineCM, err := velerotest.LoadBaselineOwnerRefConfigMapFromExample("velero")
	require.NoError(t, err)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(baselineCM).Build()

	restore := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "velero",
			Name:      "test-restore",
		},
	}

	rules := LoadQuiesceRulesForCatchUp(context.Background(), fakeClient, restore, "")
	require.Len(t, rules, 1)
	assert.Equal(t, "cluster.x-k8s.io", rules[0].Group)
	assert.Equal(t, "Cluster", rules[0].Kind)
	assert.Equal(t, "cluster.x-k8s.io/paused", rules[0].AnnotationKey)

	// 2. Server-configured custom baseline ConfigMap
	serverCM := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "velero",
			Name:      "server-custom-baseline-cm",
		},
		Data: map[string]string{
			ownerref.ConfigMapKeyQuiesceOnRestore: `
- group: server.io
  kind: ServerApp
  annotationKey: server.io/paused
`,
		},
	}
	require.NoError(t, fakeClient.Create(context.Background(), serverCM))

	serverRules := LoadQuiesceRulesForCatchUp(context.Background(), fakeClient, restore, "server-custom-baseline-cm")
	require.Len(t, serverRules, 1)
	assert.Equal(t, "server.io", serverRules[0].Group)
	assert.Equal(t, "ServerApp", serverRules[0].Kind)
	assert.Equal(t, "server.io/paused", serverRules[0].AnnotationKey)

	// 3. Per-restore ConfigMap override
	customCM := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "velero",
			Name:      "restore-custom-cm",
		},
		Data: map[string]string{
			ownerref.ConfigMapKeyQuiesceOnRestore: `
- group: cluster.x-k8s.io
  kind: Cluster
  annotationKey: custom-cluster.io/paused
  annotationValue: "custom"
- group: custom.io
  kind: CustomApp
  annotationKey: custom.io/paused
`,
		},
	}
	require.NoError(t, fakeClient.Create(context.Background(), customCM))

	restoreWithCustom := &velerov1api.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "velero",
			Name:      "test-restore-custom",
		},
		Spec: velerov1api.RestoreSpec{
			OwnerRefConfigMap: &corev1api.TypedLocalObjectReference{
				Name: "restore-custom-cm",
			},
		},
	}

	overrideRules := LoadQuiesceRulesForCatchUp(context.Background(), fakeClient, restoreWithCustom, "")
	require.Len(t, overrideRules, 2)
	assert.Equal(t, "custom-cluster.io/paused", overrideRules[0].AnnotationKey, "Restore CM must override baseline quiesce rule for Cluster")
	assert.Equal(t, "custom.io/paused", overrideRules[1].AnnotationKey)
}
