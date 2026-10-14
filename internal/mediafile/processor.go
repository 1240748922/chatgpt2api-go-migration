package mediafile

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NormalizeUpscaleEngine accepts names previously saved by the dashboard as
// well as the processor's canonical names. The built-in engine needs no service.
func NormalizeUpscaleEngine(engine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "", "go", "go_lanczos3":
		return "go_lanczos3", nil
	case "sharp", "sharp_lanczos3":
		return "sharp_lanczos3", nil
	case "pillow", "pillow_lanczos":
		return "pillow_lanczos", nil
	case "fsrcnn", "fsrcnn_x2":
		return "fsrcnn_x2", nil
	default:
		return "", fmt.Errorf("unsupported image upscale engine; choose go_lanczos3, sharp_lanczos3, pillow_lanczos or fsrcnn_x2")
	}
}

// The optional CPU service preserves the original Sharp/Pillow/FSRCNN engines.
func Upscale(ctx context.Context, raw []byte, size, engine, processor string) ([]byte, string, error) {
	if size == "" || size == "auto" {
		return raw, "", nil
	}
	w, h, err := Dimensions(size)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > 64<<20 {
		return nil, "", fmt.Errorf("image exceeds 64 MiB")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 32<<20 {
		return nil, "", fmt.Errorf("image pixel count exceeds limits")
	}
	if cfg.Width >= w && cfg.Height >= h {
		return raw, "", nil
	}
	engine, err = NormalizeUpscaleEngine(engine)
	if err != nil {
		return nil, "", err
	}
	if engine == "go_lanczos3" {
		return Transform(ctx, raw, size, false)
	}
	u, err := url.Parse(processor)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", fmt.Errorf("image processor unavailable; configure GO_IMAGE_PROCESSOR_URL or select go_lanczos3")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/upscale"
	q := u.Query()
	q.Set("size", size)
	q.Set("engine", engine)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Timeout: 180 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("image processor request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("image processor returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > 64<<20 {
		return nil, "", fmt.Errorf("processed image exceeds 64 MiB")
	}
	out, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || out.Width != w || out.Height != h {
		return nil, "", fmt.Errorf("invalid image processor output")
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}[format]
	if mime == "" {
		return nil, "", fmt.Errorf("unsupported processor output")
	}
	return data, mime, nil
}
