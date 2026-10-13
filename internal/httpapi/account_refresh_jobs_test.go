package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresManualRefreshRecoveryBusyAndAliasDedup(t *testing.T) {
	var checks atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/me":
			checks.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "second@example.test"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 7}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{}})
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.DatabaseURL = testpg.DSN(t)
	cfg.OpenAIBaseURL = upstream.URL
	a := New(cfg)
	defer a.Close()
	b := New(cfg)
	defer b.Close()
	at := migrationTestJWT("first")
	if _, _, _, err := a.store.AddAccounts(nil, []map[string]any{{"id": "first", "email": "first@example.test", "access_token": at, "source_type": "chatgpt_web"}, {"id": "second", "access_token": migrationTestJWT("second"), "source_type": "chatgpt_web"}}); err != nil {
		t.Fatal(err)
	}
	id, err := a.createAccountRefresh([]string{"first", at, "FIRST@example.test", "second"})
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := a.store.Document("refreshjob-" + id)
	if intValue(doc["total"]) != 2 {
		t.Fatalf("aliases duplicated: %v", doc)
	}
	chunk, _ := a.store.Document(refreshChunk(id, 0))
	rows := importRows(chunk["rows"])
	// Account result committed before the process died; public counters were not saved.
	if _, err = a.store.MutateDocument(refreshChunk(id, 0), func(d map[string]any) error {
		d["results"] = map[string]any{stringValue(rows[0]["key"]): map[string]any{"success": true, "category": "正常", "quota": 5}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fields, err := a.store.FindAccount("second")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := a.store.AcquireLease(context.Background(), store.SlotResource(fields, 0), time.Minute)
	if err != nil || slot == nil {
		t.Fatal(err)
	}
	b.runAccountRefresh(context.Background(), id)
	if checks.Load() != 0 {
		t.Fatal("busy account was synchronized")
	}
	doc, _ = a.store.Document("refreshjob-" + id)
	if doc["status"] == "completed" {
		t.Fatal("busy account incorrectly completed")
	}
	slot.Release()
	b.runAccountRefresh(context.Background(), id)
	a.runAccountRefresh(context.Background(), id)
	doc, _ = a.store.Document("refreshjob-" + id)
	if doc["status"] != "completed" || intValue(doc["processed"]) != 2 || intValue(doc["success"]) != 2 || checks.Load() != 1 {
		t.Fatalf("recovery: %v checks=%d", doc, checks.Load())
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), at) {
		t.Fatal("progress retained credentials")
	}
}

func TestRefreshProgressIsBoundedAndIdempotent(t *testing.T) {
	s := importTestServer(t)
	refs := make([]string, 450)
	for i := range refs {
		refs[i] = fmt.Sprintf("missing-%d", i)
	}
	id, err := s.createAccountRefresh(refs)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.store.AcquireLease(context.Background(), "refresh-dispatch", time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	for i := 0; i < 3; i++ {
		if _, err = s.store.MutateDocument(refreshChunk(id, i), func(d map[string]any) error {
			results := map[string]any{}
			for _, row := range importRows(d["rows"]) {
				results[stringValue(row["key"])] = map[string]any{"success": false, "category": "异常", "error": "not found"}
			}
			d["results"] = results
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !s.reconcileRefresh(lease, id, i) || !s.reconcileRefresh(lease, id, i) {
			t.Fatal("reconcile failed")
		}
	}
	doc, _ := s.store.Document("refreshjob-" + id)
	if intValue(doc["processed"]) != 450 || intValue(doc["failed"]) != 450 || len(importRows(doc["errors"])) != 200 {
		t.Fatalf("unbounded or doubled progress: processed=%v failed=%v errors=%d", doc["processed"], doc["failed"], len(importRows(doc["errors"])))
	}
	for _, part := range mapValue(doc["chunk_progress"]) {
		if mapValue(part)["errors"] != nil {
			t.Fatal("full error list stored in each chunk summary")
		}
	}
}
