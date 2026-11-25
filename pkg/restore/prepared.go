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

package restore

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/velero-io/velero/internal/restoreorder"
	"github.com/velero-io/velero/pkg/archive"
	"github.com/velero-io/velero/pkg/discovery"
	"github.com/velero-io/velero/pkg/util/filesystem"
)

// PreparedBackup owns the extracted archive until execution completes.
type PreparedBackup struct {
	dir          string
	resources    map[string]*archive.ResourceItems
	emptyWarning error
	plan         restoreorder.Plan
	fs           filesystem.Interface
	log          logrus.FieldLogger
}

func (p *PreparedBackup) Close() {
	if err := p.fs.RemoveAll(p.dir); err != nil {
		p.log.WithError(err).Warnf("removing prepared backup %s", p.dir)
	}
}

func extractBackup(reader io.Reader, fs filesystem.Interface, log logrus.FieldLogger) (*PreparedBackup, error) {
	dir, err := archive.NewExtractor(log, fs).UnzipAndExtractBackup(reader)
	if err != nil {
		return nil, err
	}
	p := &PreparedBackup{dir: dir, fs: fs, log: log}
	p.resources, err = archive.NewParser(log, fs).Parse(dir)
	if errors.Is(err, archive.ErrNotExist) {
		p.emptyWarning = errors.Wrap(err, "zero items to be restored")
		return p, nil
	}
	if err != nil {
		p.Close()
		return nil, errors.Wrap(err, "error parsing backup contents")
	}
	return p, nil
}

// RestoreOrderError describes a rule or priority conflict suitable for status.validationErrors.
type RestoreOrderError struct{ error }

func (kr *kubernetesRestorer) Prepare(reader io.Reader, rules []restoreorder.Rule, log logrus.FieldLogger) (*PreparedBackup, error) {
	p, err := extractBackup(reader, kr.fileSystem, log)
	if err != nil {
		return nil, err
	}
	aliases, err := p.aliases(kr.discoveryHelper.Resources())
	if err != nil {
		p.Close()
		return nil, err
	}
	p.plan, err = kr.planResources(p.resources, aliases, rules)
	if err != nil {
		p.Close()
		return nil, &RestoreOrderError{err}
	}
	return p, nil
}

func (kr *kubernetesRestorer) planResources(resources map[string]*archive.ResourceItems, aliases aliasIndex, rules []restoreorder.Rule) (restoreorder.Plan, error) {
	high, err := aliases.resolve(kr.resourcePriorities.HighPriorities, resources, kr.discoveryHelper)
	if err != nil {
		return restoreorder.Plan{}, err
	}
	low, err := aliases.resolve(kr.resourcePriorities.LowPriorities, resources, kr.discoveryHelper)
	if err != nil {
		return restoreorder.Plan{}, err
	}
	return restoreorder.Merge(slices.Collect(maps.Keys(resources)), high, low, rules)
}

type aliasIndex map[string]map[string]bool

func (p *PreparedBackup) aliases(live []*metav1.APIResourceList) (aliasIndex, error) {
	index := aliasIndex{}
	add := func(key string, names ...string) {
		if p.resources[key] == nil {
			return
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			if index[name] == nil {
				index[name] = map[string]bool{}
			}
			index[name][key] = true
		}
	}
	for key := range p.resources {
		add(key, schema.ParseGroupResource(key).Resource)
	}
	for _, list := range live {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, err
		}
		for _, res := range list.APIResources {
			key := gv.WithResource(res.Name).GroupResource().String()
			add(key, res.SingularName)
			add(key, res.ShortNames...)
		}
	}
	if crds := p.resources[restoreorder.CRDs]; crds != nil {
		for _, name := range crds.ItemsByNamespace[""] {
			path, err := archive.GetItemFilePath(p.dir, restoreorder.CRDs, "", name)
			if err != nil {
				return nil, err
			}
			obj, err := archive.Unmarshal(p.fs, path)
			if err != nil {
				return nil, errors.Wrapf(err, "reading archived CRD %s", name)
			}
			var crd apiextv1.CustomResourceDefinition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &crd); err != nil {
				return nil, errors.Wrapf(err, "reading archived CRD %s", name)
			}
			names := crd.Spec.Names
			key := schema.GroupResource{Group: crd.Spec.Group, Resource: names.Plural}.String()
			add(key, names.Singular)
			add(key, names.ShortNames...)
		}
	}
	return index, nil
}

func (index aliasIndex) resolve(entries []string, catalog map[string]*archive.ResourceItems, helper discovery.Helper) ([]string, error) {
	var keys []string
	for _, entry := range entries {
		if catalog[entry] != nil {
			keys = append(keys, entry)
			continue
		}
		gr := schema.ParseGroupResource(entry)
		candidates := map[string]bool{}
		add := func(key schema.GroupResource) {
			if catalog[key.String()] != nil && (gr.Group == "" || gr.Group == key.Group) {
				candidates[key.String()] = true
			}
		}
		for key := range index[gr.Resource] {
			add(schema.ParseGroupResource(key))
		}
		mapped, _, err := helper.ResourceFor(gr.WithVersion(""))
		if err == nil {
			add(mapped.GroupResource())
		} else {
			var ambiguous *meta.AmbiguousResourceError
			if errors.As(err, &ambiguous) {
				for _, candidate := range ambiguous.MatchingResources {
					add(candidate.GroupResource())
				}
			}
		}
		matches := slices.Sorted(maps.Keys(candidates))
		switch len(matches) {
		case 0:
		case 1:
			keys = append(keys, matches[0])
		default:
			return nil, fmt.Errorf("server priority %q is ambiguous (%s): specify a canonical GroupResource", entry, strings.Join(matches, ", "))
		}
	}
	return keys, nil
}
