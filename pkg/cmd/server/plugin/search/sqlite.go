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
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register sqlite driver

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

type sqliteStore struct {
	db      *sql.DB
	writeCh chan writeOp
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type writeOp struct {
	ctx        context.Context
	kind       int // 0: index, 1: delete
	backupName string
	records    iterator[velero.ResourceRecord]
	errCh      chan error
}

func newSQLiteStore(dsn string, opts Options) (Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	// PRAGMAs for concurrent read/write
	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set pragmas: %w", err)
	}

	db.SetMaxOpenConns(opts.MaxWorkers)

	ctx, cancel := context.WithCancel(context.Background())
	s := &sqliteStore{
		db:      db,
		writeCh: make(chan writeOp, opts.MaxWorkers),
		opts:    opts,
		ctx:     ctx,
		cancel:  cancel,
	}

	s.wg.Add(1)
	go s.writerLoop()

	return s, nil
}

func (s *sqliteStore) Migrate(ctx context.Context) error {
	ddl := `
CREATE TABLE IF NOT EXISTS resources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    backup_name   TEXT    NOT NULL,
    resource_name TEXT    NOT NULL,
    api_version   TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    namespace     TEXT    NOT NULL DEFAULT '',
    labels        TEXT    NOT NULL DEFAULT '{}'
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_resources_unique
    ON resources (backup_name, resource_name, api_version, kind, namespace);
CREATE INDEX IF NOT EXISTS idx_resource_name ON resources (resource_name);
CREATE INDEX IF NOT EXISTS idx_kind_ns       ON resources (kind, namespace);

CREATE TABLE IF NOT EXISTS processed_backups (
    backup_name     TEXT PRIMARY KEY,
    indexed_at      TEXT NOT NULL,
    resource_count  INTEGER NOT NULL DEFAULT 0
);
`
	_, err := s.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("failed to execute migrations: %w", err)
	}
	return nil
}

func (s *sqliteStore) ReplaceBackup(ctx context.Context, backupName string, records iterator[velero.ResourceRecord]) error {
	errCh := make(chan error, 1)
	op := writeOp{
		ctx:        ctx,
		kind:       0,
		backupName: backupName,
		records:    records,
		errCh:      errCh,
	}
	select {
	case s.writeCh <- op:
	case <-s.ctx.Done():
		return fmt.Errorf("store is closed")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *sqliteStore) DeleteBackup(ctx context.Context, backupName string) error {
	errCh := make(chan error, 1)
	op := writeOp{
		ctx:        ctx,
		kind:       1,
		backupName: backupName,
		errCh:      errCh,
	}
	select {
	case s.writeCh <- op:
	case <-s.ctx.Done():
		return fmt.Errorf("store is closed")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *sqliteStore) writerLoop() {
	defer s.wg.Done()
	for {
		select {
		case op := <-s.writeCh:
			if op.kind == 0 {
				op.errCh <- s.doReplaceBackup(op.ctx, op.backupName, op.records)
			} else {
				op.errCh <- s.doDeleteBackup(op.ctx, op.backupName)
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *sqliteStore) doReplaceBackup(ctx context.Context, backupName string, records iterator[velero.ResourceRecord]) error {
	defer records.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM resources WHERE backup_name = ?", backupName); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO resources (backup_name, resource_name, api_version, kind, namespace, labels) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	for {
		rec, ok := records.Next()
		if !ok {
			break
		}
		labelsJSON, err := json.Marshal(rec.Labels)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, rec.BackupName, rec.ResourceName, rec.APIVersion, rec.Kind, rec.Namespace, string(labelsJSON)); err != nil {
			return err
		}
		count++
	}
	if err := records.Err(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO processed_backups (backup_name, indexed_at, resource_count) VALUES (?, ?, ?)", backupName, time.Now().Format(time.RFC3339), count); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *sqliteStore) doDeleteBackup(ctx context.Context, backupName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM resources WHERE backup_name = ?", backupName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM processed_backups WHERE backup_name = ?", backupName); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *sqliteStore) Search(ctx context.Context, params velero.SearchParams) (velero.SearchResult, error) {
	query := "SELECT backup_name, resource_name, api_version, kind, namespace, labels FROM resources WHERE 1=1"
	var args []any

	if params.Name != "" {
		if isGlob(params.Name) {
			query += " AND resource_name LIKE ?"
			args = append(args, globToLike(params.Name))
		} else {
			query += " AND resource_name = ?"
			args = append(args, params.Name)
		}
	}
	if params.Namespace != "" {
		query += " AND namespace = ?"
		args = append(args, params.Namespace)
	}
	if params.Kind != "" {
		query += " AND kind = ?"
		args = append(args, params.Kind)
	}
	if params.APIVersion != "" {
		query += " AND api_version = ?"
		args = append(args, params.APIVersion)
	}
	if params.BackupName != "" {
		query += " AND backup_name = ?"
		args = append(args, params.BackupName)
	}
	for k, v := range params.Labels {
		query += " AND json_extract(labels, ?) = ?"
		args = append(args, jsonPath(k), v)
	}

	countQuery := strings.Replace(query, "SELECT backup_name, resource_name, api_version, kind, namespace, labels", "SELECT count(*)", 1)
	var totalCount int
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return velero.SearchResult{}, err
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, params.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return velero.SearchResult{}, err
	}
	defer rows.Close()

	var records []velero.ResourceRecord
	for rows.Next() {
		var rec velero.ResourceRecord
		var labelsJSON string
		if err := rows.Scan(&rec.BackupName, &rec.ResourceName, &rec.APIVersion, &rec.Kind, &rec.Namespace, &labelsJSON); err != nil {
			return velero.SearchResult{}, err
		}
		if err := json.Unmarshal([]byte(labelsJSON), &rec.Labels); err != nil {
			return velero.SearchResult{}, fmt.Errorf("failed to unmarshal labels: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return velero.SearchResult{}, err
	}

	return velero.SearchResult{
		Records:    records,
		TotalCount: totalCount,
	}, nil
}

func (s *sqliteStore) ListBackups(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT backup_name FROM processed_backups")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backups []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		backups = append(backups, name)
	}
	return backups, rows.Err()
}

func (s *sqliteStore) Close() error {
	s.cancel()
	s.wg.Wait()
	return s.db.Close()
}

func jsonPath(k string) string {
	return `$."` + strings.ReplaceAll(k, `"`, `\"`) + `"`
}
