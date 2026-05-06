package backup

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/resourcepolicies"
	"github.com/vmware-tanzu/velero/pkg/test"
)

func TestResolveNamespacedFilterPolicies(t *testing.T) {
	// Create a fake client with some namespaces
	ns1 := &corev1api.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns1",
			Labels: map[string]string{
				"env": "prod",
			},
		},
	}
	ns2 := &corev1api.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns2",
			Labels: map[string]string{
				"env": "staging",
			},
		},
	}
	ns3 := &corev1api.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns3",
			Labels: map[string]string{
				"env":  "prod",
				"skip": "true",
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithRuntimeObjects(ns1, ns2, ns3).Build()

	// Create a fake discovery helper
	discoveryHelper := test.NewFakeDiscoveryHelper(true, map[schema.GroupVersionResource]schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"}:       {Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "configmaps"}: {Group: "", Version: "v1", Resource: "configmaps"},
	})

	log := logrus.StandardLogger()

	policies := []resourcepolicies.NamespacedFilterPolicy{
		{
			Namespaces: []string{"ns1"},
			ResourceFilters: []resourcepolicies.ResourceFilter{
				{
					Kinds: []string{"pods"},
					LabelSelector: map[string]string{
						"app": "web",
					},
				},
			},
		},
		{
			NamespaceLabelSelector: map[string]string{
				"env": "prod",
			},
			ExcludedNamespaceLabelSelector: map[string]string{
				"skip": "true",
			},
			Action: "Skip",
		},
	}

	result, patternOrder, err := resolveNamespacedFilterPolicies(policies, discoveryHelper, fakeClient, log)
	require.NoError(t, err)

	// ns1 should match both policies, but the first one wins
	assert.Contains(t, patternOrder, "ns1")
	assert.NotNil(t, result["ns1"])
	assert.False(t, result["ns1"].SkipEntirely)
	assert.NotNil(t, result["ns1"].ResourceFilterMap["pods"])

	// ns3 should not match the second policy because of ExcludedNamespaceLabelSelector
	assert.NotContains(t, patternOrder, "ns3")
	assert.Nil(t, result["ns3"])

	// ns2 should not match any policy
	assert.NotContains(t, patternOrder, "ns2")
	assert.Nil(t, result["ns2"])
}
