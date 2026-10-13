package store

import (
	"context"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresImportChunkAtomicReplayAndOwnerFence(t *testing.T) {
	dsn := testpg.DSN(t)
	a, b := pgTestStore(t, dsn), pgTestStore(t, dsn)
	name := "importchunk-test-000000"
	if _, err := a.MutateDocument(name, func(d map[string]any) error { d["total"] = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	lease, err := a.AcquireLease(context.Background(), "import:test", time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	rows := []map[string]any{{"id": "one", "access_token": "synthetic-at"}, {"id": "two", "access_token": "synthetic-at"}}
	// Fail precisely after account inserts, when the durable chunk checkpoint is committed.
	_, err = a.pg.Exec(`CREATE FUNCTION fail_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'checkpoint test failure'; END $$;
 CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON go_documents FOR EACH ROW EXECUTE FUNCTION fail_checkpoint()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = lease.ImportChunk(name, rows); err == nil {
		t.Fatal("checkpoint failure was hidden")
	}
	items, _, err := b.AccountSnapshot()
	if err != nil || len(items) != 0 {
		t.Fatalf("account insert escaped rollback: %d %v", len(items), err)
	}
	if _, err = a.pg.Exec(`DROP TRIGGER fail_checkpoint ON go_documents`); err != nil {
		t.Fatal(err)
	}
	added, skipped, err := lease.ImportChunk(name, rows)
	if err != nil || added != 1 || skipped != 1 {
		t.Fatalf("import: %d %d %v", added, skipped, err)
	}
	// A restart must retain the original added/skipped counts, even with different input.
	added, skipped, err = lease.ImportChunk(name, []map[string]any{{"access_token": "must-not-insert"}})
	if err != nil || added != 1 || skipped != 1 {
		t.Fatalf("replay changed counts: %d %d %v", added, skipped, err)
	}
	items, _, err = a.AccountSnapshot()
	if err != nil || len(items) != 1 {
		t.Fatalf("replay inserted accounts: %d %v", len(items), err)
	}
	if _, err = a.pg.Exec(`UPDATE go_leases SET expires_at=now()-interval '1 second' WHERE resource=$1`, lease.Resource); err != nil {
		t.Fatal(err)
	}
	next, err := b.AcquireLease(context.Background(), lease.Resource, time.Minute)
	if err != nil || next == nil {
		t.Fatal(err)
	}
	defer next.Release()
	if _, _, err = lease.ImportChunk(name, rows); err == nil {
		t.Fatal("expired owner committed an import")
	}
	if added, skipped, err = next.ImportChunk(name, rows); err != nil || added != 1 || skipped != 1 {
		t.Fatalf("new owner lost checkpoint: %d %d %v", added, skipped, err)
	}
}
