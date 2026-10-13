package store

import (
	"path/filepath"
	"testing"
)

func TestManagedConfigSecretPreservationAndExplicitClear(t *testing.T) {
	root := t.TempDir()
	s := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	original := map[string]any{"backup": map[string]any{"secret_access_key": "secret", "passphrase": "password", "enabled": true}, "ai_review": map[string]any{"api_key": "key"}}
	if err := s.ReplaceConfig(original); err != nil {
		t.Fatal(err)
	}
	redacted := RedactManagedConfig(original)
	backup := redacted["backup"].(map[string]any)
	if backup["secret_access_key"] != "" || backup["has_secret_access_key"] != true {
		t.Fatal(backup)
	}
	if original["backup"].(map[string]any)["secret_access_key"] != "secret" {
		t.Fatal("redaction mutated original")
	}
	result, err := s.PatchManagedConfig(map[string]any{"backup": map[string]any{"secret_access_key": "", "enabled": false, "has_secret_access_key": false}})
	if err != nil {
		t.Fatal(err)
	}
	backup = result["backup"].(map[string]any)
	if backup["secret_access_key"] != "secret" || backup["enabled"] != false {
		t.Fatal(backup)
	}
	result, err = s.PatchManagedConfig(map[string]any{"backup": map[string]any{"secret_access_key": nil}})
	if err != nil || result["backup"].(map[string]any)["secret_access_key"] != "" {
		t.Fatal(result, err)
	}
}
