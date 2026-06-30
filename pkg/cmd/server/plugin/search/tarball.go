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
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

func parseTarballStream(r io.Reader, backupName string) iterator[velero.ResourceRecord] {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return errIter[velero.ResourceRecord](err)
	}
	tr := tar.NewReader(gzr)
	return &tarIterator{tr: tr, gz: gzr, backupName: backupName}
}

type tarIterator struct {
	tr         *tar.Reader
	gz         *gzip.Reader
	backupName string
	err        error
	done       bool
}

func (it *tarIterator) Close() error {
	if it.gz != nil {
		return it.gz.Close()
	}
	return nil
}

func (it *tarIterator) Next() (velero.ResourceRecord, bool) {
	if it.done {
		return velero.ResourceRecord{}, false
	}
	for {
		hdr, err := it.tr.Next()
		if err == io.EOF {
			it.done = true
			return velero.ResourceRecord{}, false
		}
		if err != nil {
			it.err = err
			it.done = true
			return velero.ResourceRecord{}, false
		}
		if !isResourceFile(hdr.Name) {
			continue
		}
		rec, err := parseResourceEntry(it.tr, it.backupName)
		if err != nil {
			continue // skip malformed entry
		}
		return rec, true
	}
}

func (it *tarIterator) Err() error {
	return it.err
}

func isResourceFile(name string) bool {
	if !strings.HasPrefix(name, "resources/") {
		return false
	}
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	if strings.HasPrefix(name, "resources/metadata/") {
		return false
	}
	return true
}

type k8sMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
}

func parseResourceEntry(r io.Reader, backupName string) (velero.ResourceRecord, error) {
	var meta k8sMeta
	if err := json.NewDecoder(r).Decode(&meta); err != nil {
		return velero.ResourceRecord{}, err
	}
	return velero.ResourceRecord{
		BackupName:   backupName,
		ResourceName: meta.Metadata.Name,
		APIVersion:   meta.APIVersion,
		Kind:         meta.Kind,
		Namespace:    meta.Metadata.Namespace,
		Labels:       meta.Metadata.Labels,
	}, nil
}
