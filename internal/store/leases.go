package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type localLease struct {
	owner string
	until time.Time
}
type Lease struct {
	store           *Store
	Resource, Owner string
	stop            chan struct{}
	once            sync.Once
}

// AcquireLease uses database time, so worker clock skew cannot steal an active lease.
func (s *Store) AcquireLease(ctx context.Context, resource string, ttl time.Duration) (*Lease, error) {
	if ttl < time.Second {
		ttl = time.Second
	}
	owner := randomID() + randomID()
	ok, err := s.leaseWrite(ctx, resource, owner, ttl, false)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &Lease{store: s, Resource: resource, Owner: owner, stop: make(chan struct{})}, nil
}
func (s *Store) leaseWrite(ctx context.Context, resource, owner string, ttl time.Duration, renew bool) (bool, error) {
	if s.pg == nil {
		s.leaseMu.Lock()
		defer s.leaseMu.Unlock()
		if s.localLeases == nil {
			s.localLeases = map[string]localLease{}
		}
		old := s.localLeases[resource]
		if renew {
			if old.owner != owner || old.until.Before(time.Now()) {
				return false, nil
			}
		} else if old.until.After(time.Now()) {
			return false, nil
		}
		s.localLeases[resource] = localLease{owner: owner, until: time.Now().Add(ttl)}
		return true, nil
	}
	query := `INSERT INTO go_leases(resource,owner,expires_at) VALUES($1,$2,now()+$3*interval '1 millisecond') ON CONFLICT(resource) DO UPDATE SET owner=excluded.owner,expires_at=excluded.expires_at WHERE go_leases.expires_at<=now() RETURNING owner`
	if renew {
		query = `UPDATE go_leases SET expires_at=now()+$3*interval '1 millisecond' WHERE resource=$1 AND owner=$2 AND expires_at>now() RETURNING owner`
	}
	var got string
	err := s.pg.QueryRowContext(ctx, query, resource, owner, ttl.Milliseconds()).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (l *Lease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	return l.store.leaseWrite(ctx, l.Resource, l.Owner, ttl, true)
}
func (l *Lease) KeepAlive(ctx context.Context, ttl time.Duration, onLost func()) {
	go func() {
		timer := time.NewTicker(ttl / 3)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.stop:
				return
			case <-timer.C:
				check, cancel := context.WithTimeout(ctx, 5*time.Second)
				ok, err := l.Renew(check, ttl)
				cancel()
				if err != nil || !ok {
					if onLost != nil {
						onLost()
					}
					return
				}
			}
		}
	}()
}
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		s := l.store
		if s.pg == nil {
			s.leaseMu.Lock()
			if s.localLeases[l.Resource].owner == l.Owner {
				delete(s.localLeases, l.Resource)
			}
			s.leaseMu.Unlock()
			return
		}
		ctx, cancel := dbContext()
		defer cancel()
		_, _ = s.pg.ExecContext(ctx, `DELETE FROM go_leases WHERE resource=$1 AND owner=$2`, l.Resource, l.Owner)
	})
}
func (s *Store) ActiveLeases(prefix string) (int, error) {
	if s.pg == nil {
		s.leaseMu.Lock()
		defer s.leaseMu.Unlock()
		n := 0
		for k, l := range s.localLeases {
			if l.until.After(time.Now()) && strings.HasPrefix(k, prefix) {
				n++
			}
		}
		return n, nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	var n int
	err := s.pg.QueryRowContext(ctx, `SELECT count(*) FROM go_leases WHERE expires_at>now() AND left(resource,length($1))=$1`, prefix).Scan(&n)
	return n, err
}
func (s *Store) documentPath(name string) string {
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		panic("invalid document name")
	}
	root := filepath.Join(filepath.Dir(s.accountsPath), "jobs")
	if name == "" {
		return root
	}
	return filepath.Join(root, name+".json")
}
func AccountResource(item map[string]any) string {
	if id := stringValue(item["id"]); id != "" {
		return id
	}
	return hashKey(accountToken(item))
}
func SlotResource(item map[string]any, slot int) string {
	return fmt.Sprintf("image:%s:%d", AccountResource(item), slot)
}
