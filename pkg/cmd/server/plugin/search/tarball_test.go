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

package search

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTarGz(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return &buf
}

func TestParseTarballStream(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		it := parseTarballStream(writeTarGz(t, map[string]string{}), "b")
		defer it.Close()
		_, ok := it.Next()
		assert.False(t, ok)
		assert.NoError(t, it.Err())
	})

	t.Run("mixed and malformed", func(t *testing.T) {
		meta, _ := json.Marshal(map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":      "nginx",
				"namespace": "prod",
				"labels":    map[string]string{"app": "nginx"},
			},
		})
		it := parseTarballStream(writeTarGz(t, map[string]string{
			"resources/apps/v1/namespaces/prod/nginx.json": string(meta),
			"resources/metadata/foo.json":                  `{"apiVersion":"v1","kind":"ConfigMap"}`,
			"velero-backup.json":                           `{}`,
			"resources/apps/v1/namespaces/prod/bad.json":   `{not-json`,
			"other/file.txt":                               "x",
		}), "daily")
		defer it.Close()

		var names []string
		for {
			rec, ok := it.Next()
			if !ok {
				break
			}
			names = append(names, rec.ResourceName)
			assert.Equal(t, "daily", rec.BackupName)
			assert.Equal(t, "Deployment", rec.Kind)
			assert.Equal(t, "prod", rec.Namespace)
			assert.Equal(t, "nginx", rec.Labels["app"])
		}
		assert.Equal(t, []string{"nginx"}, names)
		assert.NoError(t, it.Err())
	})
}

func TestIsResourceFile(t *testing.T) {
	assert.True(t, isResourceFile("resources/apps/v1/namespaces/ns/name.json"))
	assert.False(t, isResourceFile("resources/metadata/x.json"))
	assert.False(t, isResourceFile("velero-backup.json"))
	assert.False(t, isResourceFile("resources/foo.txt"))
}
