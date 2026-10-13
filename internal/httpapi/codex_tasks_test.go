package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestCodexTaskStorageRecoveryNeverResubmits(t *testing.T) {
	var posts atomic.Int32
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Method != "POST" {
			t.Errorf("unexpected Web route: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 || mapValue(tools[0])["action"] != "edit" {
			t.Error("edit action missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":%q}}\n\n", base64.StdEncoding.EncodeToString(raw.Bytes()))
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.DatabaseURL = testpg.DSN(t)
	cfg.OpenAIBaseURL = upstream.URL
	cfg.RequestTimeout = 10 * time.Second
	s := New(cfg)
	defer s.Close()
	if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"id": "codex", "access_token": migrationTestJWT("codex"), "source_type": "codex", "plan_type": "plus", "quota": 2, "image_quota_unknown": false, "upload_cooldown_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	task := imageTaskState{ID: "recover", OwnerID: "owner", Status: "queued", Mode: "edit", Model: "plus-codex-gpt-image-2", N: 1, Size: "1024x1024", Quality: "auto", PublicBase: "https://images.example.test", Outputs: []imageTaskOutput{{Stage: "queued"}}}
	name := imageTaskDocument(task.OwnerID, task.ID)
	if _, err := s.store.MutateDocument(name, func(d map[string]any) error {
		for k, v := range taskMap(task) {
			d[k] = v
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	payloadDoc := map[string]any{"prompt": "edit", "inputs": []map[string]any{{"Data": raw.Bytes(), "MIME": "image/png", "Name": "input.png"}}}
	if _, err := s.store.MutateDocument("imagepayload-"+strings.TrimPrefix(name, "imagetask-"), func(d map[string]any) error {
		for k, v := range payloadDoc {
			d[k] = v
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A regular file in place of the image directory forces delivery to fail.
	if err := os.MkdirAll(filepath.Dir(cfg.ImageDataDir), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ImageDataDir, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		lease, err := s.store.AcquireLease(context.Background(), "task:"+name, time.Minute)
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		defer lease.Release()
		s.runImageTask(context.Background(), name, lease)
	}
	run()
	doc, _ := s.store.Document(name)
	failed, _ := taskDecode(doc)
	if failed.Status != "error" || failed.Outputs[0].ResultsDocument == "" {
		t.Fatalf("missing durable result: %#v", failed)
	}
	if err := os.Remove(cfg.ImageDataDir); err != nil {
		t.Fatal(err)
	}
	// Expired credentials and zero quota must not prevent delivery of saved bytes.
	original, err := s.store.FindAccount("codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.UpdateAccount(accountToken(original), map[string]any{"access_token": "expired", "quota": 0}); err != nil {
		t.Fatal(err)
	}
	_, err = s.store.MutateDocument(name, func(d map[string]any) error {
		current, _ := taskDecode(d)
		current.Status = "queued"
		current.Outputs[0].Stage = "result_ready"
		for k, v := range taskMap(current) {
			d[k] = v
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run()
	run()
	doc, _ = s.store.Document(name)
	done, _ := taskDecode(doc)
	a, _ := s.store.FindAccount("codex")
	if done.Status != "success" || posts.Load() != 1 || intValue(a["image_generated_count"]) != 1 {
		t.Fatalf("recovery changed generation: task=%#v posts=%d count=%v", done, posts.Load(), a["image_generated_count"])
	}
	public, _ := json.Marshal(imageTaskPublic(done))
	if strings.Contains(string(public), "ResultsDocument") || strings.Contains(string(public), "imageresult-") {
		t.Fatal("private checkpoint exposed")
	}
}

func TestImageModelAccountSourcesAndPlans(t *testing.T) {
	for _, tc := range []struct {
		source, plan, model string
		want                bool
	}{
		{"codex", "plus", "gpt-image-2", false}, {"chatgpt_web", "plus", "codex-gpt-image-2", false},
		{"codex", "free", "codex-gpt-image-2", false}, {"codex", "plus", "codex-gpt-image-2", true},
		{"codex", "team", "plus-codex-gpt-image-2", false}, {"codex", "team", "team-codex-gpt-image-2", true},
	} {
		a := accounts.Account{Fields: map[string]any{"source_type": tc.source, "plan_type": tc.plan}}
		if got := imageModelAccount(a, tc.model); got != tc.want {
			t.Errorf("%+v got %v", tc, got)
		}
	}
}
