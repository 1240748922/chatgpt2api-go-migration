package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open never imports a local account file into PostgreSQL. A new database starts empty.
func Open(accountsPath, authKeysPath, configPath, dsn string) (*Store, error) {
	s := New(accountsPath, authKeysPath, configPath)
	if strings.TrimSpace(dsn) == "" {
		return s, nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("open account database failed")
	}
	connections := 8
	if value, e := strconv.Atoi(os.Getenv("GO_DATABASE_MAX_CONNECTIONS")); e == nil && value >= 1 && value <= 32 {
		connections = value
	}
	db.SetMaxOpenConns(connections)
	db.SetMaxIdleConns(min(4, connections))
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("account database unavailable")
	}
	// Names deliberately differ from the Python tables; no implicit data conversion.
	const schema = `
 CREATE TABLE IF NOT EXISTS go_account_revision (id smallint PRIMARY KEY CHECK(id=1), revision bigint NOT NULL);
 INSERT INTO go_account_revision VALUES (1,0) ON CONFLICT DO NOTHING;
 CREATE TABLE IF NOT EXISTS go_accounts (id text PRIMARY KEY, token_key text UNIQUE NOT NULL, rt_key text UNIQUE, data jsonb NOT NULL, revision bigint NOT NULL, deleted boolean NOT NULL DEFAULT false);
 CREATE INDEX IF NOT EXISTS go_accounts_revision_idx ON go_accounts(revision);
 CREATE TABLE IF NOT EXISTS go_documents (name text PRIMARY KEY, data jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now());
 CREATE TABLE IF NOT EXISTS go_leases (resource text PRIMARY KEY, owner text NOT NULL, expires_at timestamptz NOT NULL);
 CREATE INDEX IF NOT EXISTS go_leases_expiry_idx ON go_leases(expires_at);
 CREATE TABLE IF NOT EXISTS go_call_records (id text PRIMARY KEY, recorded_at timestamptz NOT NULL, data jsonb NOT NULL);
 CREATE INDEX IF NOT EXISTS go_call_records_time_idx ON go_call_records(recorded_at DESC);
 CREATE INDEX IF NOT EXISTS go_call_records_status_idx ON go_call_records((data->'detail'->>'status'),recorded_at DESC);
 CREATE INDEX IF NOT EXISTS go_call_records_model_idx ON go_call_records((data->'detail'->>'model'),recorded_at DESC);
 CREATE TABLE IF NOT EXISTS go_runtime_instances (id text PRIMARY KEY, data jsonb NOT NULL, expires_at timestamptz NOT NULL);
 CREATE TABLE IF NOT EXISTS go_runtime_cancels (call_id text PRIMARY KEY, instance_id text NOT NULL, expires_at timestamptz NOT NULL);
 CREATE TABLE IF NOT EXISTS go_image_consumptions (id text PRIMARY KEY, account_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
 `
	// Serialize first boot across replicas: IF NOT EXISTS alone does not protect
	// concurrent PostgreSQL catalog inserts for a previously empty schema.
	tx, err := db.BeginTx(ctx, nil)
	if err == nil {
		_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashtext('gptgrok2api-go-schema-v1'))`)
		if err == nil {
			_, err = tx.ExecContext(ctx, schema)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize migration schema: %w", err)
	}
	s.pg = db
	s.pgItems = map[string]map[string]any{}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.accountsTimer != nil {
		s.accountsTimer.Stop()
		s.accountsTimer = nil
	}
	s.mu.Unlock()
	if s.pg != nil {
		return s.pg.Close()
	}
	return s.FlushAccounts()
}
func (s *Store) Shared() bool { return s.pg != nil }
func dbContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (s *Store) pgSnapshot() ([]map[string]any, uint64, error) {
	started := time.Now()
	defer s.observe(started, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountsLoaded && time.Since(s.pgSnapshotAt) < 250*time.Millisecond {
		return s.accountsCache, s.accountsRevision, nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	// Read a consistent revision and its changes. Mutations bump the revision inside the same transaction.
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var revision uint64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM go_account_revision WHERE id=1`).Scan(&revision); err != nil {
		return nil, 0, err
	}
	if !s.accountsLoaded || revision != s.accountsRevision {
		rows, err := tx.QueryContext(ctx, `SELECT id,data,deleted FROM go_accounts WHERE revision>$1 ORDER BY revision,id`, s.accountsRevision)
		if err != nil {
			return nil, 0, err
		}
		changes := map[string]map[string]any{}
		for rows.Next() {
			var id string
			var raw []byte
			var deleted bool
			if err = rows.Scan(&id, &raw, &deleted); err != nil {
				rows.Close()
				return nil, 0, err
			}
			if deleted {
				changes[id] = nil
				continue
			}
			var item map[string]any
			if err = json.Unmarshal(raw, &item); err != nil {
				rows.Close()
				return nil, 0, err
			}
			changes[id] = item
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
		if err = tx.Commit(); err != nil {
			return nil, 0, err
		}
		for id, item := range changes {
			if item == nil {
				delete(s.pgItems, id)
			} else {
				s.pgItems[id] = item
			}
		}
		ids := make([]string, 0, len(s.pgItems))
		for id := range s.pgItems {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		items := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			items = append(items, s.pgItems[id])
		}
		s.accountsCache = items
		s.accountsIndex = accountIndex(items)
		s.accountsRevision = revision
		s.accountsLoaded = true
	} else {
		if err = tx.Commit(); err != nil {
			return nil, 0, err
		}
	}
	s.pgSnapshotAt = time.Now()
	return s.accountsCache, s.accountsRevision, nil
}

func nextRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `UPDATE go_account_revision SET revision=revision+1 WHERE id=1 RETURNING revision`).Scan(&revision)
	return revision, err
}
func (s *Store) invalidatePG() { s.mu.Lock(); s.pgSnapshotAt = time.Time{}; s.mu.Unlock() }

func (s *Store) pgAdd(tokens []string, payloads []map[string]any) (int, int, []map[string]any, error) {
	return s.pgAddWithCheckpoint(tokens, payloads, nil, "")
}

// Import membership and its progress receipt commit together. Replaying a chunk
// after a process crash returns the original counts instead of counting skips.
func (l *Lease) ImportChunk(name string, rows []map[string]any) (int, int, error) {
	if l.store.pg != nil {
		added, skipped, _, err := l.store.pgAddWithCheckpoint(nil, rows, l, name)
		return added, skipped, err
	}
	l.store.leaseMu.Lock()
	active := l.store.localLeases[l.Resource]
	l.store.leaseMu.Unlock()
	if active.owner != l.Owner || !active.until.After(time.Now()) {
		return 0, 0, errors.New("task lease lost")
	}
	doc, err := l.store.Document(name)
	if err != nil {
		return 0, 0, err
	}
	if boolValue(doc["imported"], false) {
		return quotaNumber(doc["added"]), quotaNumber(doc["skipped"]), nil
	}
	added, skipped, _, err := l.store.AddAccounts(nil, rows)
	if err != nil {
		return 0, 0, err
	}
	err = l.store.FlushAccounts()
	if err != nil {
		return 0, 0, err
	}
	_, err = l.MutateDocument(name, func(d map[string]any) error {
		d["imported"] = true
		d["added"] = added
		d["skipped"] = skipped
		d["total"] = len(rows)
		return nil
	})
	return added, skipped, err
}

func (s *Store) pgAddWithCheckpoint(tokens []string, payloads []map[string]any, lease *Lease, checkpoint string) (int, int, []map[string]any, error) {
	started := time.Now()
	defer s.observe(started, true)
	all := make([]map[string]any, 0, len(tokens)+len(payloads))
	for _, p := range payloads {
		p = cloneMap(p)
		normalizeAccount(p)
		if accountToken(p) != "" {
			all = append(all, p)
		}
	}
	for _, token := range tokens {
		if strings.TrimSpace(token) != "" {
			p := map[string]any{"access_token": strings.TrimSpace(token)}
			normalizeAccount(p)
			all = append(all, p)
		}
	}
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	defer tx.Rollback()
	var chunk map[string]any
	if lease != nil {
		var valid bool
		if err = tx.QueryRowContext(ctx, `SELECT owner=$2 AND expires_at>now() FROM go_leases WHERE resource=$1 FOR UPDATE`, lease.Resource, lease.Owner).Scan(&valid); err != nil {
			return 0, 0, nil, err
		}
		if !valid {
			return 0, 0, nil, errors.New("task lease lost")
		}
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM go_documents WHERE name=$1 FOR UPDATE`, checkpoint).Scan(&raw); err != nil {
			return 0, 0, nil, err
		}
		if err = json.Unmarshal(raw, &chunk); err != nil {
			return 0, 0, nil, err
		}
		if boolValue(chunk["imported"], false) {
			return quotaNumber(chunk["added"]), quotaNumber(chunk["skipped"]), nil, tx.Commit()
		}
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return 0, 0, nil, err
	}
	added, skipped := 0, 0
	for _, item := range all {
		if stringValue(item["id"]) == "" {
			item["id"] = randomID()
		}
		var rt any
		if v := stringValue(item["refresh_token"]); v != "" {
			rt = hashKey(v)
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return 0, 0, nil, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO go_accounts(id,token_key,rt_key,data,revision) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, stringValue(item["id"]), hashKey(accountToken(item)), rt, raw, revision)
		if err != nil {
			return 0, 0, nil, err
		}
		n, _ := result.RowsAffected()
		if n > 0 {
			added++
		} else {
			skipped++
		}
	}
	if chunk != nil {
		chunk["imported"] = true
		chunk["added"] = added
		chunk["skipped"] = skipped
		chunk["total"] = len(all)
		raw, e := json.Marshal(chunk)
		if e != nil {
			return 0, 0, nil, e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE go_documents SET data=$1,updated_at=now() WHERE name=$2`, raw, checkpoint); err != nil {
			return 0, 0, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, nil, err
	}
	s.invalidatePG()
	return added, skipped, nil, nil
}

func (s *Store) pgUpdate(token string, updates map[string]any) (map[string]any, error) {
	ctx, cancel := dbContext()
	defer cancel()
	return s.pgUpdateFenced(ctx, token, updates, nil, nil)
}

func (s *Store) pgUpdateFenced(ctx context.Context, token string, updates map[string]any, lease *Lease, expected map[string]any) (map[string]any, error) {
	started := time.Now()
	defer s.observe(started, true)
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if lease != nil {
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT owner=$2 AND expires_at>clock_timestamp() FROM go_leases WHERE resource=$1 FOR UPDATE`, lease.Resource, lease.Owner).Scan(&valid)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !valid {
			return nil, ErrLeaseLost
		}
		if err != nil {
			return nil, err
		}
	}
	// All account transactions lock revision before a row, avoiding lock-order inversions.
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return nil, err
	}
	var id string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT id,data FROM go_accounts WHERE token_key=$1 AND NOT deleted FOR UPDATE`, hashKey(strings.TrimSpace(token))).Scan(&id, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	var item map[string]any
	if err = json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	if expected != nil && !sameCredentials(item, expected) {
		return nil, ErrCredentialsChanged
	}
	for k, v := range updates {
		item[k] = v
	}
	item["id"] = id
	normalizeAccount(item)
	raw, err = json.Marshal(item)
	if err != nil {
		return nil, err
	}
	var rt any
	if v := stringValue(item["refresh_token"]); v != "" {
		rt = hashKey(v)
	}
	_, err = tx.ExecContext(ctx, `UPDATE go_accounts SET token_key=$1,rt_key=$2,data=$3,revision=$4 WHERE id=$5`, hashKey(accountToken(item)), rt, raw, revision, id)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidatePG()
	return item, nil
}
func (s *Store) pgDelete(tokens []string) (int, []map[string]any, error) {
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	removed := 0
	for _, token := range tokens {
		res, err := tx.ExecContext(ctx, `UPDATE go_accounts SET deleted=true,data='{}',token_key=id,rt_key=NULL,revision=$1 WHERE token_key=$2 AND NOT deleted`, revision, hashKey(strings.TrimSpace(token)))
		if err != nil {
			return 0, nil, err
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	s.invalidatePG()
	items, _, err := s.AccountSnapshot()
	return removed, cloneAccountList(items), err
}

// FindAccount looks up one account without cloning the complete pool.
func (s *Store) FindAccount(ref string) (map[string]any, error) {
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		var raw []byte
		err := s.pg.QueryRowContext(ctx, `SELECT data FROM go_accounts WHERE NOT deleted AND (token_key=$1 OR id=$2 OR data->>'email'=$2) LIMIT 1`, hashKey(ref), ref).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		var item map[string]any
		err = json.Unmarshal(raw, &item)
		return item, err
	}
	items, _, err := s.AccountSnapshot()
	if err != nil {
		return nil, err
	}
	for _, a := range items {
		if accountToken(a) == ref || stringValue(a["id"]) == ref || stringValue(a["email"]) == ref {
			return cloneMap(a), nil
		}
	}
	return nil, os.ErrNotExist
}

// Documents are durable server-side job/config records. Mutations take a row lock.
func (s *Store) Document(name string) (map[string]any, error) {
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		var raw []byte
		err := s.pg.QueryRowContext(ctx, `SELECT data FROM go_documents WHERE name=$1`, name).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]any{}, nil
		}
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		err = json.Unmarshal(raw, &doc)
		return doc, err
	}
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return loadMap(s.documentPath(name))
}
func (s *Store) MutateDocument(name string, fn func(map[string]any) error) (map[string]any, error) {
	return s.mutateDocument(name, nil, fn)
}
func (l *Lease) MutateDocument(name string, fn func(map[string]any) error) (map[string]any, error) {
	return l.store.mutateDocument(name, l, fn)
}
func (s *Store) mutateDocument(name string, lease *Lease, fn func(map[string]any) error) (map[string]any, error) {
	if s.pg == nil {
		if lease != nil {
			s.leaseMu.Lock()
			defer s.leaseMu.Unlock()
			active := s.localLeases[lease.Resource]
			if active.owner != lease.Owner || !active.until.After(time.Now()) {
				return nil, errors.New("task lease lost")
			}
		}
		s.documentMu.Lock()
		defer s.documentMu.Unlock()
		doc, err := loadMap(s.documentPath(name))
		if err != nil {
			return nil, err
		}
		if err = fn(doc); err != nil {
			return nil, err
		}
		return doc, writeJSON(s.documentPath(name), doc)
	}
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if lease != nil {
		var ok bool
		if err = tx.QueryRowContext(ctx, `SELECT owner=$2 AND expires_at>now() FROM go_leases WHERE resource=$1 FOR UPDATE`, lease.Resource, lease.Owner).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("task lease lost")
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO go_documents(name,data) VALUES($1,'{}') ON CONFLICT DO NOTHING`, name); err != nil {
		return nil, err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM go_documents WHERE name=$1 FOR UPDATE`, name).Scan(&raw); err != nil {
		return nil, err
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if err = fn(doc); err != nil {
		return nil, err
	}
	raw, err = json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE go_documents SET data=$1,updated_at=now() WHERE name=$2`, raw, name); err != nil {
		return nil, err
	}
	return doc, tx.Commit()
}
func (s *Store) DocumentNames(prefix string) ([]string, error) {
	if s.pg == nil {
		entries, err := os.ReadDir(s.documentPath(""))
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		if err != nil {
			return nil, err
		}
		names := []string{}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".json") {
				names = append(names, strings.TrimSuffix(e.Name(), ".json"))
			}
		}
		return names, nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	rows, err := s.pg.QueryContext(ctx, `SELECT name FROM go_documents WHERE left(name,length($1))=$1 ORDER BY name DESC`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// Ready verifies the shared account schema is reachable without returning connection details.
func (s *Store) Ready(ctx context.Context) error {
	if s.pg == nil {
		_, _, err := s.AccountSnapshot()
		return err
	}
	var revision uint64
	return s.pg.QueryRowContext(ctx, "SELECT revision FROM go_account_revision WHERE id=1").Scan(&revision)
}
