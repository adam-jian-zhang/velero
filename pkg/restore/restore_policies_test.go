package restore

import (
	"context"
	"io"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/internal/resourcepolicies"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	"github.com/vmware-tanzu/velero/pkg/test"
)

func TestRestoreResourcePoliciesFiltering(t *testing.T) {
	tests := []struct {
		name         string
		restore      *velerov1api.Restore
		backup       *velerov1api.Backup
		policyYAML   string
		apiResources []*test.APIResource
		tarball      io.Reader
		want         map[*test.APIResource][]string
	}{
		{
			name:    "namespaced filter policy with exact namespace match",
			restore: defaultRestore().Result(),
			backup:  defaultBackup().Result(),
			policyYAML: `version: v1
namespacedFilterPolicies:
  - namespaces:
      - ns-1
    resourceFilters:
      - kinds:
          - pods
        names:
          - pod-1
`,
			tarball: test.NewTarWriter(t).
				AddItems("pods",
					builder.ForPod("ns-1", "pod-1").Result(),
					builder.ForPod("ns-1", "pod-2").Result(),
					builder.ForPod("ns-2", "pod-1").Result(),
				).
				Done(),
			apiResources: []*test.APIResource{
				test.Pods(),
			},
			want: map[*test.APIResource][]string{
				test.Pods(): {"ns-1/pod-1", "ns-2/pod-1"}, // ns-2 is not filtered, ns-1 only includes pod-1
			},
		},
		{
			name:    "namespaced filter policy with glob namespace match and first-match semantics",
			restore: defaultRestore().Result(),
			backup:  defaultBackup().Result(),
			policyYAML: `version: v1
namespacedFilterPolicies:
  - namespaces:
      - ns-*
    resourceFilters:
      - kinds:
          - pods
        names:
          - pod-1
  - namespaces:
      - ns-1
    resourceFilters:
      - kinds:
          - pods
        names:
          - pod-2
`,
			tarball: test.NewTarWriter(t).
				AddItems("pods",
					builder.ForPod("ns-1", "pod-1").Result(),
					builder.ForPod("ns-1", "pod-2").Result(),
					builder.ForPod("ns-2", "pod-1").Result(),
					builder.ForPod("ns-2", "pod-2").Result(),
				).
				Done(),
			apiResources: []*test.APIResource{
				test.Pods(),
			},
			want: map[*test.APIResource][]string{
				test.Pods(): {"ns-1/pod-1", "ns-2/pod-1"},
			},
		},
		{
			name:    "cluster scoped filter policy",
			restore: defaultRestore().Result(),
			backup:  defaultBackup().Result(),
			policyYAML: `version: v1
clusterScopedFilterPolicy:
  resourceFilters:
    - kinds:
        - persistentvolumes
      names:
        - pv-1
`,
			tarball: test.NewTarWriter(t).
				AddItems("persistentvolumes",
					builder.ForPersistentVolume("pv-1").Result(),
					builder.ForPersistentVolume("pv-2").Result(),
				).
				Done(),
			apiResources: []*test.APIResource{
				test.PVs(),
			},
			want: map[*test.APIResource][]string{
				test.PVs(): {"/pv-1"},
			},
		},
		{
			name:    "catch-all filter",
			restore: defaultRestore().Result(),
			backup:  defaultBackup().Result(),
			policyYAML: `version: v1
namespacedFilterPolicies:
  - namespaces:
      - ns-1
    resourceFilters:
      - kinds:
          - '*'
        labelSelector:
          app: test
`,
			tarball: test.NewTarWriter(t).
				AddItems("pods",
					builder.ForPod("ns-1", "pod-1").ObjectMeta(builder.WithLabels("app", "test")).Result(),
					builder.ForPod("ns-1", "pod-2").Result(),
				).
				AddItems("deployments.apps",
					builder.ForDeployment("ns-1", "deploy-1").ObjectMeta(builder.WithLabels("app", "test")).Result(),
					builder.ForDeployment("ns-1", "deploy-2").Result(),
				).
				Done(),
			apiResources: []*test.APIResource{
				test.Pods(),
				test.Deployments(),
			},
			want: map[*test.APIResource][]string{
				test.Pods():        {"ns-1/pod-1"},
				test.Deployments(): {"ns-1/deploy-1"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			for _, r := range tc.apiResources {
				h.DiscoveryClient.WithAPIResource(r)
			}
			require.NoError(t, h.restorer.discoveryHelper.Refresh())

			var resPolicies *resourcepolicies.Policies
			if tc.policyYAML != "" {
				cm := &corev1api.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-policies",
						Namespace: "velero",
					},
					Data: map[string]string{
						"policy.yaml": tc.policyYAML,
					},
				}
				client := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm).Build()
				restore := tc.restore.DeepCopy()
				restore.Namespace = "velero"
				restore.Spec.ResourcePolicy = &corev1api.TypedLocalObjectReference{
					Kind: "configmap",
					Name: "test-policies",
				}
				var err error
				resPolicies, err = resourcepolicies.GetResourcePoliciesFromRestore(context.Background(), *restore, client, logrus.New())
				require.NoError(t, err)
			}

			data := &Request{
				Log:              h.log,
				Restore:          tc.restore,
				Backup:           tc.backup,
				PodVolumeBackups: nil,
				VolumeSnapshots:  nil,
				BackupReader:     tc.tarball,
				ResPolicies:      resPolicies,
			}
			warnings, errs := h.restorer.Restore(
				data,
				nil, // restoreItemActions
				nil, // volume snapshotter getter
			)

			assertEmptyResults(t, warnings, errs)
			assertAPIContents(t, h, tc.want)
		})
	}
}
