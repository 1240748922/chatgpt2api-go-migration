package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/remotestore"
)

var r2AccountID = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

func (s *Server) backupConfig() (*remotestore.R2, map[string]any, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return nil, nil, err
	}
	b := mapValue(cfg["backup"])
	if !boolValue(b["enabled"], false) {
		return nil, b, nil
	}
	id := stringValue(b["account_id"])
	if !r2AccountID.MatchString(id) {
		return nil, b, fmt.Errorf("R2 account ID must contain 32 hexadecimal characters")
	}
	d := &remotestore.R2{Endpoint: "https://" + id + ".r2.cloudflarestorage.com", AccessKey: stringValue(b["access_key_id"]), Secret: stringValue(b["secret_access_key"]), Bucket: stringValue(b["bucket"]), Prefix: stringValue(b["prefix"])}
	if d.AccessKey == "" || d.Secret == "" || d.Bucket == "" {
		return nil, b, fmt.Errorf("R2 credentials and bucket are required")
	}
	return d, b, nil
}

func backupName(name string) bool {
	return filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00\r\n") && strings.HasPrefix(name, "backup-") && (strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".zip.enc"))
}
func backupObject(d *remotestore.R2, key string) bool {
	return d != nil && backupName(strings.TrimPrefix(key, d.Key(""))) && key == d.Key(strings.TrimPrefix(key, d.Key("")))
}

func (s *Server) executeBackup(parent context.Context) (result map[string]any, err error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	lease, e := s.store.AcquireLease(ctx, "backup-worker", 60*time.Second)
	if e != nil {
		return nil, e
	}
	if lease == nil {
		return nil, fmt.Errorf("another backup is running")
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 60*time.Second, cancel)
	started := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = lease.MutateDocument("backup-status", func(d map[string]any) error {
		d["running"] = true
		d["started_at"] = started
		d["last_started_at"] = started
		d["last_status"] = "running"
		d["last_error"] = ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = lease.MutateDocument("backup-status", func(d map[string]any) error {
			d["running"] = false
			d["finished_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			d["last_finished_at"] = d["finished_at"]
			if err != nil {
				d["last_error"] = err.Error()
				d["last_status"] = "failed"
			} else {
				d["last_success_at"] = d["finished_at"]
				d["last_result"] = result
				d["last_status"] = "success"
				d["last_object_key"] = result["key"]
			}
			return nil
		})
	}()
	remote, b, err := s.backupConfig()
	if err != nil {
		return nil, err
	}
	encrypt := boolValue(b["encrypt"], false)
	pass := stringValue(b["passphrase"])
	if encrypt && pass == "" {
		return nil, fmt.Errorf("backup encryption passphrase is required")
	}
	include := map[string]bool{}
	for k, v := range mapValue(b["include"]) {
		include[k] = boolValue(v, false)
	}
	path, err := s.createLocalBackup(ctx, include)
	if err != nil {
		return nil, err
	}
	if encrypt {
		plain := path
		defer os.Remove(plain)
		src, e := os.Open(plain)
		if e != nil {
			return nil, e
		}
		defer src.Close()
		tmp, e := os.CreateTemp(s.backupDir(), ".encrypted-*")
		if e != nil {
			return nil, e
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		_ = tmp.Chmod(0600)
		e = remotestore.EncryptBackup(tmp, src, pass)
		if e == nil {
			e = tmp.Sync()
		}
		closeErr := tmp.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return nil, e
		}
		path = plain + ".enc"
		if e = os.Rename(tmp.Name(), path); e != nil {
			return nil, e
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	key := name
	backend := "local"
	if remote != nil {
		backend = "r2"
		key = remote.Key(name)
		file, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		e = remote.Put(ctx, key, file)
		file.Close()
		if e != nil {
			return nil, e
		}
		// The successfully uploaded archive is authoritative. Keep a local copy
		// only on upload failure so an operator can recover it.
		_ = os.Remove(path)
	}
	result = map[string]any{"key": key, "name": name, "size": info.Size(), "backend": backend, "encrypted": encrypt, "created_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if e = s.rotateBackups(ctx, remote, int(imageSeconds(b, "rotation_keep", 10, 0, 10000)/time.Second)); e != nil {
		result["rotation_error"] = e.Error()
	}
	return result, nil
}

func (s *Server) rotateBackups(ctx context.Context, remote *remotestore.R2, keep int) error {
	if keep <= 0 {
		return nil
	}
	if remote != nil {
		items, err := remote.List(ctx)
		if err != nil {
			return err
		}
		filtered := items[:0]
		for _, o := range items {
			if backupObject(remote, o.Key) {
				filtered = append(filtered, o)
			}
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Modified > filtered[j].Modified })
		for _, o := range filtered[min(keep, len(filtered)):] {
			if err = remote.Delete(ctx, o.Key); err != nil {
				return err
			}
		}
		return nil
	}
	entries, err := os.ReadDir(s.backupDir())
	if err != nil {
		return err
	}
	type archive struct {
		name string
		t    time.Time
	}
	items := []archive{}
	for _, e := range entries {
		if backupName(e.Name()) {
			info, err := e.Info()
			if err == nil && info.Mode().IsRegular() {
				items = append(items, archive{e.Name(), info.ModTime()})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
	for _, e := range items[min(keep, len(items)):] {
		if err = os.Remove(filepath.Join(s.backupDir(), e.name)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) backupScheduler() {
	if strings.EqualFold(os.Getenv("GO_REGISTRATION_WORKER"), "true") {
		return
	}
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-timer.C:
		}
		_, b, err := s.backupConfig()
		if err != nil || !boolValue(b["enabled"], false) {
			continue
		}
		status, err := s.store.Document("backup-status")
		if err != nil {
			continue
		}
		last, _ := time.Parse(time.RFC3339Nano, stringValue(status["started_at"]))
		interval := imageSeconds(b, "interval_minutes", 1440, 1, 525600) * 60
		if time.Since(last) < interval {
			continue
		}
		metrics := s.store.LatencyMetrics()
		if intValue(metrics["database_p95_ms"]) > 500 || intValue(metrics["account_write_p95_ms"]) > 1000 {
			continue
		}
		if _, err = s.executeBackup(s.backgroundCtx); err != nil {
			s.runtimeLog("warning", "scheduled backup: %v", err)
		}
	}
}

func (s *Server) testBackup(ctx context.Context) map[string]any {
	remote, _, err := s.backupConfig()
	backend := "local"
	if err == nil && remote != nil {
		backend = "r2"
		key := remote.Key(".probe-" + randomID())
		err = remote.Put(ctx, key, bytes.NewReader([]byte("storage probe")))
		if err == nil {
			err = remote.Delete(ctx, key)
		}
	} else if err == nil {
		err = os.MkdirAll(s.backupDir(), 0700)
		if err == nil {
			var f *os.File
			f, err = os.CreateTemp(s.backupDir(), ".probe-*")
			if err == nil {
				_, err = f.WriteString("storage probe")
				if err == nil {
					err = f.Sync()
				}
				_ = f.Close()
				_ = os.Remove(f.Name())
			}
		}
	}
	out := map[string]any{"ok": err == nil, "backend": backend}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}

func (s *Server) remoteBackupRequest(w http.ResponseWriter, r *http.Request, key, action string) bool {
	remote, _, err := s.backupConfig()
	if err != nil {
		writeError(w, 503, err.Error(), "backup_storage_failed")
		return true
	}
	if remote == nil || !backupObject(remote, key) {
		return false
	}
	if action == "delete" {
		if err = remote.Delete(r.Context(), key); err != nil {
			writeError(w, 502, err.Error(), "backup_storage_failed")
		} else {
			writeJSON(w, 200, map[string]any{"ok": true})
		}
		return true
	}
	if action == "detail" {
		objects, err := remote.List(r.Context())
		if err != nil {
			writeError(w, 502, err.Error(), "backup_storage_failed")
			return true
		}
		for _, o := range objects {
			if o.Key == key {
				writeJSON(w, 200, map[string]any{"item": remoteBackupItem(o)})
				return true
			}
		}
		writeError(w, 404, "backup not found", "not_found")
		return true
	}
	body, err := remote.Get(r.Context(), key)
	if err != nil {
		writeError(w, 502, err.Error(), "backup_storage_failed")
		return true
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(key)+`"`)
	_, _ = io.Copy(w, body)
	return true
}
func remoteBackupItem(o remotestore.Object) map[string]any {
	return map[string]any{"key": o.Key, "name": filepath.Base(o.Key), "size": o.Size, "created_at": o.Modified, "updated_at": o.Modified, "type": "r2", "encrypted": strings.HasSuffix(o.Key, ".enc")}
}
