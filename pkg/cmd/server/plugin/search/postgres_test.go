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
	"context"
	"os"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

func TestPostgresStore_Integration(t *testing.T) {
	dsn := os.Getenv("VELERO_SEARCH_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set VELERO_SEARCH_POSTGRES_DSN to run postgres integration test")
	}

	store, err := newPostgresStore(dsn, Options{MaxWorkers: 2, Logger: logrus.New()})
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Migrate(context.Background()))

	recs := []velero.ResourceRecord{
		{BackupName: "pb1", ResourceName: "nginx", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "prod", Labels: map[string]string{"app": "nginx"}},
	}
	require.NoError(t, store.ReplaceBackup(context.Background(), "pb1", sliceIter(recs)))
	require.NoError(t, store.ReplaceBackup(context.Background(), "pb1", sliceIter(recs)))

	res, err := store.Search(context.Background(), velero.SearchParams{
		Labels: map[string]string{"app": "nginx"}, Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)

	backs, err := store.ListBackups(context.Background())
	require.NoError(t, err)
	assert.Contains(t, backs, "pb1")

	require.NoError(t, store.DeleteBackup(context.Background(), "pb1"))
}
