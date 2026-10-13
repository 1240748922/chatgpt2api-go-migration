package httpapi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/store"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) backupsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/backups")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		s.listBackups(w, r)
	case path == "/run" && r.Method == http.MethodPost:
		s.runBackup(w, r)
	case path == "/delete" && r.Method == http.MethodPost:
		s.deleteBackup(w, r)
	case path == "/detail" && r.Method == http.MethodGet:
		s.backupDetail(w, r)
	case path == "/download" && r.Method == http.MethodGet:
		s.downloadBackup(w, r)
	default:
		writeError(w, http.StatusNotFound, "backup endpoint not found", "not_found")
	}
}

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, "backups") }

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(s.backupDir())
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !backupName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, backupItem(entry.Name(), info))
	}
	remote, _, err := s.backupConfig()
	if err != nil {
		writeError(w, 503, err.Error(), "backup_storage_failed")
		return
	}
	if remote != nil {
		objects, e := remote.List(r.Context())
		if e != nil {
			writeError(w, 502, e.Error(), "backup_storage_failed")
			return
		}
		for _, o := range objects {
			if backupObject(remote, o.Key) {
				items = append(items, remoteBackupItem(o))
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return fmt.Sprint(items[i]["created_at"]) > fmt.Sprint(items[j]["created_at"]) })
	state, err := s.store.Document("backup-status")
	if err != nil {
		writeError(w, 503, "备份状态读取失败", "storage_error")
		return
	}
	running, err := s.store.ActiveLeases("backup-worker")
	if err != nil {
		writeError(w, 503, "备份运行状态读取失败", "storage_error")
		return
	}
	state["running"] = running > 0
	backend := "local"
	if remote != nil {
		backend = "r2"
	}
	writeJSON(w, 200, map[string]any{"items": items, "state": state, "settings": map[string]any{"backend": backend}})
}

func (s *Server) runBackup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	result, err := s.executeBackup(ctx)
	if err != nil {
		writeError(w, 503, "备份失败："+err.Error(), "backup_failed")
		return
	}
	writeJSON(w, 200, map[string]any{"result": result, "ok": true})
}
func (s *Server) createLocalBackup(ctx context.Context, include map[string]bool) (string, error) {
	if err := os.MkdirAll(s.backupDir(), 0700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("backup-%s-%s.zip", time.Now().UTC().Format("20060102-150405"), randomID())
	file, err := os.CreateTemp(s.backupDir(), ".backup-*")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	defer os.Remove(temp)
	defer file.Close()
	_ = file.Chmod(0600)
	archive := zip.NewWriter(file)
	addJSON := func(name string, value any) error {
		dest, e := archive.Create(name)
		if e != nil {
			return e
		}
		return json.NewEncoder(dest).Encode(value)
	}
	backup := func() error {
		if s.store.Shared() {
			dest, e := archive.Create("database.sql")
			if e != nil {
				return e
			}
			if e = s.store.WritePostgresBackupIncluded(ctx, dest, include); e != nil {
				return e
			}
		} else {
			if store.BackupIncluded(include, "accounts_snapshot") {
				items, e := s.store.AccountList()
				if e != nil {
					return e
				}
				if e = addJSON("data/accounts.json", map[string]any{"items": items}); e != nil {
					return e
				}
			}
			if store.BackupIncluded(include, "auth_keys_snapshot") {
				keys, e := s.store.LoadAuthKeys()
				if e != nil {
					return e
				}
				if e = addJSON("data/auth_keys.json", map[string]any{"items": keys}); e != nil {
					return e
				}
			}
			if store.BackupIncluded(include, "config") {
				cfg, e := s.store.Config()
				if e != nil {
					return e
				}
				if e = addJSON("config.json", cfg); e != nil {
					return e
				}
			}
			names, e := s.store.DocumentNames("")
			if e != nil {
				return e
			}
			for _, name := range names {
				if !store.BackupDocumentIncluded(include, name) {
					continue
				}
				doc, e := s.store.Document(name)
				if e != nil {
					return e
				}
				if e = addJSON("data/jobs/"+name+".json", doc); e != nil {
					return e
				}
			}
			if store.BackupIncluded(include, "logs") {
				if e = addBackupFile(archive, filepath.Join(s.cfg.DataDir, "logs.jsonl"), "data/logs.jsonl"); e != nil {
					return e
				}
			}
		}
		if store.BackupIncluded(include, "accounts_snapshot") {
			if e := addBackupFile(archive, s.cfg.OAuthPath, "data/oauth_accounts.json.enc"); e != nil {
				return e
			}
		}
		if store.BackupIncluded(include, "image_tasks") {
			if e := addBackupFile(archive, s.cfg.QueuePath, "data/tasks.json"); e != nil {
				return e
			}
		}
		if store.BackupIncluded(include, "register") {
			if e := addBackupTree(ctx, archive, s.replenishmentRoot(), "data/registration"); e != nil {
				return e
			}
		}
		if store.BackupIncluded(include, "images") {
			if e := addBackupTree(ctx, archive, s.cfg.ImageDataDir, "data/images"); e != nil {
				return e
			}
			remote, e := s.remoteImageItems()
			if e != nil {
				return e
			}
			for _, item := range remote {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				rel := stringValue(item["path"])
				raw, e := s.remoteImageBytes(ctx, rel)
				if e != nil {
					return e
				}
				dest, e := archive.Create("data/images/" + rel)
				if e != nil {
					return e
				}
				if _, e = dest.Write(raw); e != nil {
					return e
				}
			}
		}
		return addJSON("manifest.json", map[string]any{"format": "go-migration-backup-v1", "shared": s.store.Shared(), "created_at": time.Now().UTC().Format(time.RFC3339), "media_included": store.BackupIncluded(include, "images"), "include": include, "restore_requires": "empty initialized Go database; stop all application workers before restore", "oauth_cipher_key_included": false})
	}
	err = backup()
	closeErr := archive.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr = file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = ctx.Err()
	}
	path := filepath.Join(s.backupDir(), name)
	if err == nil {
		err = os.Rename(temp, path)
	}
	return path, err
}

func addBackupTree(ctx context.Context, archive *zip.Writer, root, prefix string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return addBackupFile(archive, path, prefix+"/"+filepath.ToSlash(rel))
	})
}

func addBackupFile(archive *zip.Writer, path, name string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer, err := archive.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}

func backupItem(name string, info os.FileInfo) map[string]any {
	item := map[string]any{"key": name, "name": name, "type": "local", "content_type": backupContentType(name), "encrypted": strings.HasSuffix(name, ".enc")}
	if info != nil {
		item["size"] = info.Size()
		item["created_at"] = info.ModTime().UTC().Format(time.RFC3339Nano)
		item["updated_at"] = info.ModTime().UTC().Format(time.RFC3339Nano)
	}
	return item
}

func (s *Server) backupPath(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" || filepath.Base(key) != key || !backupName(key) {
		return "", false
	}
	path := filepath.Join(s.backupDir(), key)
	if !isWithin(s.backupDir(), path) {
		return "", false
	}
	if !regularFileWithin(s.backupDir(), path) {
		return "", false
	}
	return path, true
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Key string `json:"key"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if s.remoteBackupRequest(w, r, request.Key, "delete") {
		return
	}
	path, ok := s.backupPath(request.Key)
	if !ok || os.Remove(path) != nil {
		writeError(w, http.StatusBadRequest, "backup not found", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) backupDetail(w http.ResponseWriter, r *http.Request) {
	if s.remoteBackupRequest(w, r, r.URL.Query().Get("key"), "detail") {
		return
	}
	path, ok := s.backupPath(r.URL.Query().Get("key"))
	if !ok {
		writeError(w, http.StatusBadRequest, "backup not found", "invalid_request_error")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": backupItem(info.Name(), info)})
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	if s.remoteBackupRequest(w, r, r.URL.Query().Get("key"), "download") {
		return
	}
	path, ok := s.backupPath(r.URL.Query().Get("key"))
	if !ok {
		writeError(w, http.StatusBadRequest, "backup not found", "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", backupContentType(path))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

func backupContentType(name string) string {
	if strings.HasSuffix(name, ".enc") {
		return "application/octet-stream"
	}
	return "application/zip"
}
