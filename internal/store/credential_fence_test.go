package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestCredentialFence(t *testing.T) {
	for _, backend := range []string{"json", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			s := New(filepath.Join(root, "a.json"), filepath.Join(root, "k.json"), filepath.Join(root, "c.json"))
			if backend == "postgres" {
				s = pgTestStore(t, testpg.DSN(t))
			} else {
				t.Cleanup(func() { _ = s.Close() })
			}
			_, _, _, err := s.AddAccounts(nil, []map[string]any{{"id": "account", "access_token": "at", "refresh_token": "rt", "credential_generation": 1}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			stale, err := s.AcquireLease(ctx, "credential:account", time.Minute)
			if err != nil || stale == nil {
				t.Fatal(err)
			}
			expected, err := s.FindAccount("account")
			if err != nil {
				t.Fatal(err)
			}
			if s.pg != nil {
				if _, err = s.pg.Exec(`UPDATE go_leases SET expires_at=now()-interval '1 second' WHERE resource=$1`, stale.Resource); err != nil {
					t.Fatal(err)
				}
			} else {
				s.leaseMu.Lock()
				s.localLeases[stale.Resource] = localLease{owner: stale.Owner, until: time.Now().Add(-time.Second)}
				s.leaseMu.Unlock()
			}
			current, err := s.AcquireLease(ctx, stale.Resource, time.Minute)
			if err != nil || current == nil {
				t.Fatal(err)
			}
			defer current.Release()
			rotated, err := current.UpdateAccount(ctx, expected, RotationUpdates("at-new", "rt-new", "", map[string]any{"credential_generation": 2}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = stale.UpdateAccount(ctx, expected, map[string]any{"refresh_token": "old-overwrite"}); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("stale worker write accepted", err)
			}
			// Releasing the former owner must leave the new owner's lease intact.
			stale.Release()
			if _, err = current.UpdateAccount(ctx, rotated, map[string]any{"quota": 7}); err != nil {
				t.Fatal(err)
			}
			// Credential CAS also catches a change made outside this worker's lease.
			_, _, err = s.UpdateAccount("at-new", map[string]any{"refresh_token": "rt-newer", "credential_generation": 3})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = current.UpdateAccount(ctx, rotated, map[string]any{"quota": 0}); !errors.Is(err, ErrCredentialsChanged) {
				t.Fatal("credential change not fenced", err)
			}
			saved, err := s.FindAccount("account")
			if err != nil || saved["refresh_token"] != "rt-newer" || quotaNumber(saved["quota"]) != 7 {
				t.Fatal("new credentials damaged", saved, err)
			}
		})
	}
}

func TestCleanupFenceRejectsChangedAccountAndExpiredLease(t *testing.T) {
	for _, backend := range []string{"json", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			var s *Store
			if backend == "postgres" {
				s = pgTestStore(t, testpg.DSN(t))
			} else {
				s = New(filepath.Join(root, "a.json"), filepath.Join(root, "k.json"), filepath.Join(root, "c.json"))
				t.Cleanup(func() { _ = s.Close() })
			}
			_, _, _, err := s.AddAccounts(nil, []map[string]any{{"id": "cleanup", "access_token": "at-cleanup", "quota": 0}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			lease, err := s.AcquireLease(ctx, "credential:cleanup", time.Minute)
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			defer lease.Release()
			before, _ := s.FindAccount("cleanup")
			if _, _, err = s.UpdateAccount("at-cleanup", map[string]any{"quota": 7}); err != nil {
				t.Fatal(err)
			}
			if err = lease.DeleteAccount(ctx, before); !errors.Is(err, ErrCredentialsChanged) {
				t.Fatal("stale cleanup decision accepted", err)
			}
			current, _ := s.FindAccount("cleanup")
			if s.pg != nil {
				_, err = s.pg.Exec(`UPDATE go_leases SET expires_at=now()-interval '1 second' WHERE resource=$1`, lease.Resource)
			} else {
				s.leaseMu.Lock()
				s.localLeases[lease.Resource] = localLease{owner: lease.Owner, until: time.Now().Add(-time.Second)}
				s.leaseMu.Unlock()
				err = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = lease.DeleteAccount(ctx, current); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("expired lease deleted account", err)
			}
			newOwner, err := s.AcquireLease(ctx, lease.Resource, time.Minute)
			if err != nil || newOwner == nil {
				t.Fatal(err)
			}
			defer newOwner.Release()
			if err = newOwner.DeleteAccount(ctx, current); err != nil {
				t.Fatal(err)
			}
			if _, err = s.FindAccount("cleanup"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fenced delete missing", err)
			}
		})
	}
}
