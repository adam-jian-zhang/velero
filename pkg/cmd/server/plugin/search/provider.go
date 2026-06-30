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
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

type Options struct {
	Driver       string
	DSN          string
	MaxWorkers   int
	QueryTimeout time.Duration
	Logger       logrus.FieldLogger
}

type BuiltInSearchProvider struct {
	store  Store
	opts   Options
	ready  atomic.Bool
	mu     sync.Mutex
	inited bool
}

func NewBuiltInSearchProvider(opts Options) *BuiltInSearchProvider {
	return &BuiltInSearchProvider{opts: opts}
}

func (p *BuiltInSearchProvider) Init(config map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	driver := config["driver"]
	if driver == "" {
		driver = p.opts.Driver
	}
	dsn := config["dsn"]
	if dsn == "" {
		dsn = p.opts.DSN
	}

	opts := p.opts
	if w, err := strconv.Atoi(config["maxWorkers"]); err == nil && w > 0 {
		opts.MaxWorkers = w
	}

	store, err := OpenStore(driver, dsn, opts)
	if err != nil {
		return err
	}
	if err := store.Migrate(context.Background()); err != nil {
		return err
	}
	p.store = store
	p.inited = true
	p.ready.Store(true) // Ready as soon as initialized for now, as gRPC doesn't expose MarkReady
	return nil
}

func (p *BuiltInSearchProvider) fetchTarball(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("failed to fetch tarball: status " + resp.Status)
	}
	return resp, nil
}

func (p *BuiltInSearchProvider) IndexBackup(ctx context.Context, name, url string) error {
	p.mu.Lock()
	inited := p.inited
	p.mu.Unlock()
	if !inited {
		return errors.New("not initialized")
	}

	resp, err := p.fetchTarball(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return p.store.ReplaceBackup(ctx, name, parseTarballStream(resp.Body, name))
}

func (p *BuiltInSearchProvider) DeleteBackup(ctx context.Context, name string) error {
	p.mu.Lock()
	inited := p.inited
	p.mu.Unlock()
	if !inited {
		return errors.New("not initialized")
	}
	return p.store.DeleteBackup(ctx, name)
}

func (p *BuiltInSearchProvider) Search(ctx context.Context, params velero.SearchParams) (velero.SearchResult, error) {
	p.mu.Lock()
	inited := p.inited
	p.mu.Unlock()
	if !inited {
		return velero.SearchResult{}, errors.New("not initialized")
	}

	ctx, cancel := context.WithTimeout(ctx, p.opts.QueryTimeout)
	defer cancel()

	return p.store.Search(ctx, params)
}

func (p *BuiltInSearchProvider) Ready(ctx context.Context) (bool, error) {
	return p.ready.Load(), nil
}
