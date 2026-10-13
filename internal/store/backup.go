package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WritePostgresBackup streams a consistent snapshot. Runtime leases and cancel
// requests are deliberately not restored; they belong to the old processes.
// The restore requires an initialized, empty Go schema and refuses to overwrite
// live data. PostgreSQL's standard_conforming_strings makes JSON quoting exact.
func (s *Store) WritePostgresBackup(ctx context.Context, w io.Writer) error {
	return s.WritePostgresBackupIncluded(ctx, w, nil)
}
func BackupIncluded(include map[string]bool, key string) bool {
	if v, ok := include[key]; ok {
		return v
	}
	return key != "images"
}
func BackupDocumentIncluded(include map[string]bool, name string) bool {
	switch {
	case strings.HasPrefix(name, "import-") || strings.HasPrefix(name, "credential-") || strings.HasPrefix(name, "oauth-") || strings.HasPrefix(name, "account-refresh"):
		return BackupIncluded(include, "accounts_snapshot")
	case name == "config":
		return BackupIncluded(include, "config")
	case name == "auth-keys":
		return BackupIncluded(include, "auth_keys_snapshot")
	case strings.HasPrefix(name, "replenish"):
		return BackupIncluded(include, "register")
	case strings.HasPrefix(name, "cpa"):
		return BackupIncluded(include, "cpa")
	case strings.HasPrefix(name, "sub2api"):
		return BackupIncluded(include, "sub2api")
	case strings.HasPrefix(name, "imagetask") || strings.HasPrefix(name, "editable") || strings.HasPrefix(name, "imagepayload-") || strings.HasPrefix(name, "imageresult-") || strings.HasPrefix(name, "filepayload-") || strings.HasPrefix(name, "fileexport-"):
		return BackupIncluded(include, "image_tasks")
	case strings.HasPrefix(name, "dashboard"):
		return BackupIncluded(include, "dashboard_metrics")
	}
	return true
}
func (s *Store) WritePostgresBackupIncluded(ctx context.Context, w io.Writer, include map[string]bool) error {
	if s.pg == nil {
		return fmt.Errorf("PostgreSQL backup requires shared storage")
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = io.WriteString(w, `BEGIN;
SET LOCAL standard_conforming_strings = on;
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM go_accounts) OR EXISTS (SELECT 1 FROM go_documents) OR EXISTS (SELECT 1 FROM go_call_records) OR EXISTS (SELECT 1 FROM go_image_consumptions) THEN
  RAISE EXCEPTION 'Restore requires an empty Go database';
 END IF;
END $$;
DELETE FROM go_account_revision;
`); err != nil {
		return err
	}
	for _, table := range []string{"go_account_revision", "go_accounts", "go_documents", "go_call_records", "go_image_consumptions"} {
		if table == "go_call_records" && !BackupIncluded(include, "logs") || (table == "go_accounts" || table == "go_image_consumptions") && !BackupIncluded(include, "accounts_snapshot") {
			continue
		}
		rows, e := tx.QueryContext(ctx, "SELECT row_to_json(t)::text FROM "+table+" t")
		if e != nil {
			return e
		}
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return e
			}
			if table == "go_documents" {
				var doc struct {
					Name string `json:"name"`
				}
				if e = json.Unmarshal([]byte(raw), &doc); e != nil {
					rows.Close()
					return e
				}
				if !BackupDocumentIncluded(include, doc.Name) {
					continue
				}
			}
			_, e = fmt.Fprintf(w, "INSERT INTO %s SELECT * FROM jsonb_populate_record(NULL::%s, '%s'::jsonb);\n", table, table, strings.ReplaceAll(raw, "'", "''"))
			if e != nil {
				rows.Close()
				return e
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "COMMIT;\n")
	return err
}
