package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func importTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	st := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	t.Cleanup(func() { _ = st.Close() })
	return &Server{store: st, backgroundCtx: context.Background()}
}

func TestSessionJSONImportEndpointQueuesParsedCredentials(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	request := `{"mode":"session_json","text":"{\"auth_session\":{\"accessToken\":\"session-at\",\"refreshToken\":\"session-rt\"},\"user\":{\"email\":\"session@example.test\"},\"proxy\":\"http://discard.invalid\"}"}`
	response := adminRequest(s.Handler(), "POST", "/api/account-import-jobs", strings.NewReader(request))
	if response.Code != 202 {
		t.Fatalf("session job not queued: %d %s", response.Code, response.Body.String())
	}
	var job map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	chunk, err := s.store.Document(importChunk(stringValue(job["id"]), 0))
	if err != nil {
		t.Fatal(err)
	}
	rows := importRows(chunk["accounts"])
	if len(rows) != 1 || stringValue(rows[0]["access_token"]) != "session-at" || stringValue(rows[0]["refresh_token"]) != "session-rt" || stringValue(rows[0]["email"]) != "session@example.test" || rows[0]["proxy"] != nil {
		t.Fatalf("session credentials not normalized: %#v", rows)
	}
}

func TestImportProgressReconcilesRestartAndStablePagination(t *testing.T) {
	s := importTestServer(t)
	rows := []map[string]any{}
	for i := 0; i < 405; i++ {
		rows = append(rows, map[string]any{"id": fmt.Sprintf("row-%08d", i), "access_token": migrationTestJWT(fmt.Sprint(i))})
	}
	job, err := s.createImportJob(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	id := stringValue(job["id"])
	for chunk := 0; chunk < 3; chunk++ {
		_, err = s.store.MutateDocument(importChunk(id, chunk), func(d map[string]any) error {
			d["imported"] = true
			d["added"] = d["total"]
			results := map[string]any{}
			for _, key := range stringSliceValue(d["result_order"]) {
				results[key] = map[string]any{"key": key, "status": "success", "time": "2026-10-13T00:00:00Z"}
			}
			d["results"] = results
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !s.reconcileImportProgress(id, true) {
		t.Fatal("reconcile failed")
	}
	job, _ = s.store.Document(importDocument(id))
	if intValue(job["sync_processed"]) != 405 || intValue(job["import_processed"]) != 405 || len(importRows(job["events"])) != 200 {
		t.Fatalf("progress %v", job)
	}
	s.importProgressAt = time.Time{}
	if !s.reconcileImportProgress(id, false, 1) {
		t.Fatal("incremental failed")
	}
	job, _ = s.store.Document(importDocument(id))
	if intValue(job["sync_processed"]) != 405 {
		t.Fatal("double counted")
	}
	seen := map[string]bool{}
	for offset := 0; offset < 405; {
		events, next, err := s.importEvents(id, job, offset, 137)
		if err != nil {
			t.Fatal(err)
		}
		if next <= offset {
			t.Fatal("cursor stalled")
		}
		offset = next
		for _, result := range events {
			key := stringValue(result["key"])
			if seen[key] {
				t.Fatal("duplicate page row")
			}
			seen[key] = true
		}
	}
	if len(seen) != 405 {
		t.Fatalf("missing rows: %d", len(seen))
	}
	// Simulate losing the public counters while durable chunk results survive.
	_, _ = s.store.MutateDocument(importDocument(id), func(d map[string]any) error { d["sync_success"] = 0; d["chunks_progress"] = nil; return nil })
	if !s.reconcileImportProgress(id, true) {
		t.Fatal("restart failed")
	}
	job, _ = s.store.Document(importDocument(id))
	if intValue(job["sync_success"]) != 405 {
		t.Fatal("restart lost results")
	}
}
func TestImportRetryUsesCurrentAccountAndBlocksUncertainRT(t *testing.T) {
	s := importTestServer(t)
	rows := []map[string]any{{"id": "good", "access_token": migrationTestJWT("good"), "refresh_token": "rt.current"}, {"id": "uncertain", "access_token": migrationTestJWT("uncertain"), "refresh_token": "rt.uncertain", "refresh_uncertain": true}}
	if _, _, _, err := s.store.AddAccounts(nil, rows); err != nil {
		t.Fatal(err)
	}
	job, err := s.createImportJob(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	id := stringValue(job["id"])
	_, _ = s.store.MutateDocument(importChunk(id, 0), func(d map[string]any) error {
		d["results"] = map[string]any{"good": map[string]any{"status": "failed", "account_id": "good"}, "uncertain": map[string]any{"status": "failed", "account_id": "uncertain"}}
		return nil
	})
	retry, blocked, err := s.importRetryRows(id, job)
	if err != nil || blocked != 1 || len(retry) != 1 {
		t.Fatalf("retry=%v blocked=%d err=%v", retry, blocked, err)
	}
	if retry[0]["account_id"] != "good" || retry[0]["refresh_token"] != nil {
		t.Fatal("retry retained old RT")
	}
}
