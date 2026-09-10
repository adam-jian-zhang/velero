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

package test

import (
	"fmt"
	"os"
	"path/filepath"

	corev1api "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// LoadBaselineOwnerRefConfigMapFromExample loads and unmarshals the baseline ConfigMap
// directly from examples/velero-ownerref-config.yaml for testing purposes.
func LoadBaselineOwnerRefConfigMapFromExample(namespace string) (*corev1api.ConfigMap, error) {
	if namespace == "" {
		namespace = "velero"
	}

	startDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting current working directory: %w", err)
	}

	dir := startDir
	var target string
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "examples", "velero-ownerref-config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			target = candidate
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if target == "" {
		return nil, fmt.Errorf("examples/velero-ownerref-config.yaml not found searching upward from %s", startDir)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", target, err)
	}

	cm := &corev1api.ConfigMap{}
	if err := yaml.Unmarshal(data, cm); err != nil {
		return nil, fmt.Errorf("unmarshaling %s: %w", target, err)
	}
	cm.Namespace = namespace
	return cm, nil
}
