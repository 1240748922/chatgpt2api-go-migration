package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"time"
)

var ErrLeaseLost = errors.New("credential lease lost")
var ErrCredentialsChanged = errors.New("account credentials changed")

func sameCredentials(a, b map[string]any) bool {
	for _, key := range []string{"id", "access_token", "refresh_token", "id_token"} {
		if stringValue(a[key]) != stringValue(b[key]) {
			return false
		}
	}
	return quotaNumber(a["credential_generation"]) == quotaNumber(b["credential_generation"])
}

// DeleteAccount rejects stale cleanup decisions as well as stale lease owners.
func (l *Lease) DeleteAccount(ctx context.Context, expected map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := l.store
	want, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	if s.pg != nil {
		tx, err := s.pg.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT owner FROM go_leases WHERE resource=$1 AND owner=$2 AND expires_at>clock_timestamp() FOR UPDATE`, l.Resource, l.Owner).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		revision, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE go_accounts SET deleted=true,data='{}',token_key=id,rt_key=NULL,revision=$1 WHERE id=$2 AND NOT deleted AND data=$3::jsonb`, revision, stringValue(expected["id"]), string(want))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCredentialsChanged
		}
		if err = tx.Commit(); err == nil {
			s.invalidatePG()
		}
		return err
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	active := s.localLeases[l.Resource]
	if active.owner != l.Owner || !active.until.After(time.Now()) {
		return ErrLeaseLost
	}
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(s.accountsCache))
	found := false
	for _, item := range s.accountsCache {
		if accountToken(item) == accountToken(expected) {
			got, e := json.Marshal(item)
			if e != nil {
				return e
			}
			if string(got) != string(want) {
				return ErrCredentialsChanged
			}
			found = true
			continue
		}
		items = append(items, item)
	}
	if !found {
		return os.ErrNotExist
	}
	if err = writeJSON(s.accountsPath, items); err != nil {
		return err
	}
	s.replaceAccountsLocked(items)
	s.accountsDirty = false
	return nil
}

// UpdateAccount fences both lease ownership and credential identity in the same
// transaction as the write. A paused former worker cannot overwrite a new RT.
func (l *Lease) UpdateAccount(ctx context.Context, expected, updates map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := l.store
	if s.pg != nil {
		return s.pgUpdateFenced(ctx, accountToken(expected), updates, l, expected)
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	active := s.localLeases[l.Resource]
	if active.owner != l.Owner || !active.until.After(time.Now()) {
		return nil, ErrLeaseLost
	}
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, err
	}
	found := false
	for _, item := range s.accountsCache {
		if accountToken(item) == accountToken(expected) {
			found = true
			if !sameCredentials(item, expected) {
				return nil, ErrCredentialsChanged
			}
			break
		}
	}
	if !found {
		return nil, os.ErrNotExist
	}
	items, next, ok := s.updatedAccountsLocked(accountToken(expected), updates)
	if !ok {
		return nil, os.ErrNotExist
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := writeJSON(s.accountsPath, items); err != nil {
		return nil, err
	}
	s.replaceAccountsLocked(items)
	s.accountsDirty = false
	return cloneMap(next), nil
}
