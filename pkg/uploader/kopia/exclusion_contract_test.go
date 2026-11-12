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
package kopia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/ignorefs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/velero-io/velero/pkg/uploader"
	uploaderutil "github.com/velero-io/velero/pkg/uploader/util"
)

func TestKopiaExistingFiltersAndPolicyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		files                     map[string]string
		patterns, present, absent []string
	}{
		{name: "root ignore overrides policy", files: map[string]string{".kopiaignore": "!keep.tmp\nhide.db\n", "keep.tmp": "data", "hide.db": "data", "skip.tmp": "data"}, patterns: []string{"*.tmp"}, present: []string{"keep.tmp"}, absent: []string{"hide.db", "skip.tmp"}},
		{name: "nested ignore is scoped", files: map[string]string{".kopiaignore": "*.log\n", "a/.kopiaignore": "!keep.log\n", "a/keep.log": "data", "b/keep.log": "data"}, present: []string{"a/keep.log"}, absent: []string{"b/keep.log"}},
		{name: "pruned parent hides nested ignore", files: map[string]string{"cache/.kopiaignore": "!keep.tmp\n", "cache/keep.tmp": "data"}, patterns: []string{"cache/", "!cache/keep.tmp"}, absent: []string{"cache", "cache/keep.tmp"}},
		{name: "cache tag is independent of negation", files: map[string]string{"cache/CACHEDIR.TAG": "Signature: 8a477f597d28d172789f06886806bc55\n", "cache/keep.db": "data"}, patterns: []string{"!**"}, absent: []string{"cache/keep.db"}},
		{name: "Windows defaults", files: map[string]string{"System Volume Information/a": "data", "$Recycle.Bin/b": "data"}, patterns: []string{"/System Volume Information/", "/$Recycle.Bin/"}, absent: []string{"System Volume Information", "$Recycle.Bin"}},
		{name: "Windows defaults are overridable", files: map[string]string{"System Volume Information/a": "data", "$Recycle.Bin/b": "data"}, patterns: []string{"/System Volume Information/", "/$Recycle.Bin/", "!**"}, present: []string{"System Volume Information/a", "$Recycle.Bin/b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, data := range tc.files {
				path := filepath.Join(root, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
				require.NoError(t, os.WriteFile(path, []byte(data), 0600))
			}
			configured := getDefaultPolicy()
			configured.FilesPolicy.IgnoreRules = tc.patterns
			effective, _ := policy.MergePolicies([]*policy.Policy{configured}, snapshot.SourceInfo{})
			tree := policy.BuildTree(map[string]*policy.Policy{".": effective}, policy.DefaultPolicy)
			dir, err := localfs.Directory(root)
			require.NoError(t, err)
			filtered := ignorefs.New(dir, tree)
			defer filtered.Close()
			paths := map[string]bool{}
			var walk func(context.Context, fs.Directory, string)
			walk = func(ctx context.Context, dir fs.Directory, prefix string) {
				entries, err := fs.GetAllEntries(ctx, dir)
				require.NoError(t, err)
				for _, entry := range entries {
					name := prefix + entry.Name()
					paths[name] = true
					if sub, ok := entry.(fs.Directory); ok {
						walk(ctx, sub, name+"/")
					}
				}
			}
			walk(t.Context(), filtered, "")
			for _, name := range tc.present {
				require.True(t, paths[name], "missing %s; paths=%v", name, paths)
			}
			for _, name := range tc.absent {
				require.False(t, paths[name], "unexpected %s; paths=%v", name, paths)
			}
		})
	}
}

func TestRawBlockBackupRejectsExcludeBeforeOpeningDevice(t *testing.T) {
	for _, value := range []string{`["*"]`, `["*.tmp"]`, `invalid-json`} {
		_, _, err := Backup(t.Context(), nil, nil, "/nonexistent-device", "", false, "", uploader.PersistentVolumeBlock, map[string]string{uploaderutil.Exclude: value}, nil, nil, logrus.New())
		require.Error(t, err)
		if value == "invalid-json" {
			require.Contains(t, err.Error(), "Exclude")
		} else {
			require.Contains(t, err.Error(), "raw-block")
		}
	}
}
