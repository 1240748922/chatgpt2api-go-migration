package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresEditableResumeAndSavedDeliveryNeverResubmit(t *testing.T) {
	var posts, gets atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts.Add(1)
			http.Error(w, "unexpected submit", 500)
			return
		}
		gets.Add(1)
		switch r.URL.Path {
		case "/backend-api/conversation/existing":
			_ = json.NewEncoder(w).Encode(map[string]any{"attachments": []any{map[string]any{"file_id": "file_primary", "name": "slides.pptx"}, map[string]any{"file_id": "file_archive", "name": "assets.zip"}}})
		case "/backend-api/conversation/existing/attachment/file_primary/download", "/backend-api/conversation/existing/attachment/file_archive/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("PK-local-test-artifact"))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.DatabaseURL = testpg.DSN(t)
	cfg.OpenAIBaseURL = upstream.URL
	a := New(cfg)
	defer a.Close()
	if _, _, _, err := a.store.AddAccounts(nil, []map[string]any{{"id": "original", "access_token": migrationTestJWT("original"), "source_type": "chatgpt_web", "plan_type": "plus", "quota": 0}}); err != nil {
		t.Fatal(err)
	}
	task := editableFileTaskState{ID: "recover", TaskID: "recover", Owner: "owner", Status: "queued", Stage: "submitted", Kind: "ppt", AccountID: "original", ConversationID: "existing", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	name := editableTaskDocument(task.Owner, task.ID)
	if _, err := a.store.MutateDocument(name, func(d map[string]any) error {
		for k, v := range documentMap(task) {
			d[k] = v
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(cfg.DataDir, "files")
	if err := os.MkdirAll(cfg.DataDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(s *Server) {
		t.Helper()
		lease, err := s.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		defer lease.Release()
		// Another instance must not dispatch the same task.
		competing, err := a.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
		if err != nil || competing != nil {
			t.Fatalf("double dispatch: %v", err)
		}
		s.runEditableFileTask(context.Background(), name, lease)
	}
	run(a)
	doc, _ := a.store.Document(name)
	failed, _ := editableTaskDecode(doc)
	if failed.Status != "error" || failed.ExportDocument == "" {
		t.Fatalf("export was not checkpointed: %#v", failed)
	}
	before := gets.Load()
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	fields, err := a.store.FindAccount("original")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = a.store.UpdateAccount(accountToken(fields), map[string]any{"access_token": "expired"}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.MutateDocument(name, func(d map[string]any) error { d["status"] = "queued"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Independent process configuration, shared PostgreSQL and media volume.
	b := New(cfg)
	defer b.Close()
	run(b)
	run(b)
	doc, _ = b.store.Document(name)
	done, _ := editableTaskDecode(doc)
	if done.Status != "success" || posts.Load() != 0 || gets.Load() != before {
		t.Fatalf("delivery regenerated: %#v posts=%d gets=%d/%d", done, posts.Load(), gets.Load(), before)
	}
	public, _ := json.Marshal(editableTaskPublic(&done))
	if strings.Contains(string(public), "fileexport-") || strings.Contains(string(public), "account_id") || strings.Contains(string(public), "owner_id") {
		t.Fatal("private data leaked")
	}
}

func TestEditableUncertainSubmissionAndFrozenElapsed(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	task := editableFileTaskState{ID: "uncertain", Owner: "owner", Status: "queued", Stage: "submitting", Kind: "ppt"}
	name := editableTaskDocument(task.Owner, task.ID)
	if _, err := s.store.MutateDocument(name, func(d map[string]any) error {
		for k, v := range documentMap(task) {
			d[k] = v
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s.runEditableFileTask(context.Background(), name, lease)
	doc, _ := s.store.Document(name)
	done, _ := editableTaskDecode(doc)
	if done.Status != "error" || done.ErrorCode != "editable_submission_uncertain" {
		t.Fatalf("unsafe restart: %#v", done)
	}
	done.StartedAt = time.Now().Add(-time.Hour)
	done.UpdatedAt = done.StartedAt.Add(10 * time.Second).UTC().Format(time.RFC3339Nano)
	if editableTaskPublic(&done)["elapsed_seconds"] != 10 {
		t.Fatal("terminal elapsed kept growing")
	}
}

func TestEditableDeleteProtectsOwnerAndRunningTask(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	task := editableFileTaskState{ID: "delete-test", Owner: "owner", Kind: "ppt", Status: "queued"}
	name := editableTaskDocument(task.Owner, task.ID)
	put := func() {
		t.Helper()
		if _, err := s.store.MutateDocument(name, func(d map[string]any) error {
			for k, v := range documentMap(task) {
				d[k] = v
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	put()
	remove := func(owner string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/v1/editable-file-tasks/"+task.ID, nil)
		r.Header.Set("Authorization", "Bearer api-secret")
		r = r.WithContext(context.WithValue(r.Context(), imageTaskOwnerKey{}, owner))
		w := httptest.NewRecorder()
		s.editableFileTaskByID(w, r)
		return w
	}
	if w := remove("other-owner"); w.Code != 404 {
		t.Fatal("cross-owner task visible")
	}
	if w := remove("owner"); w.Code != 409 {
		t.Fatal("queued task deleted")
	}
	task.Status = "success"
	put()
	lease, err := s.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if w := remove("owner"); w.Code != 409 {
		t.Fatal("leased task deleted")
	}
	lease.Release()
	directory := filepath.Join(s.cfg.DataDir, "files", editableRelativeDirectory(task))
	if err := os.MkdirAll(directory, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "slides.pptx")
	if err := os.WriteFile(path, []byte("PK-test"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"filepayload-", "fileexport-"} {
		_, err = s.store.MutateDocument(editablePrivateDocument(name, prefix), func(d map[string]any) error { d["private"] = true; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if w := remove("owner"); w.Code != 200 {
		t.Fatalf("delete %d %s", w.Code, w.Body.String())
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("output remains")
	}
	for _, document := range []string{name, editablePrivateDocument(name, "filepayload-"), editablePrivateDocument(name, "fileexport-")} {
		d, err := s.store.Document(document)
		if err != nil || len(d) != 0 {
			t.Fatalf("private task remains %s %v", document, err)
		}
	}
}
