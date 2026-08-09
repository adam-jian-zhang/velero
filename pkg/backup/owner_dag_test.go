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

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/dag"
	"github.com/vmware-tanzu/velero/pkg/features"
)

func TestWriteAndLoadResourceDAG(t *testing.T) {
	features.NewFeatureFlagSet(velerov1api.OwnerReferenceDAGFeatureFlag)
	defer features.NewFeatureFlagSet()

	acc := dag.NewAccumulator()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	obj.SetKind("Machine")
	obj.SetNamespace("default")
	obj.SetName("m1")
	obj.SetUID("child")
	obj.SetOwnerReferences([]metav1.OwnerReference{{UID: "parent", Kind: "MachineSet", Name: "ms"}})
	acc.RecordItem(obj)

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := NewTarWriter(tar.NewWriter(gzw))
	kb := &kubernetesBackupper{}
	require.NoError(t, kb.writeResourceDAG(logrus.New(), tw, acc))
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	loaded, err := loadResourceDAGFromTarReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, dag.ResourceDAGVersion, loaded.Version)
	assert.Contains(t, loaded.Nodes, types.UID("child"))
	require.Len(t, loaded.Edges, 1)
}

func TestResourceDAGFileForArchive(t *testing.T) {
	acc := dag.NewAccumulator()
	file, err := resourceDAGFileForArchive(acc.Snapshot())
	require.NoError(t, err)
	assert.Equal(t, velerov1api.OwnerRefDAGFileName, file.FilePath)

	var parsed dag.ResourceDAG
	require.NoError(t, json.Unmarshal(file.FileBytes, &parsed))
	assert.Equal(t, dag.ResourceDAGVersion, parsed.Version)
}

func TestLoadResourceDAGMissing(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{Name: "metadata/version", Size: 4, Mode: 0644, Typeflag: tar.TypeReg}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("1.1\n"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	loaded, err := loadResourceDAGFromTarReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestFinalizeMergeViaAccumulator(t *testing.T) {
	// Simulate pass-1 DAG loaded then postOperation item recorded
	pass1 := &dag.ResourceDAG{
		Version: dag.ResourceDAGVersion,
		Nodes: map[types.UID]dag.ResourceNode{
			"a": {Name: "cluster", Kind: "Cluster", APIVersion: "cluster.x-k8s.io/v1beta1", Namespace: "default"},
		},
	}
	acc := dag.NewAccumulator()
	acc.Merge(pass1)

	postOp := &unstructured.Unstructured{}
	postOp.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	postOp.SetKind("Machine")
	postOp.SetName("m1")
	postOp.SetNamespace("default")
	postOp.SetUID("b")
	postOp.SetOwnerReferences([]metav1.OwnerReference{{UID: "a", Kind: "Cluster", Name: "cluster"}})
	acc.RecordItem(postOp)

	snapshot := acc.Snapshot()
	assert.Contains(t, snapshot.Nodes, types.UID("a"))
	assert.Contains(t, snapshot.Nodes, types.UID("b"))
	require.Len(t, snapshot.Edges, 1)
	assert.Equal(t, types.UID("a"), snapshot.Edges[0].ParentUID)
}

func TestNewOwnerDAGAccumulatorIfEnabled(t *testing.T) {
	features.NewFeatureFlagSet()
	assert.Nil(t, NewOwnerDAGAccumulatorIfEnabled())

	features.NewFeatureFlagSet(velerov1api.OwnerReferenceDAGFeatureFlag)
	defer features.NewFeatureFlagSet()
	assert.NotNil(t, NewOwnerDAGAccumulatorIfEnabled())
}
