package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// A conversation consumes quota when its result exists, including delivery failures.
// Its receipt and the account decrement commit together, so poll recovery is idempotent.
func (s *Store) ConsumeImageQuota(ref, receipt string) error {
	if strings.TrimSpace(receipt) == "" {
		return errors.New("image consumption receipt is required")
	}
	apply := func(item map[string]any) {
		if q, ok := item["quota"]; ok && q != nil && !boolValue(item["image_quota_unknown"], false) && quotaNumber(q) >= 0 {
			remaining := max(0, quotaNumber(q)-1)
			item["quota"] = remaining
			if remaining == 0 {
				item["image_quota_exhausted_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				item["next_maintenance_at"] = time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)
			}
		}
		item["image_generated_count"] = quotaNumber(item["image_generated_count"]) + 1
		item["last_image_generated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		tx, err := s.pg.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		revision, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		var id string
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT id,data FROM go_accounts WHERE NOT deleted AND (id=$1 OR token_key=$2) FOR UPDATE`, ref, hashKey(ref)).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO go_image_consumptions(id,account_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, receipt, id)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return nil
		}
		var item map[string]any
		if err = json.Unmarshal(raw, &item); err != nil {
			return err
		}
		apply(item)
		raw, err = json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE go_accounts SET data=$1,revision=$2 WHERE id=$3`, raw, revision, id); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		s.invalidatePG()
		return nil
	}
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return err
	}
	index := -1
	for i, a := range s.accountsCache {
		if accountToken(a) == ref || stringValue(a["id"]) == ref {
			index = i
			break
		}
	}
	if index < 0 {
		return os.ErrNotExist
	}
	item := cloneMap(s.accountsCache[index])
	receipts, _ := item["image_consumption_receipts"].([]any)
	for _, r := range receipts {
		if stringValue(r) == receipt {
			return nil
		}
	}
	// JSON mode is single-instance and stores receipts for all retained conversations.
	item["image_consumption_receipts"] = append(receipts, receipt)
	apply(item)
	next := append([]map[string]any(nil), s.accountsCache...)
	next[index] = item
	if err := writeJSON(s.accountsPath, next); err != nil {
		return err
	}
	s.replaceAccountsLocked(next)
	s.accountsDirty = false
	return nil
}
func quotaNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		r, _ := n.Int64()
		return int(r)
	case string:
		r, _ := strconv.Atoi(n)
		return r
	}
	return 0
}
