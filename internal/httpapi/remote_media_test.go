package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func remoteMediaTestServer(t *testing.T, endpoint, mode string) *Server {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = root
	cfg.ImageDataDir = filepath.Join(root, "images")
	cfg.VideoDataDir = filepath.Join(root, "videos")
	_ = os.MkdirAll(cfg.ImageDataDir, 0700)
	repo := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repo.ReplaceConfig(map[string]any{"image_storage": map[string]any{"enabled": true, "mode": mode, "webdav_url": endpoint, "webdav_root_path": "images"}}); err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: cfg, store: repo}
}

func TestRemoteMediaDeliveryDeleteAndRetention(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	release := make(chan struct{})
	entered := make(chan struct{})
	block := false
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && block {
			close(entered)
			<-release
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(201)
		case "PUT":
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case "GET":
			w.Write(objects[r.URL.Path])
		case "DELETE":
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		}
	}))
	defer dav.Close()
	s := remoteMediaTestServer(t, dav.URL, "webdav")
	ctx := context.Background()
	path := filepath.Join(s.cfg.ImageDataDir, "first.png")
	_ = os.WriteFile(path, tinyPNG, 0600)
	if err := s.queueRemoteImage("first.png"); err != nil {
		t.Fatal(err)
	}
	s.transferRemoteImage(ctx, "first.png")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("full image not removed after durable transfer")
	}
	if !regularFileWithin(s.cfg.ImageDataDir, s.thumbnailPath(path)) {
		t.Fatal("thumbnail missing")
	}
	raw, err := s.remoteImageBytes(ctx, "first.png")
	if err != nil || !bytes.Equal(raw, tinyPNG) {
		t.Fatal(err)
	}
	items, err := s.remoteImageItems()
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	mu.Lock()
	objects["/images/first.png"] = []byte("corrupted")
	mu.Unlock()
	if _, err = s.remoteImageBytes(ctx, "first.png"); err == nil {
		t.Fatal("corruption accepted")
	}
	if !s.removeMediaPath("first.png") {
		t.Fatal("delete rejected")
	}
	s.transferRemoteImage(ctx, "first.png")
	mu.Lock()
	remaining := len(objects)
	mu.Unlock()
	if remaining != 0 {
		t.Fatal("remote deletion failed")
	}
	if err = s.queueRemoteImage("first.png"); err == nil {
		t.Fatal("deleted image resurrected")
	}
	// A delete cannot bypass an in-flight upload's per-image lease.
	block = true
	path = filepath.Join(s.cfg.ImageDataDir, "second.png")
	_ = os.WriteFile(path, tinyPNG, 0600)
	if err = s.queueRemoteImage("second.png"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.transferRemoteImage(ctx, "second.png") }()
	<-entered
	if s.removeMediaPath("second.png") {
		t.Fatal("delete bypassed active transfer")
	}
	if !regularFileWithin(s.cfg.ImageDataDir, path) {
		t.Fatal("source deleted during upload")
	}
	close(release)
	<-done
	n, _, err := s.cleanupImageFiles(ctx, time.Now().Add(time.Hour), false)
	if err != nil || n == 0 {
		t.Fatal("remote retention failed", n, err)
	}
	s.transferRemoteImage(ctx, "second.png")
	items, err = s.remoteImageItems()
	if err != nil || len(items) != 0 {
		t.Fatal("expired remote image still visible", items, err)
	}
}

func TestRemoteMediaFailureRetainsLocalAndDueQueueIsFair(t *testing.T) {
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "MKCOL" {
			w.WriteHeader(201)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer dav.Close()
	s := remoteMediaTestServer(t, dav.URL, "webdav")
	for _, name := range []string{"failed.png", "ready.png"} {
		_ = os.WriteFile(filepath.Join(s.cfg.ImageDataDir, name), tinyPNG, 0600)
		if err := s.queueRemoteImage(name); err != nil {
			t.Fatal(err)
		}
	}
	s.transferRemoteImage(context.Background(), "failed.png")
	if !regularFileWithin(s.cfg.ImageDataDir, filepath.Join(s.cfg.ImageDataDir, "failed.png")) {
		t.Fatal("source lost after upload failure")
	}
	docs, err := s.store.PendingDueDocuments("mediaremote-", []string{"queued"}, 1)
	if err != nil || len(docs) != 1 || docs[0]["path"] != "ready.png" {
		t.Fatal("backoff starved ready work", docs, err)
	}
}

func TestEncryptedLocalBackupContainsNoPlainArchive(t *testing.T) {
	s := remoteMediaTestServer(t, "", "local")
	_ = s.store.ReplaceConfig(map[string]any{"backup": map[string]any{"encrypt": true, "passphrase": "test only", "include": map[string]any{"register": false}}})
	result, err := s.executeBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := stringValue(result["key"])
	if !backupName(name) || result["encrypted"] != true {
		t.Fatal(result)
	}
	files, err := os.ReadDir(s.backupDir())
	if err != nil || len(files) != 1 || files[0].Name() != name {
		t.Fatal("plaintext archive retained", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(s.backupDir(), name))
	if err != nil || !bytes.HasPrefix(raw, []byte("Salted__")) {
		t.Fatal("invalid encrypted archive", err)
	}
}
