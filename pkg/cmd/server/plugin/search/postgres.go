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
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

const postgresSchema = `
CREATE TABLE IF NOT EXISTS resources (
    id            SERIAL PRIMARY KEY,
    backup_name   TEXT    NOT NULL,
    resource_name TEXT    NOT NULL,
    api_version   TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    namespace     TEXT    NOT NULL DEFAULT '',
    labels        JSONB   NOT NULL DEFAULT '{}'::jsonb
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_resources_unique
    ON resources (backup_name, resource_name, api_version, kind, namespace);
CREATE INDEX IF NOT EXISTS idx_resource_name ON resources (resource_name);
CREATE INDEX IF NOT EXISTS idx_kind_ns       ON resources (kind, namespace);
CREATE INDEX IF NOT EXISTS idx_labels        ON resources USING GIN (labels);

CREATE TABLE IF NOT EXISTS processed_backups (
    backup_name     TEXT PRIMARY KEY,
    indexed_at      TEXT NOT NULL,
    resource_count  INTEGER NOT NULL DEFAULT 0
);
`

type postgresStore struct {
	db   *sql.DB
	opts Options
}

func newPostgresStore(dsn string, opts Options) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres db: %w", err)
	}
	workers := opts.MaxWorkers
	if workers <= 0 {
		workers = 10
	}
	db.SetMaxOpenConns(workers)
	db.SetMaxIdleConns(workers)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping postgres: %w", err)
	}

	return &postgresStore{db: db, opts: opts}, nil
}

func (s *postgresStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, postgresSchema); err != nil {
		return fmt.Errorf("failed to execute postgres migrations: %w", err)
	}
	return nil
}

func (s *postgresStore) ReplaceBackup(ctx context.Context, backupName string, records iterator[velero.ResourceRecord]) error {
	defer records.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM resources WHERE backup_name = $1", backupName); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO resources (backup_name, resource_name, api_version, kind, namespace, labels) VALUES ($1, $2, $3, $4, $5, $6::jsonb)")
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
		if labelsJSON == nil {
			labelsJSON = []byte("{}")
		}
		if _, err := stmt.ExecContext(ctx, rec.BackupName, rec.ResourceName, rec.APIVersion, rec.Kind, rec.Namespace, string(labelsJSON)); err != nil {
			return err
		}
		count++
	}
	if err := records.Err(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO processed_backups (backup_name, indexed_at, resource_count) VALUES ($1, $2, $3)
		 ON CONFLICT (backup_name) DO UPDATE SET indexed_at = EXCLUDED.indexed_at, resource_count = EXCLUDED.resource_count`,
		backupName, time.Now().UTC().Format(time.RFC3339), count); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *postgresStore) DeleteBackup(ctx context.Context, backupName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM resources WHERE backup_name = $1", backupName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM processed_backups WHERE backup_name = $1", backupName); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) Search(ctx context.Context, params velero.SearchParams) (velero.SearchResult, error) {
	query := "SELECT backup_name, resource_name, api_version, kind, namespace, labels FROM resources WHERE 1=1"
	var args []any

	if params.Name != "" {
		if isGlob(params.Name) {
			query += ` AND resource_name ILIKE ? ESCAPE '\'`
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
	if len(params.Labels) > 0 {
		labelsJSON, err := json.Marshal(params.Labels)
		if err != nil {
			return velero.SearchResult{}, err
		}
		query += " AND labels @> ?::jsonb"
		args = append(args, string(labelsJSON))
	}

	countQuery := strings.Replace(query, "SELECT backup_name, resource_name, api_version, kind, namespace, labels", "SELECT count(*)", 1)
	var totalCount int
	if err := s.db.QueryRowContext(ctx, convertPlaceholders(countQuery), args...).Scan(&totalCount); err != nil {
		return velero.SearchResult{}, err
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, params.Offset)

	rows, err := s.db.QueryContext(ctx, convertPlaceholders(query), args...)
	if err != nil {
		return velero.SearchResult{}, err
	}
	defer rows.Close()

	var records []velero.ResourceRecord
	for rows.Next() {
		var rec velero.ResourceRecord
		var labelsJSON []byte
		if err := rows.Scan(&rec.BackupName, &rec.ResourceName, &rec.APIVersion, &rec.Kind, &rec.Namespace, &labelsJSON); err != nil {
			return velero.SearchResult{}, err
		}
		if len(labelsJSON) > 0 {
			if err := json.Unmarshal(labelsJSON, &rec.Labels); err != nil {
				return velero.SearchResult{}, fmt.Errorf("failed to unmarshal labels: %w", err)
			}
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return velero.SearchResult{}, err
	}

	return velero.SearchResult{Records: records, TotalCount: totalCount}, nil
}

func (s *postgresStore) ListBackups(ctx context.Context) ([]string, error) {
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

func (s *postgresStore) Close() error {
	return s.db.Close()
}

func convertPlaceholders(query string) string {
	var b strings.Builder
	paramIdx := 1
	for _, r := range query {
		if r == '?' {
			b.WriteString(fmt.Sprintf("$%d", paramIdx))
			paramIdx++
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
