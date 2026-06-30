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

package clientmgmt

import (
	"context"

	"github.com/pkg/errors"

	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt/process"
	"github.com/vmware-tanzu/velero/pkg/plugin/framework/common"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

// restartableSearchProvider is a search provider for a given implementation (such as "velero.io/search-provider").
// It is associated with a restartableProcess, which may be shared and used to run multiple plugins. At the beginning
// of each method call, the restartableSearchProvider asks its restartableProcess to restart itself if needed (e.g. if the
// process terminated for any reason), then it proceeds with the actual call.
type restartableSearchProvider struct {
	key                 process.KindAndName
	sharedPluginProcess process.RestartableProcess
	config              map[string]string
}

// NewRestartableSearchProvider returns a new restartableSearchProvider.
func NewRestartableSearchProvider(name string, sharedPluginProcess process.RestartableProcess) velero.SearchProvider {
	r := &restartableSearchProvider{
		key:                 process.KindAndName{Kind: common.PluginKindSearchProvider, Name: name},
		sharedPluginProcess: sharedPluginProcess,
	}

	// Register our Reinitialize method with the restartableProcess so it can be invoked whenever the process is restarted.
	sharedPluginProcess.AddReinitializer(r.key, r)

	return r
}

// Reinitialize re-initializes the plugin by invoking its Init method with the saved config.
func (r *restartableSearchProvider) Reinitialize(dispensed any) error {
	delegate, ok := dispensed.(velero.SearchProvider)
	if !ok {
		return errors.Errorf("plugin %T is not a SearchProvider", dispensed)
	}

	if r.config == nil {
		return nil
	}

	return delegate.Init(r.config)
}

// getDelegate restarts the plugin process (if needed) and returns the search provider delegate.
func (r *restartableSearchProvider) getDelegate() (velero.SearchProvider, error) {
	if err := r.sharedPluginProcess.ResetIfNeeded(); err != nil {
		return nil, err
	}

	plugin, err := r.sharedPluginProcess.GetByKindAndName(r.key)
	if err != nil {
		return nil, err
	}

	delegate, ok := plugin.(velero.SearchProvider)
	if !ok {
		return nil, errors.Errorf("plugin %T is not a SearchProvider", plugin)
	}

	return delegate, nil
}

func (r *restartableSearchProvider) Init(config map[string]string) error {
	r.config = config
	delegate, err := r.getDelegate()
	if err != nil {
		return err
	}
	return delegate.Init(config)
}

func (r *restartableSearchProvider) IndexBackup(ctx context.Context, backupName, tarballURL string) error {
	delegate, err := r.getDelegate()
	if err != nil {
		return err
	}

	return delegate.IndexBackup(ctx, backupName, tarballURL)
}

func (r *restartableSearchProvider) DeleteBackup(ctx context.Context, backupName string) error {
	delegate, err := r.getDelegate()
	if err != nil {
		return err
	}

	return delegate.DeleteBackup(ctx, backupName)
}

func (r *restartableSearchProvider) Search(ctx context.Context, params velero.SearchParams) (velero.SearchResult, error) {
	delegate, err := r.getDelegate()
	if err != nil {
		return velero.SearchResult{}, err
	}

	return delegate.Search(ctx, params)
}

func (r *restartableSearchProvider) Ready(ctx context.Context) (bool, error) {
	delegate, err := r.getDelegate()
	if err != nil {
		return false, err
	}

	return delegate.Ready(ctx)
}
