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
}

func TestModeCLegacy_NonInterference(t *testing.T) {
	// Mode C: Feature flag disabled and no OwnerRefConfigMap -> OwnerRefRemap is nil on Request
	req := &Request{
		Restore: &velerov1api.Restore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "legacy-restore",
				Namespace: "velero",
			},
		},
		OwnerRefRemap: nil,
		OwnerRefScope: nil,
	}

	// Verify that if req.OwnerRefRemap == nil, the initialization logic in RestoreWithResolvers
	// does NOT initialize or enable req.OwnerRefRemap or req.OwnerRefScope.
	if req.OwnerRefRemap != nil && req.OwnerRefRemap.Enabled {
		if req.OwnerRefScope == nil {
			req.OwnerRefScope = ownerref.NewScope()
		}
	}
	assert.Nil(t, req.OwnerRefRemap, "OwnerRefRemap must remain nil in Mode C")
	assert.Nil(t, req.OwnerRefScope, "OwnerRefScope must remain nil in Mode C")

	// Verify restoreContext behaves as pure legacy when ownerRefRemap is nil:
	ctx := &restoreContext{
		log:           logrus.StandardLogger(),
		ownerRefRemap: req.OwnerRefRemap,
		ownerRefScope: req.OwnerRefScope,
		restore:       req.Restore,
		restoredItems: make(map[itemKey]restoredItemStatus),
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

	// In Phase 1A creation path, if ownerRefRemap is nil, no quiescing should ever be attempted
	if ctx.ownerRefRemap != nil && ctx.ownerRefRemap.Enabled && ctx.ownerRefScope != nil {
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
	assert.Nil(t, ctx.ownerRefRemap, "OwnerRefRemap must remain nil after registerAndMaybeEnqueue")
}

func TestExistingResource_QuiesceNonInterference(t *testing.T) {
	// Verify that pre-existing objects in the cluster are NEVER quiesced,
	// protecting running production workloads from unexpected pause mutations.
	state := ownerref.NewOwnerRefRemapState()
	state.Enabled = true
	scope := ownerref.NewScope()
	state.SetScope(scope)

	ctx := &restoreContext{
		log:           logrus.StandardLogger(),
		ownerRefRemap: state,
		ownerRefScope: scope,
		restoredItems: make(map[itemKey]restoredItemStatus),
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
	// restoreItem only calls registerAndMaybeEnqueue (to map backupUID -> liveUID for child relinking),
	// and NEVER invokes InjectQuiesceMetadata or RecordQuiescedObject.
	ctx.registerAndMaybeEnqueue(
		backupCluster,
		liveCluster,
		schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "clusters"},
		nil,
		nil,
	)

	// Live object must remain untouched (zero pause annotations or tracking labels)
	assert.NotContains(t, liveCluster.GetAnnotations(), "cluster.x-k8s.io/paused", "Pre-existing cluster must not be paused")
	assert.NotContains(t, liveCluster.GetLabels(), LabelQuiescedByRestore, "Pre-existing cluster must not have tracking label")

	// QuiescedObjects queue must be completely empty!
	quiescedRecords := state.GetQuiescedObjects()
	assert.Empty(t, quiescedRecords, "No quiesced object records should be generated for pre-existing skipped objects")

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
