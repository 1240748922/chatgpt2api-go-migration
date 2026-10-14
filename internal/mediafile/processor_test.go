package mediafile

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func processorTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUpscaleDashboardEngineAliases(t *testing.T) {
	input, output := processorTestPNG(t, 8, 5), processorTestPNG(t, 16, 10)
	for _, tc := range []struct{ engine, remote string }{
		{"go", ""}, {"go_lanczos3", ""}, {"", ""},
		{"sharp", "sharp_lanczos3"}, {"sharp_lanczos3", "sharp_lanczos3"},
		{"pillow", "pillow_lanczos"}, {"pillow_lanczos", "pillow_lanczos"},
		{"fsrcnn", "fsrcnn_x2"}, {"fsrcnn_x2", "fsrcnn_x2"},
		{"  Sharp  ", "sharp_lanczos3"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			calls := 0
			processor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/upscale" || r.URL.Query().Get("engine") != tc.remote || r.URL.Query().Get("size") != "16x10" {
					t.Errorf("unexpected processor request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(output)
			}))
			defer processor.Close()
			got, mime, err := Upscale(context.Background(), input, "16x10", tc.engine, processor.URL)
			if err != nil {
				t.Fatalf("engine %q failed: %v", tc.engine, err)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(got))
			if err != nil || cfg.Width != 16 || cfg.Height != 10 || mime != "image/png" {
				t.Fatalf("incorrect output: %#v %s %v", cfg, mime, err)
			}
			expected := 1
			if tc.remote == "" {
				expected = 0
			}
			if calls != expected {
				t.Fatalf("processor calls %d; expected %d", calls, expected)
			}
		})
	}
}

func TestUpscaleGoWorksWithoutProcessorAndPreservesNoOp(t *testing.T) {
	raw := processorTestPNG(t, 8, 5)
	for _, engine := range []string{"", "go", "go_lanczos3"} {
		if _, _, err := Upscale(context.Background(), raw, "16x10", engine, ""); err != nil {
			t.Fatalf("%q requires a processor: %v", engine, err)
		}
	}
	for _, size := range []string{"", "auto", "8x5", "4x3"} {
		got, _, err := Upscale(context.Background(), raw, size, "sharp", "")
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("no-op %q changed source: %v", size, err)
		}
	}
}

func TestUpscaleRejectsInvalidEngineAndUnavailableProcessor(t *testing.T) {
	raw := processorTestPNG(t, 8, 5)
	_, _, err := Upscale(context.Background(), raw, "16x10", "unknown", "")
	if err == nil || !strings.Contains(err.Error(), "unsupported image upscale engine") {
		t.Fatalf("invalid engine accepted: %v", err)
	}
	_, _, err = Upscale(context.Background(), raw, "16x10", "sharp", "")
	if err == nil || !strings.Contains(err.Error(), "image processor unavailable") {
		t.Fatalf("missing processor misclassified: %v", err)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer failing.Close()
	_, _, err = Upscale(context.Background(), raw, "16x10", "pillow", failing.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("processor failure hidden: %v", err)
	}
}
