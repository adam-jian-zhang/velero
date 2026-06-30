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
	"fmt"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

type Store interface {
	Migrate(ctx context.Context) error
	ReplaceBackup(ctx context.Context, backupName string, records iterator[velero.ResourceRecord]) error
	DeleteBackup(ctx context.Context, backupName string) error
	Search(ctx context.Context, params velero.SearchParams) (velero.SearchResult, error)
	ListBackups(ctx context.Context) ([]string, error)
	Close() error
}

type iterator[T any] interface {
	Next() (T, bool)
	Err() error
	Close() error
}

type errIterator[T any] struct {
	err error
}

func (e *errIterator[T]) Next() (T, bool) {
	var zero T
	return zero, false
}

func (e *errIterator[T]) Err() error {
	return e.err
}

func (e *errIterator[T]) Close() error {
	return nil
}

func errIter[T any](err error) iterator[T] {
	return &errIterator[T]{err: err}
}

func OpenStore(driver, dsn string, opts Options) (Store, error) {
	switch driver {
	case "sqlite":
		return newSQLiteStore(dsn, opts)
	case "postgres":
		return newPostgresStore(dsn, opts)
	default:
		return nil, fmt.Errorf("unknown search driver %q", driver)
	}
}
