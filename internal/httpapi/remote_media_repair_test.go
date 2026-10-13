package httpapi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteRepairAndPublicRedirectUseCommittedDestination(t *testing.T) {
	s := remoteMediaTestServer(t, "https://storage.example.test/dav", "webdav")
	if err := os.WriteFile(filepath.Join(s.cfg.ImageDataDir, "repair.png"), tinyPNG, 0600); err != nil {
		t.Fatal(err)
	}
	var dir *os.File
	defer func() {
		if dir != nil {
			_ = dir.Close()
		}
	}()
	s.repairRemoteMedia(context.Background(), &dir)
	doc, err := s.store.Document(remoteMediaName("repair.png"))
	if err != nil || doc["status"] != "queued" {
		t.Fatal("unindexed local result not recovered", doc, err)
	}
	_, err = s.store.PatchManagedConfig(map[string]any{"image_storage": map[string]any{"public_base_url": "https://cdn.example.test/images"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.remotePublicImageURL("repair.png", doc) != "" {
		t.Fatal("uncommitted upload redirected")
	}
	doc, err = s.store.MutateDocument(remoteMediaName("repair.png"), func(d map[string]any) error { d["status"] = "done"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/images/repair.png", nil)
	if !s.serveRemoteImage(w, r, "repair.png") || w.Code != 307 || w.Header().Get("Location") != "https://cdn.example.test/images/repair.png" {
		t.Fatal("public URL not used", w.Code, w.Header())
	}
	_, _ = s.store.PatchManagedConfig(map[string]any{"image_storage": map[string]any{"webdav_url": "https://different.example.test/dav"}})
	if s.remotePublicImageURL("repair.png", doc) != "" {
		t.Fatal("redirect used a different storage destination")
	}
	for _, bad := range []string{"javascript:bad", "https://user:pass@cdn.example.test", "https://cdn.example.test/?token=secret", "https://cdn.example.test/#bad"} {
		_, _ = s.store.PatchManagedConfig(map[string]any{"image_storage": map[string]any{"webdav_url": "https://storage.example.test/dav", "public_base_url": bad}})
		if s.remotePublicImageURL("repair.png", doc) != "" {
			t.Fatal("unsafe CDN base accepted")
		}
	}
}
