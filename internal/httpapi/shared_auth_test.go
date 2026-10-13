package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPersistedAuthRotationIsSharedAndRejectsStaleLocalKey(t *testing.T) {
	dsn := testpg.DSN(t)
	cfg := adminTestConfig(t.TempDir())
	cfg.DatabaseURL = dsn
	first := New(cfg)
	defer first.Close()
	stale := `{"items":[{"key":"stale-local","enabled":true}]}`
	if err := os.MkdirAll(filepath.Dir(cfg.AuthKeysPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AuthKeysPath, []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := first.store.PatchConfig(map[string]any{"app_key": "new-admin", "auth-key": "new-api"})
	if err != nil {
		t.Fatal(err)
	}
	first.applyAuthConfig(values)
	otherCfg := adminTestConfig(t.TempDir())
	otherCfg.DatabaseURL = dsn
	second := New(otherCfg)
	defer second.Close()
	for _, s := range []*Server{first, second} {
		for _, test := range []struct {
			key   string
			valid bool
		}{{"new-api", true}, {"api-secret", false}, {"stale-local", false}} {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer "+test.key)
			if got := s.auth.ValidAPIRequest(r); got != test.valid {
				t.Fatalf("key %q accepted=%v", test.key, got)
			}
		}
	}
	values, err = first.store.PatchConfig(map[string]any{"auth-key": ""})
	if err != nil {
		t.Fatal(err)
	}
	second.applyAuthConfig(values)
	api, _ := second.auth.Keys()
	if api != "" {
		t.Fatal("explicit key clear fell back to startup key")
	}
}
