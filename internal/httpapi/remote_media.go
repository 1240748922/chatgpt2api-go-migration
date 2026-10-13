package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/mediafile"
	"github.com/auucoder/gptgrok2api-go/internal/remotestore"
)

func remoteMediaName(rel string) string {
	return fmt.Sprintf("mediaremote-%x", sha256.Sum256([]byte(rel)))
}
func remoteMediaLease(rel string) string { return "media-transfer:" + remoteMediaName(rel) }
func remoteDestination(d *remotestore.WebDAV) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimRight(d.URL, "/")+"/"+strings.Trim(d.Root, "/"))))
}
func validImageRel(rel string) bool {
	return rel != "" && !strings.ContainsAny(rel, "/\\\x00") && isImageStorageFile(rel) && rel != "." && rel != ".."
}

func (s *Server) webDAV() (*remotestore.WebDAV, string, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return nil, "", err
	}
	d := mapValue(cfg["image_storage"])
	mode := stringValue(d["mode"])
	if !boolValue(d["enabled"], false) || mode == "local" || mode == "" {
		return nil, "local", nil
	}
	if mode != "webdav" && mode != "both" {
		return nil, "", fmt.Errorf("invalid image storage mode")
	}
	return &remotestore.WebDAV{URL: stringValue(d["webdav_url"]), Username: stringValue(d["webdav_username"]), Password: stringValue(d["webdav_password"]), Root: stringValue(d["webdav_root_path"])}, mode, nil
}
func (s *Server) queueRemoteImage(rel string) error {
	if !validImageRel(rel) {
		return fmt.Errorf("invalid image path")
	}
	dav, mode, err := s.webDAV()
	if err != nil || dav == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, remoteMediaLease(rel), 30*time.Second)
	if err != nil {
		return err
	}
	if lease == nil {
		return fmt.Errorf("image storage is busy")
	}
	defer lease.Release()
	path := filepath.Join(s.cfg.ImageDataDir, rel)
	if !regularFileWithin(s.cfg.ImageDataDir, path) {
		return fmt.Errorf("source image unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	_, err = lease.MutateDocument(remoteMediaName(rel), func(d map[string]any) error {
		status := stringValue(d["status"])
		if status == "deleted" || status == "delete_pending" {
			return fmt.Errorf("image deleted")
		}
		if (status == "done" || status == "queued") && stringValue(d["destination"]) == remoteDestination(dav) {
			return nil
		}
		d["path"] = rel
		d["status"] = "queued"
		d["mode"] = mode
		d["destination"] = remoteDestination(dav)
		d["size"] = info.Size()
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		d["created_at"] = info.ModTime().UTC().Format(time.RFC3339Nano)
		d["metadata"] = mediaMetadata(path)
		return nil
	})
	return err
}
func (s *Server) remoteMediaScheduler() {
	if strings.EqualFold(os.Getenv("GO_REGISTRATION_WORKER"), "true") {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var repairDir *os.File
	defer func() {
		if repairDir != nil {
			_ = repairDir.Close()
		}
	}()
	nextRepair := time.Time{}
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
		}
		lease, err := s.store.AcquireLease(s.backgroundCtx, "remote-media-worker", 120*time.Second)
		if err != nil || lease == nil {
			continue
		}
		ctx, cancel := context.WithCancel(s.backgroundCtx)
		lease.KeepAlive(ctx, 120*time.Second, cancel)
		if time.Now().After(nextRepair) {
			s.repairRemoteMedia(ctx, &repairDir)
			nextRepair = time.Now().Add(time.Minute)
		}
		docs, err := s.store.PendingDueDocuments("mediaremote-", []string{"queued", "delete_pending"}, 8)
		if err == nil {
			for _, d := range docs {
				next, _ := time.Parse(time.RFC3339Nano, stringValue(d["next_retry_at"]))
				if time.Now().Before(next) {
					continue
				}
				if ctx.Err() != nil {
					break
				}
				s.transferRemoteImage(ctx, stringValue(d["path"]))
			}
		}
		cancel()
		lease.Release()
	}
}

// Scan a bounded directory page off the request path. Failed queue writes leave
// original files intact, so they can be enqueued later without regenerating.
func (s *Server) repairRemoteMedia(ctx context.Context, dir **os.File) {
	dav, _, err := s.webDAV()
	if err != nil || dav == nil {
		return
	}
	if *dir == nil {
		*dir, err = os.Open(s.cfg.ImageDataDir)
		if err != nil {
			return
		}
	}
	entries, err := (*dir).ReadDir(128)
	if err != nil {
		_ = (*dir).Close()
		*dir = nil
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.Type().IsRegular() || !validImageRel(entry.Name()) {
			continue
		}
		doc, e := s.store.Document(remoteMediaName(entry.Name()))
		if e != nil {
			return
		}
		if stringValue(doc["status"]) == "" {
			_ = s.queueRemoteImage(entry.Name())
		}
	}
}

func (s *Server) remotePublicImageURL(rel string, doc map[string]any) string {
	if !validImageRel(rel) || stringValue(doc["status"]) != "done" {
		return ""
	}
	cfg, err := s.store.Config()
	if err != nil {
		return ""
	}
	settings := mapValue(cfg["image_storage"])
	base, err := url.Parse(strings.TrimSpace(stringValue(settings["public_base_url"])))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return ""
	}
	dav, _, err := s.webDAV()
	if err != nil || dav == nil || remoteDestination(dav) != stringValue(doc["destination"]) {
		return ""
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + rel
	base.RawPath = ""
	return base.String()
}
func (s *Server) transferRemoteImage(parent context.Context, rel string) {
	if !validImageRel(rel) {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, remoteMediaLease(rel), 30*time.Second)
	if err != nil || lease == nil {
		return
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 30*time.Second, cancel)
	doc, err := s.store.Document(remoteMediaName(rel))
	if err != nil {
		return
	}
	dav, _, err := s.webDAV()
	if err != nil || dav == nil {
		return
	}
	failed := func(e error) {
		_, _ = lease.MutateDocument(remoteMediaName(rel), func(d map[string]any) error {
			attempts := min(10, intValue(d["attempts"])+1)
			d["attempts"] = attempts
			d["last_error"] = e.Error()
			d["next_retry_at"] = time.Now().Add(time.Duration(min(300, 5*(1<<attempts))) * time.Second).UTC().Format(time.RFC3339Nano)
			d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			return nil
		})
	}
	if stringValue(doc["destination"]) != remoteDestination(dav) {
		failed(fmt.Errorf("storage destination changed; restore previous settings to access this image"))
		return
	}
	status := stringValue(doc["status"])
	if status == "delete_pending" {
		if err = dav.Delete(ctx, rel); err != nil {
			failed(err)
		} else {
			_, _ = lease.MutateDocument(remoteMediaName(rel), func(d map[string]any) error {
				d["status"] = "deleted"
				d["last_error"] = ""
				d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				return nil
			})
		}
		return
	}
	if status != "queued" {
		return
	}
	path := filepath.Join(s.cfg.ImageDataDir, rel)
	if !regularFileWithin(s.cfg.ImageDataDir, path) {
		failed(fmt.Errorf("source image unavailable"))
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 64<<20 {
		failed(fmt.Errorf("source image exceeds limits"))
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		failed(fmt.Errorf("source image read failed"))
		return
	}
	if err = dav.Put(ctx, rel, raw); err != nil {
		failed(err)
		return
	}
	mode := stringValue(doc["mode"])
	if mode == "webdav" {
		thumb, _, e := mediafile.Transform(ctx, raw, "", true)
		if e == nil {
			e = mediafile.AtomicWrite(s.thumbnailPath(path), thumb, 0640)
		}
		if e != nil {
			failed(fmt.Errorf("thumbnail creation failed"))
			return
		}
	}
	_, err = lease.MutateDocument(remoteMediaName(rel), func(d map[string]any) error {
		if stringValue(d["status"]) != "queued" {
			return fmt.Errorf("image state changed")
		}
		d["status"] = "done"
		d["size"] = len(raw)
		d["sha256"] = fmt.Sprintf("%x", sha256.Sum256(raw))
		d["last_error"] = ""
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		d["metadata"] = mediaMetadata(path)
		return nil
	})
	if err == nil && mode == "webdav" && ctx.Err() == nil {
		_ = os.Remove(path)
		_ = os.Remove(path + ".meta.json")
	}
}
func (s *Server) remoteImageItems() ([]map[string]any, error) {
	docs, err := s.store.AllDocuments("mediaremote-", []string{"done"})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, d := range docs {
		rel := stringValue(d["path"])
		if !validImageRel(rel) || regularFileWithin(s.cfg.ImageDataDir, filepath.Join(s.cfg.ImageDataDir, rel)) {
			continue
		}
		item := map[string]any{"path": rel, "rel": rel, "name": rel, "filename": rel, "type": "image", "size": int64(intValue(d["size"])), "bytes": d["size"], "updated_at": d["created_at"], "url": "/images/" + rel, "thumbnail_url": "/image-thumbnails/" + rel, "storage": "webdav", "local": false, "webdav": true}
		for k, v := range mapValue(d["metadata"]) {
			item[k] = v
		}
		out = append(out, item)
	}
	return out, nil
}
func (s *Server) remoteImageBytes(ctx context.Context, rel string) ([]byte, error) {
	if !validImageRel(rel) {
		return nil, os.ErrNotExist
	}
	doc, err := s.store.Document(remoteMediaName(rel))
	if err != nil {
		return nil, err
	}
	if stringValue(doc["status"]) != "done" {
		return nil, os.ErrNotExist
	}
	dav, _, err := s.webDAV()
	if err != nil {
		return nil, err
	}
	if dav == nil || remoteDestination(dav) != stringValue(doc["destination"]) {
		return nil, fmt.Errorf("image storage configuration changed")
	}
	raw, err := dav.Get(ctx, rel)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != stringValue(doc["sha256"]) {
		return nil, fmt.Errorf("remote image checksum mismatch")
	}
	return raw, nil
}
func (s *Server) serveRemoteImage(w http.ResponseWriter, r *http.Request, rel string) bool {
	if !validImageRel(rel) {
		return false
	}
	doc, err := s.store.Document(remoteMediaName(rel))
	if err != nil {
		writeError(w, 503, "图片索引不可用", "storage_error")
		return true
	}
	if stringValue(doc["status"]) != "done" {
		return false
	}
	path := filepath.Join(s.cfg.ImageDataDir, rel)
	thumb := s.thumbnailPath(path)
	if strings.HasPrefix(r.URL.Path, "/image-thumbnails/") && regularFileWithin(s.cfg.ImageDataDir, thumb) {
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, thumb)
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/images/") {
		if destination := s.remotePublicImageURL(rel, doc); destination != "" {
			http.Redirect(w, r, destination, http.StatusTemporaryRedirect)
			return true
		}
	}
	raw, err := s.remoteImageBytes(r.Context(), rel)
	if err != nil {
		writeError(w, 502, "远端图片读取或校验失败", "image_delivery_failed")
		return true
	}
	w.Header().Set("Content-Type", http.DetectContentType(raw))
	w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
	_, _ = w.Write(raw)
	return true
}

// Busy transfers must not fall through to local deletion.
func (s *Server) tombstoneRemoteImage(ctx context.Context, rel string) (bool, bool) {
	if !validImageRel(rel) {
		return false, false
	}
	lease, err := s.store.AcquireLease(ctx, remoteMediaLease(rel), 30*time.Second)
	if err != nil || lease == nil {
		return true, false
	}
	defer lease.Release()
	doc, err := s.store.Document(remoteMediaName(rel))
	if err != nil {
		return true, false
	}
	path := filepath.Join(s.cfg.ImageDataDir, rel)
	if stringValue(doc["path"]) == "" {
		if !regularFileWithin(s.cfg.ImageDataDir, path) {
			return true, false
		}
		if err = os.Remove(path); err != nil {
			return true, false
		}
		_ = os.Remove(path + ".meta.json")
		_ = os.Remove(s.thumbnailPath(path))
		s.removeImageTags(rel)
		return true, true
	}
	_, err = lease.MutateDocument(remoteMediaName(rel), func(d map[string]any) error {
		if stringValue(d["status"]) == "deleted" {
			return os.ErrNotExist
		}
		d["status"] = "delete_pending"
		d["next_retry_at"] = ""
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		return true, false
	}
	if regularFileWithin(s.cfg.ImageDataDir, path) {
		_ = os.Remove(path)
	}
	_ = os.Remove(path + ".meta.json")
	_ = os.Remove(s.thumbnailPath(path))
	s.removeImageTags(rel)
	return true, true
}
