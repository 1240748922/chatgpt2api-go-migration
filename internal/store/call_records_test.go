package store

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func testLogQueries(t *testing.T, a, b *Store) {
	t.Helper()
	for _, row := range []map[string]any{
		{"id": "one", "type": "call", "time": time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano), "detail": map[string]any{"model": "gpt-image-2", "status": "failed", "account_email": "old@example.test"}},
		{"id": "two", "type": "call", "time": time.Now().UTC().Format(time.RFC3339Nano), "detail": map[string]any{"model": "gpt-image-2", "status": "success", "account_email": "new@example.test"}},
	} {
		if err := a.AppendCallRecord(row); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := b.QueryCallRecords(url.Values{"status": {"failed"}, "account": {"old@"}}, 1, 0)
	if err != nil || total != 1 || len(rows) != 1 || rows[0]["id"] != "one" {
		t.Fatalf("rows=%v total=%d err=%v", rows, total, err)
	}
	rows, total, err = b.QueryCallRecords(url.Values{}, 1, 1)
	if err != nil || total != 2 || len(rows) != 1 || rows[0]["id"] != "one" {
		t.Fatalf("pagination: %v %d %v", rows, total, err)
	}
	removed, err := a.DeleteCallRecords(nil, time.Now().Add(-24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("retention: %d %v", removed, err)
	}
	removed, err = b.DeleteCallRecords([]string{"two"}, time.Time{})
	if err != nil || removed != 1 {
		t.Fatalf("delete: %d %v", removed, err)
	}
}
func TestLocalCallRecordQueries(t *testing.T) {
	root := t.TempDir()
	a := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	defer a.Close()
	testLogQueries(t, a, a)
}
func TestPostgresSharedCallRecordsAndRuntime(t *testing.T) {
	dsn := testpg.DSN(t)
	root := t.TempDir()
	open := func() *Store {
		s, err := Open(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, b := open(), open()
	testLogQueries(t, a, b)
	if err := a.PublishRuntime("replica-a", map[string]any{"active": []map[string]any{{"call_id": "call-a"}}}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := b.RuntimeSnapshots()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("runtime %v %v", snapshots, err)
	}
	if err = b.RequestRuntimeCancel("call-a", "replica-a"); err != nil {
		t.Fatal(err)
	}
	ids, err := a.ConsumeRuntimeCancels("replica-a")
	if err != nil || len(ids) != 1 || ids[0] != "call-a" {
		t.Fatalf("cancel %v %v", ids, err)
	}
	ids, err = a.ConsumeRuntimeCancels("replica-a")
	if err != nil || len(ids) != 0 {
		t.Fatal("cancel command was repeated")
	}
	if err = a.RemoveRuntime("replica-a"); err != nil {
		t.Fatal(err)
	}
	snapshots, err = b.RuntimeSnapshots()
	if err != nil || len(snapshots) != 0 {
		t.Fatal("stale runtime")
	}
}
