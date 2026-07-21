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
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

func sliceIter(recs []velero.ResourceRecord) iterator[velero.ResourceRecord] {
	return &sliceIterator{recs: recs}
}

type sliceIterator struct {
	recs []velero.ResourceRecord
	i    int
}

func (s *sliceIterator) Next() (velero.ResourceRecord, bool) {
	if s.i >= len(s.recs) {
		return velero.ResourceRecord{}, false
	}
	r := s.recs[s.i]
	s.i++
	return r, true
}

func (s *sliceIterator) Err() error   { return nil }
func (s *sliceIterator) Close() error { return nil }

func TestSQLiteStore_ReplaceSearchDelete(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "search.db")
	store, err := newSQLiteStore(dsn, Options{MaxWorkers: 2, Logger: logrus.New()})
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Migrate(context.Background()))

	recs := []velero.ResourceRecord{
		{BackupName: "b1", ResourceName: "nginx", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "prod", Labels: map[string]string{"app": "nginx"}},
		{BackupName: "b1", ResourceName: "redis", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "prod", Labels: map[string]string{"app": "redis"}},
	}
	require.NoError(t, store.ReplaceBackup(context.Background(), "b1", sliceIter(recs)))

	// Idempotent re-index
	require.NoError(t, store.ReplaceBackup(context.Background(), "b1", sliceIter(recs)))

	backs, err := store.ListBackups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"b1"}, backs)

	res, err := store.Search(context.Background(), velero.SearchParams{Name: "nginx*", Kind: "Deployment", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)
	require.Len(t, res.Records, 1)
	assert.Equal(t, "nginx", res.Records[0].ResourceName)

	res, err = store.Search(context.Background(), velero.SearchParams{
		Labels: map[string]string{"app": "redis"}, Namespace: "prod", Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)

	res, err = store.Search(context.Background(), velero.SearchParams{BackupName: "b1", Limit: 1, Offset: 1})
	require.NoError(t, err)
	assert.Equal(t, 2, res.TotalCount)
	require.Len(t, res.Records, 1)

	// Literal % in name should not act as SQL wildcard when using exact match
	require.NoError(t, store.ReplaceBackup(context.Background(), "b2", sliceIter([]velero.ResourceRecord{
		{BackupName: "b2", ResourceName: "100%ready", APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns"},
	})))
	res, err = store.Search(context.Background(), velero.SearchParams{Name: "100%ready", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)

	require.NoError(t, store.DeleteBackup(context.Background(), "b1"))
	backs, err = store.ListBackups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"b2"}, backs)
}

func TestGlobToLikeEscapes(t *testing.T) {
	assert.Equal(t, `100\%ready%`, globToLike("100%ready*"))
	assert.Equal(t, `a\_b_`, globToLike("a_b?"))
	assert.True(t, isGlob("foo*"))
	assert.False(t, isGlob("foo"))
}

func TestBuiltInProvider_ReadyAndList(t *testing.T) {
	dir := t.TempDir()
	p := NewBuiltInSearchProvider(Options{
		Driver:       "sqlite",
		DSN:          filepath.Join(dir, "x.db"),
		MaxWorkers:   1,
		QueryTimeout: time.Second,
		Logger:       logrus.New(),
	})
	require.NoError(t, p.Init(map[string]string{}))

	ready, err := p.Ready(context.Background())
	require.NoError(t, err)
	assert.False(t, ready)

	require.NoError(t, p.MarkReady(context.Background()))
	ready, err = p.Ready(context.Background())
	require.NoError(t, err)
	assert.True(t, ready)

	names, err := p.ListIndexedBackups(context.Background())
	require.NoError(t, err)
	assert.Empty(t, names)
}
