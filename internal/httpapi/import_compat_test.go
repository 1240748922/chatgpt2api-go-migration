package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresLegacyImportRetainsCredentialsAndResumesSync(t *testing.T) {
	var refreshes atomic.Int32
	at := migrationTestJWT("legacy")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshes.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"access_token": at, "refresh_token": "rt.rotated"})
		case "/backend-api/me":
			json.NewEncoder(w).Encode(map[string]any{"email": "legacy@example.test"})
		case "/backend-api/conversation/init":
			json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 7}}})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.DatabaseURL = testpg.DSN(t)
	cfg.OpenAIBaseURL = upstream.URL
	cfg.OpenAIOAuthURL = upstream.URL + "/oauth/token"
	a := New(cfg)
	added, skipped, rows, id, err := a.addImportedAccounts(context.Background(), []string{"opaque-grok-sso"}, []map[string]any{{"refreshToken": "rt.legacy", "email": "legacy@example.test", "proxy": "http://do-not-inherit.invalid", "quota": 999, "upload_cooldown_until": "2099-01-01T00:00:00Z"}})
	if err != nil || added != 2 || skipped != 0 || id == "" || len(rows) != 2 {
		t.Fatalf("import added=%d skipped=%d rows=%d job=%s err=%v", added, skipped, len(rows), id, err)
	}
	if refreshes.Load() != 0 {
		t.Fatal("legacy import blocked on upstream sync")
	}
	for _, row := range rows {
		if stringValue(row["email"]) == "legacy@example.test" && (row["proxy"] != nil || row["quota"] != nil || row["upload_cooldown_until"] != nil || stringValue(row["refresh_token"]) != "rt.legacy") {
			t.Fatal("imported stale state or lost RT")
		}
	}
	a.Close()
	b := New(cfg)
	defer b.Close()
	b.runImportJob(context.Background(), id)
	job, err := b.store.Document(importDocument(id))
	if err != nil || job["status"] != "completed" || intValue(job["sync_success"]) != 1 || refreshes.Load() != 1 {
		t.Fatalf("resume %v err=%v refreshes=%d", job, err, refreshes.Load())
	}
	fields, err := b.store.FindAccount(at)
	if err != nil || fields["refresh_token"] != "rt.rotated" || intValue(fields["quota"]) != 7 {
		t.Fatalf("rotation %v %v", fields, err)
	}
	added, skipped, _, _, err = b.addImportedAccounts(context.Background(), nil, []map[string]any{{"access_token": at, "refresh_token": "rt.rotated", "source_type": "chatgpt_web"}})
	if err != nil || added != 0 || skipped != 1 {
		t.Fatalf("duplicate %d %d %v", added, skipped, err)
	}
}

func TestAccountSelectionDetailsAndFailureClassification(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	at := migrationTestJWT("shown")
	_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"id": "shown", "access_token": at, "refresh_token": "rt.private", "email": "shown@example.test", "source_type": "chatgpt_web", "quota": 12, "status": "异常", "last_remote_check_result": "invalid"}, {"id": "valid-at", "access_token": migrationTestJWT("valid-at"), "refresh_token": "rt.reused", "refresh_token_invalid_at": "2026-10-14T00:00:00Z", "source_type": "chatgpt_web", "quota": 5}})
	if err != nil {
		t.Fatal(err)
	}
	r := adminRequest(s.Handler(), "POST", "/api/accounts/selection-preview", strings.NewReader(`{"selection":{"mode":"filtered","status":"abnormal"}}`))
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"selected_count":1`) {
		t.Fatalf("selection %d %s", r.Code, r.Body.String())
	}
	r = adminRequest(s.Handler(), "GET", "/api/accounts/shown", nil)
	if r.Code != 200 || strings.Contains(r.Body.String(), at) || strings.Contains(r.Body.String(), "rt.private") || !strings.Contains(r.Body.String(), `"status_category":"abnormal"`) {
		t.Fatalf("detail %d %s", r.Code, r.Body.String())
	}
	r = adminRequest(s.Handler(), "GET", "/api/accounts/shown/refresh-token", nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "rt.private") || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential read failed")
	}
	r = httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/api/accounts/shown/refresh-token", nil))
	if r.Code != 401 {
		t.Fatal("credential exposed without admin")
	}
	r = adminRequest(s.Handler(), "DELETE", "/api/accounts", strings.NewReader(`{"selection":{"mode":"filtered","status":"abnormal"}}`))
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"removed":1`) {
		t.Fatalf("delete %d %s", r.Code, r.Body.String())
	}
	if _, err = s.store.FindAccount("valid-at"); err != nil {
		t.Fatal("deleted usable AT with invalid RT")
	}
}
