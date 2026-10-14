package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestUpscaleSettingsSaveAndDeliver(t *testing.T) {
	var resized bytes.Buffer
	if err := png.Encode(&resized, image.NewNRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, canonical string }{
		{"", "go_lanczos3"}, {"go", "go_lanczos3"}, {"go_lanczos3", "go_lanczos3"},
		{"sharp", "sharp_lanczos3"}, {"sharp_lanczos3", "sharp_lanczos3"},
		{"pillow", "pillow_lanczos"}, {"pillow_lanczos", "pillow_lanczos"},
		{"fsrcnn", "fsrcnn_x2"}, {"fsrcnn_x2", "fsrcnn_x2"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			calls := 0
			processor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Query().Get("engine") != tc.canonical {
					t.Errorf("incorrect engine: %s", r.URL)
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(resized.Bytes())
			}))
			defer processor.Close()
			t.Setenv("GO_IMAGE_PROCESSOR_URL", processor.URL)
			s := New(adminTestConfig(t.TempDir()))
			defer s.Close()
			body, _ := json.Marshal(map[string]any{"image_upscale_enabled": true, "image_upscale_engine": tc.input, "image_account_concurrency": 7})
			response := adminRequest(s.Handler(), "PATCH", "/api/settings", bytes.NewReader(body))
			if response.Code != 200 {
				t.Fatalf("settings save: %d %s", response.Code, response.Body.String())
			}
			fields, err := s.store.Config()
			if err != nil || fields["image_upscale_engine"] != tc.canonical || intValue(fields["image_account_concurrency"]) != 7 {
				t.Fatalf("settings not normalized: %v %v", fields, err)
			}
			ctx, err := s.imageOptionsContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := s.resolveOpenAIImage(ctx, accounts.Account{}, provider.ImageResult{Base64: base64.StdEncoding.EncodeToString(tinyPNG), MIME: "image/png"}, "16x16", "b64_json", "")
			if err != nil {
				t.Fatalf("saved engine fails delivery: %v", err)
			}
			raw, err := base64.StdEncoding.DecodeString(result["b64_json"])
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
			if err != nil || cfg.Width != 16 || cfg.Height != 16 {
				t.Fatalf("invalid delivery: %v %v", cfg, err)
			}
			files, err := filepath.Glob(filepath.Join(s.cfg.ImageDataDir, "*.png"))
			if err != nil || len(files) != 1 {
				t.Fatalf("image not stored: %v %v", files, err)
			}
			stored, err := os.ReadFile(files[0])
			if err != nil || !bytes.Equal(stored, raw) {
				t.Fatalf("stored image differs: %v", err)
			}
			expected := 1
			if tc.canonical == "go_lanczos3" {
				expected = 0
			}
			if calls != expected {
				t.Fatalf("remote calls: %d want %d", calls, expected)
			}
		})
	}
}

func TestUpscaleLegacySavedEngineAndDefaultDeliverLocally(t *testing.T) {
	t.Setenv("GO_IMAGE_PROCESSOR_URL", "")
	for _, engine := range []string{"", "go", "go_lanczos3"} {
		s := New(adminTestConfig(t.TempDir()))
		config := map[string]any{"image_upscale_enabled": true}
		if engine != "" {
			config["image_upscale_engine"] = engine
		}
		if err := s.store.ReplaceConfig(config); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.resolveOpenAIImage(context.Background(), accounts.Account{}, provider.ImageResult{Base64: base64.StdEncoding.EncodeToString(tinyPNG), MIME: "image/png"}, "16x16", "url", "")
		s.Close()
		if err != nil {
			t.Fatalf("previously saved engine %q fails: %v", engine, err)
		}
	}
}

func TestUpscaleInvalidSettingsRejectedWithoutPartialChanges(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	defer s.Close()
	if err := s.store.ReplaceConfig(map[string]any{"image_upscale_enabled": true, "image_upscale_engine": "go_lanczos3", "image_account_concurrency": 7}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{"unknown", 42, nil, []string{"go"}} {
		body, _ := json.Marshal(map[string]any{"image_upscale_engine": value, "image_account_concurrency": 3})
		response := adminRequest(s.Handler(), "PATCH", "/api/settings", bytes.NewReader(body))
		if response.Code != 400 || !strings.Contains(response.Body.String(), "invalid_image_upscale_engine") {
			t.Fatalf("invalid settings accepted: %d %s", response.Code, response.Body.String())
		}
		cfg, _ := s.store.Config()
		if cfg["image_upscale_engine"] != "go_lanczos3" || intValue(cfg["image_account_concurrency"]) != 7 {
			t.Fatalf("partial settings write: %v", cfg)
		}
	}
	if err := s.store.ReplaceConfig(map[string]any{"image_upscale_enabled": true, "image_upscale_engine": "unknown"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.imageOptionsContext(context.Background()); err == nil {
		t.Fatal("invalid stored engine not caught before generation")
	}
	if _, err := s.store.UpdateConfig("image_upscale_enabled", false); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.imageOptionsContext(context.Background())
	if err != nil {
		t.Fatalf("disabled upscale blocks generation: %v", err)
	}
	result, _, err := s.resolveOpenAIImage(ctx, accounts.Account{}, provider.ImageResult{Base64: base64.StdEncoding.EncodeToString(tinyPNG), MIME: "image/png"}, "16x16", "b64_json", "")
	if err != nil || result["b64_json"] != base64.StdEncoding.EncodeToString(tinyPNG) {
		t.Fatalf("disabled upscale did not preserve original: %v", err)
	}
}
