package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresNineReplicasInitializeEmptySchema(t *testing.T) {
	dsn := testpg.DSN(t)
	root := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			base := filepath.Join(root, fmt.Sprint(i))
			s, err := Open(filepath.Join(base, "a.json"), filepath.Join(base, "k.json"), filepath.Join(base, "c.json"), dsn)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			if s.pg.Stats().MaxOpenConnections != 8 {
				t.Error("unexpected connection budget")
			}
			if _, _, err = s.AccountSnapshot(); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

func pgTestStore(t *testing.T, dsn string) *Store {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestPostgresSharedSnapshotRotationAndTombstones(t *testing.T) {
	dsn := testpg.DSN(t)
	a, b := pgTestStore(t, dsn), pgTestStore(t, dsn)
	added, _, _, err := a.AddAccounts(nil, []map[string]any{{"id": "one", "access_token": "old-at", "refresh_token": "old-rt"}})
	if err != nil || added != 1 {
		t.Fatalf("add: %d %v", added, err)
	}
	items, revision, err := b.AccountSnapshot()
	if err != nil || len(items) != 1 {
		t.Fatalf("snapshot: %v", err)
	}
	rotated, _, err := a.RotateAccountTokens("old-at", "new-at", "new-rt", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rotated["id"] != "one" {
		t.Fatal("rotation changed stable id")
	}
	if _, err = b.FindAccount("old-at"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed token still indexed: %v", err)
	}
	b.invalidatePG()
	items, next, err := b.AccountSnapshot()
	if err != nil || next <= revision || accountToken(items[0]) != "new-at" {
		t.Fatalf("incremental rotation: %v", err)
	}
	if _, _, _, err = a.AddAccounts(nil, []map[string]any{{"access_token": "duplicate-at", "refresh_token": "new-rt"}}); err != nil {
		t.Fatal(err)
	}
	items, _, err = a.AccountSnapshot()
	if err != nil || len(items) != 1 {
		t.Fatalf("RT dedupe: %v", err)
	}
	if _, _, err = a.DeleteAccounts([]string{"new-at"}); err != nil {
		t.Fatal(err)
	}
	b.invalidatePG()
	items, _, err = b.AccountSnapshot()
	if err != nil || len(items) != 0 {
		t.Fatalf("tombstone: %v", err)
	}
}

func TestPostgresLeaseOwnershipAndConcurrentDocuments(t *testing.T) {
	dsn := testpg.DSN(t)
	a, b := pgTestStore(t, dsn), pgTestStore(t, dsn)
	ctx := context.Background()
	first, err := a.AcquireLease(ctx, "image:test:0", time.Minute)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if second, err := b.AcquireLease(ctx, first.Resource, time.Minute); err != nil || second != nil {
		t.Fatalf("lease double dispatch: %v", err)
	}
	if _, err = a.pg.Exec(`UPDATE go_leases SET expires_at=now()-interval '1 second' WHERE resource=$1`, first.Resource); err != nil {
		t.Fatal(err)
	}
	second, err := b.AcquireLease(ctx, first.Resource, time.Minute)
	if err != nil || second == nil {
		t.Fatal(err)
	}
	first.Release()
	if ok, err := second.Renew(ctx, time.Minute); err != nil || !ok {
		t.Fatalf("old owner released new lease: %v", err)
	}
	second.Release()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			_, err := s.MutateDocument("counter", func(d map[string]any) error { d["value"] = testNumber(d["value"]) + 1; return nil })
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	doc, err := a.Document("counter")
	if err != nil || testNumber(doc["value"]) != 40 {
		t.Fatalf("lost update: %v %v", doc, err)
	}
	_, err = a.PatchConfig(map[string]any{"one": 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.PatchConfig(map[string]any{"two": 2})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.Config()
	if err != nil || cfg["one"] == nil || cfg["two"] == nil {
		t.Fatalf("config merge: %v %v", cfg, err)
	}
}

func testNumber(v any) float64 { n, _ := v.(float64); return n }
