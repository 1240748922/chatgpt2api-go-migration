package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestPublicBaseDashboardSettingReachesDelivery(t *testing.T) {
	t.Setenv("GO_PUBLIC_BASE_URL", "")
	t.Setenv("CHATGPT2API_BASE_URL", "")
	t.Setenv("GO_TRUST_REQUEST_PUBLIC_BASE", "false")
	t.Setenv("CHATGPT2API_TRUST_REQUEST_HOST", "false")
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	w := adminRequest(s.Handler(), "PATCH", "/api/settings", strings.NewReader(`{"base_url":"https://images.example.test/"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodPost, "https://untrusted.example.test/v1/images/generations", nil)
	base := s.requestPublicBase(r)
	if base != "https://images.example.test" {
		t.Fatalf("configured dashboard URL ignored: got %q", base)
	}
	result, loggedURL, err := s.resolveOpenAIImage(context.Background(), accounts.Account{}, provider.ImageResult{Base64: base64.StdEncoding.EncodeToString(tinyPNG), MIME: "image/png"}, "auto", "url", base)
	if err != nil || !strings.HasPrefix(result["url"], base+"/v1/files/image?id=") || loggedURL != result["url"] {
		t.Fatalf("bad delivery: result=%v log=%q error=%v", result, loggedURL, err)
	}
	result, loggedURL, err = s.resolveOpenAIImage(context.Background(), accounts.Account{}, provider.ImageResult{Base64: base64.StdEncoding.EncodeToString(tinyPNG), MIME: "image/png"}, "auto", "b64_json", base)
	if err != nil || result["b64_json"] != base64.StdEncoding.EncodeToString(tinyPNG) || !strings.HasPrefix(loggedURL, base+"/v1/files/image?id=") {
		t.Fatalf("base64 delivery changed: %v", err)
	}
}

func TestPublicBaseSettingsPrecedenceAndHostIsolation(t *testing.T) {
	t.Setenv("GO_PUBLIC_BASE_URL", "")
	t.Setenv("CHATGPT2API_BASE_URL", "")
	t.Setenv("GO_TRUST_REQUEST_PUBLIC_BASE", "false")
	t.Setenv("CHATGPT2API_TRUST_REQUEST_HOST", "false")
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	r := httptest.NewRequest(http.MethodPost, "https://untrusted.example.test/v1/images/generations", nil)
	r.Header.Set("X-Public-Base-URL", "https://spoof.example.test")
	r.Header.Set("X-Forwarded-Host", "spoof.example.test")
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{"dashboard", map[string]any{"base_url": " https://images.example.test/prefix/ "}, "https://images.example.test/prefix"},
		{"legacy nested", map[string]any{"basic": map[string]any{"base_url": "https://legacy.example.test/"}}, "https://legacy.example.test"},
		{"explicit clear", map[string]any{"base_url": "", "basic": map[string]any{"base_url": "https://legacy.example.test"}}, ""},
		{"unconfigured", map[string]any{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.store.ReplaceConfig(tc.settings); err != nil {
				t.Fatal(err)
			}
			if got := s.requestPublicBase(r); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	for _, invalid := range []string{"images.example.test", "/images", "ftp://images.example.test", "https://name:secret@images.example.test", "https://images.example.test?token=value", "https://images.example.test/#fragment"} {
		if err := s.store.ReplaceConfig(map[string]any{"base_url": invalid}); err != nil {
			t.Fatal(err)
		}
		if got := s.requestPublicBase(r); got != "" {
			t.Fatalf("invalid base accepted: %q", got)
		}
	}
	if err := s.store.ReplaceConfig(map[string]any{"base_url": "https://images.example.test"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT2API_BASE_URL", "https://legacy-env.example.test/")
	if got := s.requestPublicBase(r); got != "https://legacy-env.example.test" {
		t.Fatal(got)
	}
	t.Setenv("GO_PUBLIC_BASE_URL", "https://deployment.example.test/")
	if got := s.requestPublicBase(r); got != "https://deployment.example.test" {
		t.Fatal(got)
	}
	for _, saved := range []string{"https://task.example.test", ""} {
		recovered := r.WithContext(context.WithValue(r.Context(), imageTaskPublicBaseKey{}, saved))
		if got := s.requestPublicBase(recovered); got != saved {
			t.Fatalf("captured task origin changed: %q want %q", got, saved)
		}
	}
}

func TestPublicBaseCapturedWhenCreatingImageTask(t *testing.T) {
	t.Setenv("GO_PUBLIC_BASE_URL", "")
	t.Setenv("CHATGPT2API_BASE_URL", "")
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	if err := s.store.ReplaceConfig(map[string]any{"base_url": "https://images.example.test/"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "https://untrusted.example.test/api/image-tasks", strings.NewReader(`{"client_task_id":"public-url","prompt":"a test image","model":"gpt-image-2"}`))
	r = r.WithContext(context.WithValue(r.Context(), imageTaskOwnerKey{}, "owner"))
	w := httptest.NewRecorder()
	s.imageTasksAPI(w, r)
	if w.Code != 202 {
		t.Fatalf("task creation: %d %s", w.Code, w.Body.String())
	}
	doc, err := s.store.Document(imageTaskDocument("owner", "public-url"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := taskDecode(doc)
	if err != nil || task.PublicBase != "https://images.example.test" {
		t.Fatalf("task origin not captured: %q %v", task.PublicBase, err)
	}
}
