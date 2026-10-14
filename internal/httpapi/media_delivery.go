package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/mediafile"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type imageDeliveryKey struct{}

func (s *Server) resolveOpenAIImage(ctx context.Context, account accounts.Account, image provider.ImageResult, size, format, publicBase string) (map[string]string, string, error) {
	image.DeliveryID, _ = ctx.Value(imageDeliveryKey{}).(string)
	cfg, err := s.store.Config()
	if err != nil {
		return nil, "", err
	}
	if boolValue(cfg["image_upscale_enabled"], false) {
		image.OutputSize = size
		image.OutputEngine = stringValue(cfg["image_upscale_engine"])
		image.ProcessorURL = os.Getenv("GO_IMAGE_PROCESSOR_URL")
	}
	return s.openAIImage.Resolve(ctx, account, image, format, s.cfg.ImageDataDir, publicBase)
}

// Reject invalid names before persisting settings, rather than after generation.
func validateUpscaleSettings(updates map[string]any) error {
	value, exists := updates["image_upscale_engine"]
	if !exists {
		return nil
	}
	engine, ok := value.(string)
	if !ok {
		return fmt.Errorf("image_upscale_engine must be a string")
	}
	normalized, err := mediafile.NormalizeUpscaleEngine(engine)
	if err != nil {
		return err
	}
	updates["image_upscale_engine"] = normalized
	return nil
}

func (s *Server) thumbnailPath(source string) string {
	rel, _ := filepath.Rel(s.cfg.ImageDataDir, source)
	return filepath.Join(s.cfg.ImageDataDir, ".thumbnails", rel+".jpg")
}

func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request, source string) {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, 404, "image not found", "not_found")
		return
	}
	destination := s.thumbnailPath(source)
	if old, e := os.Stat(destination); e != nil || old.ModTime().Before(info.ModTime()) {
		rel, _ := filepath.Rel(s.cfg.ImageDataDir, source)
		lease, e := s.store.AcquireLease(r.Context(), remoteMediaLease(filepath.ToSlash(rel)), 30*time.Second)
		if e != nil {
			writeError(w, 503, "thumbnail storage unavailable", "storage_error")
			return
		}
		if lease == nil {
			w.Header().Set("Retry-After", "1")
			writeError(w, 503, "thumbnail is being prepared", "thumbnail_busy")
			return
		}
		defer lease.Release()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		lease.KeepAlive(ctx, 30*time.Second, cancel)
		if info.Size() > 64<<20 {
			writeError(w, 413, "image exceeds thumbnail limits", "image_too_large")
			return
		}
		raw, e := os.ReadFile(source)
		if e == nil {
			raw, _, e = mediafile.Transform(ctx, raw, "", true)
		}
		if e == nil {
			e = mediafile.AtomicWrite(destination, raw, 0640)
		}
		if e != nil {
			writeError(w, 502, "unable to create thumbnail", "image_thumbnail_failed")
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, destination)
}

func (s *Server) compressImageStorage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, "image-storage-maintenance", 30*time.Second)
	if err != nil || lease == nil {
		writeError(w, 503, "image storage maintenance is busy", "storage_busy")
		return
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 30*time.Second, cancel)
	compressed, checked := 0, 0
	var saved int64
	for _, file := range imageFilesByAge(s.cfg.ImageDataDir) {
		if ctx.Err() != nil {
			break
		}
		if strings.ToLower(filepath.Ext(file.path)) != ".png" || file.size > 64<<20 {
			continue
		}
		changed, bytes, e := s.compressStoredImage(ctx, file.path, file.mtime)
		if e != nil {
			if ctx.Err() != nil {
				break
			}
			writeError(w, 503, e.Error(), "storage_error")
			return
		}
		checked++
		if changed {
			compressed++
			saved += bytes
		}
	}

	writeJSON(w, 200, map[string]any{"ok": ctx.Err() == nil, "checked": checked, "compressed": compressed, "saved_bytes": saved, "partial": ctx.Err() != nil, "message": "PNG 无损压缩，保留原图像素和格式"})
}

func (s *Server) compressStoredImage(ctx context.Context, path string, age time.Time) (bool, int64, error) {
	rel, err := filepath.Rel(s.cfg.ImageDataDir, path)
	if err != nil {
		return false, 0, err
	}
	rel = filepath.ToSlash(rel)
	lease, err := s.store.AcquireLease(ctx, remoteMediaLease(rel), 30*time.Second)
	if err != nil || lease == nil {
		return false, 0, err
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lease.KeepAlive(ctx, 30*time.Second, cancel)
	doc, err := s.store.Document(remoteMediaName(rel))
	if err != nil {
		return false, 0, err
	}
	// A completed remote upload remains immutable; recompressing its local copy
	// would invalidate the checksum and the persistent delivery receipt.
	if status := stringValue(doc["status"]); status == "done" || status == "delete_pending" || status == "deleted" {
		return false, 0, nil
	}
	if !regularFileWithin(s.cfg.ImageDataDir, path) {
		return false, 0, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, 0, fmt.Errorf("read media failed")
	}
	next, err := mediafile.RecompressPNG(ctx, raw)
	if err != nil {
		return false, 0, ctx.Err()
	}
	if len(next) >= len(raw) {
		return false, 0, nil
	}
	if err = ctx.Err(); err != nil {
		return false, 0, err
	}
	if err = mediafile.AtomicWrite(path, next, 0640); err != nil {
		return false, 0, fmt.Errorf("replace media failed")
	}
	_ = os.Chtimes(path, age, age)
	return true, int64(len(raw) - len(next)), nil
}
