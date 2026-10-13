package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenboxLostReceiptRetriesSameImage(t *testing.T) {
	calls := 0
	expectedHash := fmt.Sprintf("%x", sha256.Sum256(tinyPNG))
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GenBox-Source") != "test-source" || r.Header.Get("X-GenBox-Key") != "test-key" {
			t.Error("missing push identity")
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"ok":true,"contract_version":"v1","source_id":"test-source","max_image_bytes":67108864}`)
			return
		}
		calls++
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("image")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		raw, _ := io.ReadAll(file)
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != expectedHash || r.FormValue("source_sha256") != expectedHash || r.FormValue("remote_path") != "sample.png" {
			t.Error("retry changed image identity")
		}
		if calls == 1 {
			fmt.Fprint(w, `{"ok":`)
			return
		} // Source stored, receipt lost.
		fmt.Fprintf(w, `{"ok":true,"contract_version":"v1","source_id":"test-source","sha256":%q,"status":"already-imported"}`, expectedHash)
	}))
	defer remote.Close()
	s := remoteMediaTestServer(t, "", "local")
	config := map[string]any{"genbox_push": map[string]any{"enabled": true, "base_url": remote.URL, "source_id": "test-source", "push_key": "test-key"}}
	if err := s.store.ReplaceConfig(config); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.cfg.ImageDataDir, "sample.png")
	if err := os.WriteFile(path, tinyPNG, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.queueGenbox("sample.png", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pushGenbox(context.Background(), "sample.png"); err == nil {
		t.Fatal("incomplete receipt accepted")
	}
	d, err := s.store.Document(genboxName("sample.png"))
	if err != nil || d["status"] != "uncertain" || d["sha256"] != expectedHash {
		t.Fatal(d, err)
	}
	// Changed bytes cannot silently replace the image whose receipt is missing.
	changed := append(append([]byte(nil), tinyPNG...), 0)
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pushGenbox(context.Background(), "sample.png"); err == nil || calls != 1 {
		t.Fatal("changed source was sent", calls, err)
	}
	_ = os.WriteFile(path, tinyPNG, 0600)
	if err = s.queueGenbox("sample.png", false); err != nil {
		t.Fatal(err)
	}
	result, err := s.pushGenbox(context.Background(), "sample.png")
	if err != nil || result["status"] != "already-imported" || calls != 2 {
		t.Fatal(result, calls, err)
	}
	// Completed receipts suppress further POSTs, including manual double clicks.
	if _, err = s.pushGenbox(context.Background(), "sample.png"); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("source image removed", err)
	}
	config["genbox_push"].(map[string]any)["source_id"] = "different-source"
	_ = s.store.ReplaceConfig(config)
	if err = s.queueGenbox("sample.png", false); err == nil {
		t.Fatal("destination change reused old receipt")
	}
}

func TestGenboxRejectsAmbiguousReceipts(t *testing.T) {
	for _, body := range []string{`{"ok":false,"ok":true}`, `{"ok":true} {}`, strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40)} {
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		req, _ := http.NewRequest("GET", remote.URL, nil)
		_, err := genboxJSON(remote.Client(), req)
		remote.Close()
		if err == nil {
			t.Fatal("ambiguous/deep receipt accepted")
		}
	}
}
