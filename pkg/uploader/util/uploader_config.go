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

package util

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

const (
	ParallelFilesUpload = "ParallelFilesUpload"
	WriteSparseFiles    = "WriteSparseFiles"
	RestoreConcurrency  = "ParallelFilesDownload"

	// ExcludeFiles is the reserved uploader-config key carrying a JSON-encoded
	// []string of file/directory ignore patterns. It is written into PVB.Spec.UploaderSettings
	// and DU.Spec.DataMoverConfig.
	ExcludeFiles = "ExcludeFiles"

	// KopiaIgnoreDisabled is the reserved uploader-config key that, when set to "true",
	// disables in-volume .kopiaignore auto-discovery for the volume.
	KopiaIgnoreDisabled = "KopiaIgnoreDisabled"
)

func StoreBackupConfig(config *velerov1api.UploaderConfigForBackup) map[string]string {
	data := make(map[string]string)
	data[ParallelFilesUpload] = strconv.Itoa(config.ParallelFilesUpload)
	return data
}

func StoreRestoreConfig(config *velerov1api.UploaderConfigForRestore) map[string]string {
	data := make(map[string]string)
	if config.WriteSparseFiles != nil {
		data[WriteSparseFiles] = strconv.FormatBool(*config.WriteSparseFiles)
	} else {
		data[WriteSparseFiles] = strconv.FormatBool(false)
	}

	if config.ParallelFilesDownload > 0 {
		data[RestoreConcurrency] = strconv.Itoa(config.ParallelFilesDownload)
	}
	return data
}

func GetParallelFilesUpload(uploaderCfg map[string]string) (int, error) {
	parallelFilesUpload, ok := uploaderCfg[ParallelFilesUpload]
	if ok {
		parallelFilesUploadInt, err := strconv.Atoi(parallelFilesUpload)
		if err != nil {
			return 0, errors.Wrap(err, "failed to parse ParallelFilesUpload config")
		}
		return parallelFilesUploadInt, nil
	}
	return 0, nil
}

func GetWriteSparseFiles(uploaderCfg map[string]string) (bool, error) {
	writeSparseFiles, ok := uploaderCfg[WriteSparseFiles]
	if ok {
		writeSparseFilesBool, err := strconv.ParseBool(writeSparseFiles)
		if err != nil {
			return false, errors.Wrap(err, "failed to parse WriteSparseFiles config")
		}
		return writeSparseFilesBool, nil
	}
	return false, nil
}

func GetRestoreConcurrency(uploaderCfg map[string]string) (int, error) {
	restoreConcurrency, ok := uploaderCfg[RestoreConcurrency]
	if ok {
		restoreConcurrencyInt, err := strconv.Atoi(restoreConcurrency)
		if err != nil {
			return 0, errors.Wrap(err, "failed to parse RestoreConcurrency config")
		}
		return restoreConcurrencyInt, nil
	}
	return 0, nil
}

// StoreExcludeFiles JSON-encodes a slice of exclude pattern strings into a single config
// string suitable for the map[string]string carrier. Returns "" for an empty/nil slice.
func StoreExcludeFiles(rules []string) string {
	if len(rules) == 0 {
		return ""
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return ""
	}
	return string(b)
}

// GetExcludeFiles JSON-decodes the exclude pattern rules from the uploader configuration map.
// Returns (nil, nil) when the key is absent or empty. A malformed JSON value returns an error.
func GetExcludeFiles(uploaderCfg map[string]string) ([]string, error) {
	excludeStr, ok := uploaderCfg[ExcludeFiles]
	if !ok || strings.TrimSpace(excludeStr) == "" {
		return nil, nil
	}
	var rules []string
	if err := json.Unmarshal([]byte(excludeStr), &rules); err != nil {
		return nil, fmt.Errorf("invalid %q config (expected JSON-encoded string array): %w", ExcludeFiles, err)
	}
	return rules, nil
}

// IsKopiaIgnoreDisabled reports whether in-volume .kopiaignore discovery has been
// opted out for this volume via the KopiaIgnoreDisabled config key.
func IsKopiaIgnoreDisabled(uploaderCfg map[string]string) bool {
	v, ok := uploaderCfg[KopiaIgnoreDisabled]
	return ok && strings.EqualFold(strings.TrimSpace(v), "true")
}
