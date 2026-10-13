package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageTaskOwnerIsolationAndUncertainSubmission(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	create := func(owner string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://local/api/image-tasks", strings.NewReader(`{"client_task_id":"same","prompt":"a cat","model":"gpt-image-2"}`))
		r = r.WithContext(context.WithValue(r.Context(), imageTaskOwnerKey{}, owner))
		w := httptest.NewRecorder()
		s.imageTasksAPI(w, r)
		return w
	}
	if w := create("a"); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	if w := create("b"); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	name := imageTaskDocument("a", "same")
	doc, _ := s.store.Document(name)
	task, _ := taskDecode(doc)
	task.Outputs[0].Stage = "submitting"
	_, _ = s.store.MutateDocument(name, func(d map[string]any) error {
		for k, v := range taskMap(task) {
			d[k] = v
		}
		return nil
	})
	lease, err := s.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s.runImageTask(context.Background(), name, lease)
	doc, _ = s.store.Document(name)
	task, _ = taskDecode(doc)
	if task.Status != "error" || task.Outputs[0].ErrorCode != "image_submission_uncertain" {
		t.Fatalf("unsafe restart: %#v", task)
	}
	other, _ := s.store.Document(imageTaskDocument("b", "same"))
	if other["status"] != "queued" {
		t.Fatal("owners collided")
	}
	public, _ := json.Marshal(imageTaskPublic(task))
	if strings.Contains(string(public), "owner_id") || strings.Contains(string(public), "access_token") {
		t.Fatal("private data exposed")
	}
}

func TestPostgresImageTaskResumeNeedsNoQuotaAndDoesNotRegenerate(t *testing.T) {
	var posts atomic.Int32
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a0t8AAAAASUVORK5CYII=")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts.Add(1)
			http.Error(w, "must not submit", 500)
			return
		}
		switch r.URL.Path {
		case "/backend-api/conversation/existing":
			fmt.Fprint(w, `{"mapping":{"result":{"message":{"author":{"role":"tool"},"create_time":1,"metadata":{"async_task_type":"image_gen"},"content":{"parts":[{"content_type":"image_asset_pointer","asset_pointer":"file-service://file_saved"}]}}}}}`)
		case "/backend-api/files/file_saved/download":
			fmt.Fprintf(w, `{"download_url":%q}`, "http://"+r.Host+"/blob")
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			w.Write(image)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.DatabaseURL = testpg.DSN(t)
	cfg.OpenAIBaseURL = upstream.URL
	cfg.RequestTimeout = 10 * time.Second
	s := New(cfg)
	defer s.Close()
	_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"id": "original", "access_token": migrationTestJWT("original"), "quota": 0, "source_type": "chatgpt_web"}})
	if err != nil {
		t.Fatal(err)
	}
	task := imageTaskState{ID: "resume", OwnerID: "test", Status: "running", Mode: "generate", Model: "gpt-image-2", N: 1, Size: "1024x1024", Quality: "auto", PublicBase: "https://images.example.test", Outputs: []imageTaskOutput{{Stage: "submitted", AccountID: "original", ConversationID: "existing"}}}
	name := imageTaskDocument(task.OwnerID, task.ID)
	_, _ = s.store.MutateDocument(name, func(d map[string]any) error {
		for k, v := range taskMap(task) {
			d[k] = v
		}
		return nil
	})
	_, _ = s.store.MutateDocument("imagepayload-"+strings.TrimPrefix(name, "imagetask-"), func(d map[string]any) error { d["prompt"] = "test"; return nil })
	lease, err := s.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s.runImageTask(context.Background(), name, lease)
	doc, _ := s.store.Document(name)
	task, _ = taskDecode(doc)
	if task.Status != "success" {
		t.Fatalf("resume: %#v", task)
	}
	if !strings.HasPrefix(task.Outputs[0].Data[0]["url"], task.PublicBase) {
		t.Fatalf("public url: %#v", task.Outputs)
	}
	s.runImageTask(context.Background(), name, lease)
	account, _ := s.store.FindAccount("original")
	if posts.Load() != 0 || intValue(account["image_generated_count"]) != 1 || intValue(account["quota"]) != 0 {
		t.Fatalf("replayed generation: posts=%d account=%#v", posts.Load(), account)
	}
}
