package httpapi

import (
	"context"
	"encoding/json"
	"github.com/auucoder/gptgrok2api-go/internal/mediafile"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) recordGeneratedMedia(ctx context.Context, result map[string]string) {
	rawURL := strings.TrimSpace(result["url"])
	if rawURL == "" {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	id := strings.TrimSpace(parsed.Query().Get("id"))
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(parsed.Path), filepath.Ext(parsed.Path))
	}
	filename := ""
	candidate := filepath.Base(parsed.Path)
	if isImageStorageFile(candidate) && candidate != "." {
		if info, e := os.Lstat(filepath.Join(s.cfg.ImageDataDir, candidate)); e == nil && info.Mode().IsRegular() {
			filename = candidate
		}
	}
	if filename == "" {
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp"} {
			if info, e := os.Lstat(filepath.Join(s.cfg.ImageDataDir, id+ext)); e == nil && info.Mode().IsRegular() {
				filename = id + ext
				break
			}
		}
	}
	if filename == "" {
		return
	}
	callID, _ := ctx.Value(monitorCallIDKey{}).(string)
	endpoint, model := "", ""
	if callID != "" {
		if record, ok := s.monitor.detail(callID); ok {
			endpoint = record.Endpoint
			model = record.Model
		}
	}
	meta := map[string]any{"call_id": callID, "endpoint": endpoint, "model": model, "generated_at": time.Now().UTC().Format(time.RFC3339), "source_type": "generated_output", "role": "output"}
	b, _ := json.Marshal(meta)
	_ = mediafile.AtomicWrite(filepath.Join(s.cfg.ImageDataDir, filename+".meta.json"), b, 0o600)
	if err := s.queueRemoteImage(filename); err != nil {
		s.runtimeLog("warning", "remote image enqueue failed; local image retained: %v", err)
	}
	if _, studio := ctx.Value(imageTaskOwnerKey{}).(string); studio {
		if cfg, e := s.store.Config(); e == nil && boolValue(mapValue(cfg["genbox_push"])["enabled"], false) {
			if e := s.queueGenbox(filename, true); e != nil {
				s.runtimeLog("warning", "GenBox enqueue failed; source image retained: %v", e)
			}
		}
	}
}

func mediaMetadata(path string) map[string]any {
	b, err := os.ReadFile(path + ".meta.json")
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
