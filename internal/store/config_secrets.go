package store

import (
	"strings"
)

func configSecret(path string) bool {
	switch path {
	case "api_key", "admin_key", "webui_key", "webui_admin_key", "basic.api_key", "ai_review.api_key", "backup.access_key_id", "backup.secret_access_key", "backup.passphrase", "image_storage.webdav_password", "genbox_push.push_key", "account_import_api.key", "proxy_runtime.clearance.cf_cookies", "proxy_runtime.clearance.cf_clearance":
		return true
	}
	return false
}
func mergeManagedConfig(dst, updates map[string]any, prefix string) {
	for k, v := range updates {
		if strings.HasPrefix(k, "has_") {
			continue
		}
		path := prefix + k
		if configSecret(path) {
			if v == nil {
				dst[k] = ""
			} else if strings.TrimSpace(stringValue(v)) != "" {
				dst[k] = v
			}
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			current, ok := dst[k].(map[string]any)
			if !ok {
				current = map[string]any{}
			}
			mergeManagedConfig(current, nested, path+".")
			dst[k] = current
		} else {
			dst[k] = v
		}
	}
}

// Merge and preserve blank secret fields inside the same transaction as all
// other settings, so two administration requests cannot restore stale values.
func (s *Store) PatchManagedConfig(updates map[string]any) (map[string]any, error) {
	if s.pg != nil {
		return s.MutateDocument("config", func(d map[string]any) error { mergeManagedConfig(d, updates, ""); return nil })
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := loadMap(s.configPath)
	if err != nil {
		return nil, err
	}
	mergeManagedConfig(d, updates, "")
	return d, writeJSON(s.configPath, d)
}
func RedactManagedConfig(src map[string]any) map[string]any {
	var walk func(map[string]any, string) map[string]any
	walk = func(value map[string]any, prefix string) map[string]any {
		out := map[string]any{}
		for k, v := range value {
			if configSecret(prefix + k) {
				out[k] = ""
				out["has_"+k] = strings.TrimSpace(stringValue(v)) != ""
			} else if nested, ok := v.(map[string]any); ok {
				out[k] = walk(nested, prefix+k+".")
			} else {
				out[k] = v
			}
		}
		return out
	}
	return walk(src, "")
}
