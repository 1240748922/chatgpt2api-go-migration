package httpapi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) adminImages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/images")
	switch {
	case path == "" || path == "/":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		s.listAdminImages(w, r)
	case path == "/tags" && r.Method == http.MethodGet:
		s.listImageTags(w)
	case path == "/tags" && r.Method == http.MethodPost:
		s.updateImageTags(w, r)
	case strings.HasPrefix(path, "/tags/") && r.Method == http.MethodDelete:
		tag := strings.TrimPrefix(path, "/tags/")
		s.deleteImageTag(w, tag)
	case path == "/storage" && r.Method == http.MethodGet:
		s.imageStorage(w)
	case path == "/genbox-push" && r.Method == http.MethodPost:
		s.genboxPushAPI(w, r)
	case path == "/storage/compress" && r.Method == http.MethodPost:
		s.compressImageStorage(w, r)
	case path == "/storage/cleanup-to-target" && r.Method == http.MethodPost:
		s.imageStorageCleanup(w, r)
	case path == "/delete" && r.Method == http.MethodPost:
		s.deleteAdminImages(w, r)
	case path == "/download" && r.Method == http.MethodPost:
		s.downloadAdminImages(w, r)
	case strings.HasPrefix(path, "/download/") && r.Method == http.MethodGet:
		s.downloadSingleImage(w, r, strings.TrimPrefix(path, "/download/"))
	default:
		writeError(w, http.StatusNotFound, "image endpoint not found", "not_found")
	}
}

func (s *Server) listAdminImages(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0)
	items = append(items, listMediaItems(s.cfg.ImageDataDir, "image", "")...)
	items = append(items, listMediaItems(s.cfg.VideoDataDir, "video", "video/")...)
	remote, remoteErr := s.remoteImageItems()
	if remoteErr != nil {
		writeError(w, 503, "图片索引读取失败", "storage_error")
		return
	}
	items = append(items, remote...)
	if err := s.decorateGenbox(items); err != nil {
		writeError(w, 503, "图片推送状态读取失败", "storage_error")
		return
	}
	tags, err := s.loadImageTagsChecked()
	if err != nil {
		writeError(w, 503, "读取图片标签失败", "storage_error")
		return
	}
	query := r.URL.Query()
	mediaType := strings.ToLower(strings.TrimSpace(query.Get("media_type")))
	tag := strings.TrimSpace(query.Get("tag"))
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		itemTags := stringList(tags[stringValue(item["path"])])
		item["tags"] = itemTags
		if mediaType != "" && mediaType != "all" && stringValue(item["type"]) != mediaType {
			continue
		}
		if tag != "" && tag != "all" && !containsString(itemTags, tag) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(fmt.Sprint(item["path"], " ", item["name"], " ", item["updated_at"])), search) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return stringValue(filtered[i]["updated_at"]) > stringValue(filtered[j]["updated_at"])
	})
	total := len(filtered)
	offset := nonNegativeInt(query.Get("offset"), 0)
	limit := nonNegativeInt(query.Get("limit"), 0)
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	page := filtered[offset:end]
	counts := map[string]int{"all": total, "image": 0, "video": 0}
	for _, item := range filtered {
		counts[stringValue(item["type"])]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "groups": []any{}, "total": total, "total_size": mediaItemsSize(filtered), "counts": counts, "limit": limit, "offset": offset, "has_more": end < total})
}

func listMediaItems(root, kind, prefix string) []map[string]any {
	entries, _ := os.ReadDir(root)
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || strings.HasSuffix(entry.Name(), ".meta.json") || !isMediaStorageFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if !isWithin(root, path) {
			continue
		}
		rel := prefix + filepath.ToSlash(entry.Name())
		value := map[string]any{"path": rel, "rel": rel, "name": entry.Name(), "filename": entry.Name(), "type": kind, "size": info.Size(), "bytes": info.Size(), "updated_at": info.ModTime().UTC(), "url": "/images/" + rel, "thumbnail_url": "/image-thumbnails/" + rel}
		if meta := mediaMetadata(path); meta != nil {
			for key, item := range meta {
				value[key] = item
			}
		} else if kind == "image" {
			value["source_type"] = "legacy_output"
			value["role"] = "output"
			value["generated_at"] = info.ModTime().UTC().Format(time.RFC3339)
		}
		items = append(items, value)
	}
	return items
}

func mediaItemsSize(items []map[string]any) int64 {
	var total int64
	for _, item := range items {
		if value, ok := item["size"].(int64); ok {
			total += value
		}
	}
	return total
}

func (s *Server) publicImage(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/images/")
	root := s.cfg.ImageDataDir
	if strings.HasPrefix(r.URL.Path, "/image-thumbnails/") {
		path = strings.TrimPrefix(r.URL.Path, "/image-thumbnails/")
	}
	if path == "" || strings.Contains(path, "\\") || !isImageStorageFile(path) {
		writeError(w, http.StatusBadRequest, "invalid image path", "invalid_request_error")
		return
	}
	path, err := filepath.Rel(root, filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	filePath := filepath.Join(root, path)
	if !regularFileWithin(root, filePath) {
		if s.serveRemoteImage(w, r, filepath.ToSlash(path)) {
			return
		}
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/image-thumbnails/") {
		s.serveThumbnail(w, r, filePath)
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(filePath)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, filePath)
}

func (s *Server) deleteAdminImages(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Paths       []string `json:"paths"`
		AllMatching bool     `json:"all_matching"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	paths := request.Paths
	if request.AllMatching {
		paths = nil
		remote, e := s.remoteImageItems()
		if e != nil {
			writeError(w, 503, "图片索引读取失败", "storage_error")
			return
		}
		for _, item := range append(listMediaItems(s.cfg.ImageDataDir, "image", ""), remote...) {
			paths = append(paths, stringValue(item["path"]))
		}
	}
	removed := 0
	for _, item := range paths {
		if s.removeMediaPath(item) {
			removed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) removeMediaPath(value string) bool {
	root := s.cfg.ImageDataDir
	tagPath := filepath.ToSlash(strings.TrimSpace(value))
	value = filepath.ToSlash(strings.TrimSpace(value))
	if strings.HasPrefix(value, "video/") {
		root = s.cfg.VideoDataDir
		value = strings.TrimPrefix(value, "video/")
	}
	path := filepath.Join(root, filepath.FromSlash(value))
	if s.store != nil && root == s.cfg.ImageDataDir && validImageRel(value) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if managed, removed := s.tombstoneRemoteImage(ctx, value); managed {
			return removed
		}
	}
	if !isMediaStorageFile(path) || !regularFileWithin(root, path) {
		return false
	}
	if os.Remove(path) == nil {
		_ = os.Remove(path + ".meta.json")
		if root == s.cfg.ImageDataDir {
			_ = os.Remove(s.thumbnailPath(path))
		}
		s.removeImageTags(tagPath)
		return true
	}
	return false
}

func (s *Server) removeImageTags(path string) {
	if s.store == nil {
		return
	}
	_, _ = s.mutateImageTags(func(all map[string]any) error { delete(all, path); return nil })
}

func (s *Server) downloadAdminImages(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="images.zip"`)
	if err := s.writeMediaZipContext(r.Context(), w, request.Paths); err != nil {
		return
	}
}

func (s *Server) downloadSingleImage(w http.ResponseWriter, r *http.Request, path string) {
	root, clean := s.mediaPath(path)
	if root == "" {
		if validImageRel(path) {
			w.Header().Set("Content-Disposition", `attachment; filename="`+path+`"`)
			if s.serveRemoteImage(w, r, path) {
				return
			}
		}
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(clean)+`"`)
	http.ServeFile(w, r, filepath.Join(root, clean))
}

func (s *Server) writeMediaZip(w io.Writer, paths []string) error {
	return s.writeMediaZipContext(context.Background(), w, paths)
}
func (s *Server) writeMediaZipContext(ctx context.Context, w io.Writer, paths []string) error {
	archive := zip.NewWriter(w)
	defer archive.Close()
	used := map[string]bool{}
	added := 0
	for _, item := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		root, clean := s.mediaPath(item)
		var raw []byte
		var err error
		if root == "" {
			if !validImageRel(item) {
				continue
			}
			clean = item
			raw, err = s.remoteImageBytes(ctx, item)
		} else {
			raw, err = os.ReadFile(filepath.Join(root, clean))
		}
		if err != nil {
			continue
		}
		name := filepath.Base(clean)
		if used[name] {
			name = fmt.Sprintf("%d_%s", added+1, name)
		}
		used[name] = true
		writer, err := archive.Create(name)
		if err != nil {
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		added++
	}
	if added == 0 {
		return fmt.Errorf("no media found")
	}
	return nil
}

func (s *Server) mediaPath(value string) (string, string) {
	value = filepath.ToSlash(strings.Trim(strings.TrimSpace(value), "/"))
	root := s.cfg.ImageDataDir
	if strings.HasPrefix(value, "video/") {
		root = s.cfg.VideoDataDir
		value = strings.TrimPrefix(value, "video/")
	}
	if value == "" || strings.Contains(value, "\\") {
		return "", ""
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	path := filepath.Join(root, clean)
	if !isWithin(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return "", ""
	}
	if !regularFileWithin(root, path) || !isMediaStorageFile(path) {
		return "", ""
	}
	return root, clean
}

func regularFileWithin(root, path string) bool {
	if !isWithin(root, path) || filepath.Clean(root) == filepath.Clean(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	realPath, err := filepath.EvalSymlinks(path)
	return err == nil && isWithin(realRoot, realPath)
}

func (s *Server) tagsPath() string { return filepath.Join(s.cfg.DataDir, "image_tags.json") }

func (s *Server) loadImageTagsChecked() (map[string]any, error) {
	d, err := s.store.Document("image-tags")
	if err != nil {
		return nil, err
	}
	if d["items"] != nil {
		return mapValue(d["items"]), nil
	}
	// Only seed the standalone JSON backend; shared storage never trusts stale local files.
	if !s.store.Shared() {
		raw, e := os.ReadFile(s.tagsPath())
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		var tags map[string]any
		if len(raw) > 0 && json.Unmarshal(raw, &tags) != nil {
			return nil, fmt.Errorf("invalid image tags")
		}
		if tags != nil {
			return tags, nil
		}
	}
	return map[string]any{}, nil
}
func (s *Server) loadImageTags() map[string]any { tags, _ := s.loadImageTagsChecked(); return tags }
func (s *Server) mutateImageTags(fn func(map[string]any) error) (map[string]any, error) {
	seed, err := s.loadImageTagsChecked()
	if err != nil {
		return nil, err
	}
	return s.store.MutateDocument("image-tags", func(d map[string]any) error {
		tags := mapValue(d["items"])
		if d["items"] == nil {
			tags = seed
		}
		if err := fn(tags); err != nil {
			return err
		}
		d["items"] = tags
		return nil
	})
}
func (s *Server) saveImageTags(tags map[string]any) error {
	_, err := s.mutateImageTags(func(all map[string]any) error {
		for key := range all {
			delete(all, key)
		}
		for key, value := range tags {
			all[key] = value
		}
		return nil
	})
	return err
}

func (s *Server) listImageTags(w http.ResponseWriter) {
	all := map[string]bool{}
	tags, err := s.loadImageTagsChecked()
	if err != nil {
		writeError(w, 503, "读取图片标签失败", "storage_error")
		return
	}
	for _, value := range tags {
		for _, tag := range stringList(value) {
			all[tag] = true
		}
	}
	result := make([]string, 0, len(all))
	for tag := range all {
		result = append(result, tag)
	}
	sort.Strings(result)
	writeJSON(w, http.StatusOK, map[string]any{"tags": result})
}

func (s *Server) updateImageTags(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if !decodeJSON(w, r, &request) || strings.TrimSpace(request.Path) == "" {
		return
	}
	path := filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(request.Path), "/"))
	tags := map[string]any{}
	for _, tag := range request.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags[tag] = true
		}
	}
	values := make([]string, 0, len(tags))
	for tag := range tags {
		values = append(values, tag)
	}
	sort.Strings(values)
	_, err := s.mutateImageTags(func(all map[string]any) error { all[path] = values; return nil })
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tags": values})
}

func (s *Server) deleteImageTag(w http.ResponseWriter, tag string) {
	tag = strings.TrimSpace(tag)
	removed := 0
	_, err := s.mutateImageTags(func(all map[string]any) error {
		for path, value := range all {
			next := []string{}
			changed := false
			for _, item := range stringList(value) {
				if item == tag {
					changed = true
				} else {
					next = append(next, item)
				}
			}
			if changed {
				removed++
				all[path] = next
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, 503, "更新图片标签失败", "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_from": removed})
}

func (s *Server) imageStorage(w http.ResponseWriter) {
	images := mediaStats(s.cfg.ImageDataDir)
	total, used, free := diskUsage(s.cfg.ImageDataDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"disk_total_mb":    total / (1024 * 1024),
		"disk_used_mb":     used / (1024 * 1024),
		"disk_free_mb":     free / (1024 * 1024),
		"image_count":      images["count"],
		"image_size_mb":    images["size_bytes"].(int64) / (1024 * 1024),
		"image_size_bytes": images["size_bytes"],
		"images":           images,
		"videos":           mediaStats(s.cfg.VideoDataDir),
	})
}

func (s *Server) imageStorageCleanup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, "image-storage-maintenance", 30*time.Second)
	if err != nil || lease == nil {
		writeError(w, 503, "image storage maintenance is busy", "storage_busy")
		return
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 30*time.Second, cancel)
	target := positiveInt(r.URL.Query().Get("target_free_mb"), 500)
	dryRun := strings.EqualFold(r.URL.Query().Get("dry_run"), "true") || r.URL.Query().Get("dry_run") == "1"
	totalBytes, _, freeBytes := diskUsage(s.cfg.ImageDataDir)
	if totalBytes > 0 && uint64(target) > totalBytes/(1024*1024) {
		writeError(w, http.StatusBadRequest, "target free space exceeds disk capacity; enter the value in MB", "invalid_request_error")
		return
	}
	currentFree := freeBytes / (1024 * 1024)
	removed, freed := 0, int64(0)
	files := imageFilesByAge(s.cfg.ImageDataDir)
	removedTags := []string{}
	for _, file := range files {
		if ctx.Err() != nil {
			break
		}
		if currentFree+uint64(freed/(1024*1024)) >= uint64(target) {
			break
		}
		if !dryRun {
			if err := os.Remove(file.path); err != nil {
				continue
			}
			_ = os.Remove(file.path + ".meta.json")
			_ = os.Remove(s.thumbnailPath(file.path))
			removedTags = append(removedTags, file.rel)
		}
		freed += file.size
		removed++
	}
	if !dryRun {
		if len(removedTags) > 0 {
			if _, err := s.mutateImageTags(func(tags map[string]any) error {
				for _, key := range removedTags {
					delete(tags, key)
				}
				return nil
			}); err != nil {
				writeError(w, 503, "media removed but tag cleanup failed", "storage_error")
				return
			}
		}
		cleanupEmptyDirs(s.cfg.ImageDataDir)
		_, _, freeBytes = diskUsage(s.cfg.ImageDataDir)
		currentFree = freeBytes / (1024 * 1024)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "target_free_mb": target, "dry_run": dryRun,
		"removed": removed, "freed_mb": freed / (1024 * 1024),
		"current_free_mb": currentFree, "done": currentFree >= uint64(target) || (dryRun && currentFree+uint64(freed/(1024*1024)) >= uint64(target)),
	})
}

type imageStorageFile struct {
	path  string
	rel   string
	size  int64
	mtime time.Time
}

func imageFilesByAge(root string) []imageStorageFile {
	files := []imageStorageFile{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if info != nil && info.IsDir() && strings.HasPrefix(info.Name(), ".") && path != root {
			return filepath.SkipDir
		}
		if err != nil || info == nil || !info.Mode().IsRegular() || strings.HasSuffix(info.Name(), ".meta.json") || !isImageStorageFile(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			files = append(files, imageStorageFile{path: path, rel: filepath.ToSlash(rel), size: info.Size(), mtime: info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	return files
}

func isImageStorageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	default:
		return false
	}
}

func cleanupEmptyDirs(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && info.IsDir() && path != root {
			entries, readErr := os.ReadDir(path)
			if readErr == nil && len(entries) == 0 {
				_ = os.Remove(path)
			}
		}
		return nil
	})
}

func stringList(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case []string:
		return append(result, typed...)
	case []any:
		for _, item := range typed {
			if value := strings.TrimSpace(fmt.Sprint(item)); value != "" {
				result = append(result, value)
			}
		}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func nonNegativeInt(value string, fallback int) int {
	var number int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &number); err != nil || number < 0 {
		return fallback
	}
	return number
}

func isMediaStorageFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return isImageStorageFile(name) || ext == ".mp4" || ext == ".webm"
}
