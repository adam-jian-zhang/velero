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

package restore

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubetesting "k8s.io/client-go/testing"

	"github.com/velero-io/velero/internal/restoreorder"
	api "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/archive"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/test"
)

func TestArchivePriorityAliases(t *testing.T) {
	catalog := map[string]*archive.ResourceItems{}
	for _, key := range []string{"pods", "pods.example.org", "widgets.example.org", "widgets.example.net", "units.example.org"} {
		catalog[key] = &archive.ResourceItems{GroupResource: key}
	}
	p := PreparedBackup{resources: catalog}
	index, err := p.aliases(nil)
	require.NoError(t, err)
	helper := test.NewFakeDiscoveryHelper(true, nil)
	keys, err := index.resolve([]string{"pods", "units", "unmatched", "widgets.example.net"}, catalog, helper)
	require.NoError(t, err)
	require.Equal(t, []string{"pods", "units.example.org", "widgets.example.net"}, keys)
	_, err = index.resolve([]string{"widgets"}, catalog, helper)
	require.ErrorContains(t, err, "widgets.example.net, widgets.example.org")
}

func TestPreparedCRDPassProgress(t *testing.T) {
	h := newHarness(t)
	const widgets = "myclustercustomkinds.mygroup.io"
	definition := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1beta1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": widgets},
		"spec":     map[string]any{"group": "mygroup.io", "names": map[string]any{"plural": "myclustercustomkinds", "shortNames": []any{"mcc"}}},
	}}
	h.DynamicClient.PrependReactor("get", "customresourcedefinitions", func(kubetesting.Action) (bool, runtime.Object, error) {
		ready := definition.DeepCopy()
		ready.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}, map[string]any{"type": "NamesAccepted", "status": "True"}}}
		return true, ready, nil
	})
	cr := &test.APIResource{Group: "mygroup.io", Version: "v1", Name: "myclustercustomkinds", Kind: "MyClusterCustomKind"}
	h.DiscoveryClient.WithAPIResource(test.CRDs())
	h.DiscoveryClient.WithAPIResource(cr)
	require.NoError(t, h.discoveryHelper.Refresh())
	writer := test.NewTarWriter(t).AddItems(restoreorder.CRDs, definition)
	for _, name := range []string{"a", "b"} {
		writer.AddItems(widgets, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "mygroup.io/v1", "kind": cr.Kind, "metadata": map[string]any{"name": name}}})
	}
	restore := defaultRestore().Result()
	restore.Spec.ResourceRestoreOrder = []api.ResourceRestoreOrder{{Before: restoreorder.CRDs, After: widgets}}
	log := logrus.New()
	var lines bytes.Buffer
	log.SetOutput(&lines)
	req := &Request{Log: log, Restore: restore, Backup: defaultBackup().Result(), BackupReader: writer.Done(), DisableInformerCache: true}
	w, e := h.restorer.Restore(req, nil, nil)
	assertEmptyResults(t, w, e)
	for i, total := range []int{1, 3, 3} {
		require.Equal(t, 1, bytes.Count(lines.Bytes(), []byte(fmt.Sprintf("Restored %d items out of an estimated total of %d ", i+1, total))), "%s", lines.String())
	}
	require.Len(t, req.RestoredItems, 3)
	require.Equal(t, 3, bytes.Count(lines.Bytes(), []byte("items out of an estimated total")))
}

func TestLowPriorityAliasCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []api.ResourceRestoreOrder
		order []string
	}{
		{"legacy", nil, []string{"pods", "serviceaccounts"}},
		{"planned", []api.ResourceRestoreOrder{{Before: "serviceaccounts", After: "pods"}}, []string{"serviceaccounts", "pods"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.restorer.resourcePriorities.LowPriorities = []string{"po"}
			h.AddItems(t, test.Pods())
			h.AddItems(t, test.ServiceAccounts())
			recorder := &createRecorder{t: t}
			h.DynamicClient.PrependReactor("create", "*", recorder.reactor())
			r := defaultRestore().Result()
			r.Spec.ResourceRestoreOrder = tc.rules
			req := &Request{Log: h.log, Backup: defaultBackup().Result(), Restore: r,
				BackupReader: test.NewTarWriter(t).AddItems("pods", builder.ForPod("work", "pod").Result()).AddItems("serviceaccounts", builder.ForServiceAccount("work", "sa").Result()).Done()}
			w, e := h.restorer.Restore(req, nil, nil)
			assertEmptyResults(t, w, e)
			assertResourceCreationOrder(t, tc.order, recorder.resources)
		})
	}
}

func TestUnresolvedRestoreOrderReference(t *testing.T) {
	h := newHarness(t)
	r := defaultRestore().Result()
	r.Spec.ResourceRestoreOrderRef = &corev1api.TypedLocalObjectReference{Kind: "ConfigMap", Name: "order"}
	_, errs := h.restorer.Restore(&Request{Log: h.log, Restore: r}, nil, nil)
	require.Contains(t, errs.Velero[0], "controller-resolved")
}
