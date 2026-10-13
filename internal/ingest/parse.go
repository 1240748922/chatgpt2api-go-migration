package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/openaiidentity"
)

// Parse reads credential keys, never guesses an access token from an RT prefix.
// Imported quota, cooldown and proxy metadata are deliberately not authoritative.
func Parse(text, mode string) ([]map[string]any, error) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, errors.New("没有可导入的凭据")
	}
	rows := []map[string]any{}
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 12 {
			return
		}
		switch x := v.(type) {
		case []any:
			for _, child := range x {
				walk(child, depth+1)
			}
		case map[string]any:
			at := value(x, "access_token", "accessToken")
			rt := value(x, "refresh_token", "refreshToken")
			if at != "" || rt != "" {
				p := map[string]any{"access_token": at, "refresh_token": rt, "source_type": "chatgpt_web", "image_quota_unknown": true}
				if strings.EqualFold(value(x, "source_type", "export_type"), "codex") || strings.EqualFold(value(x, "type"), "codex") {
					p["source_type"] = "codex"
				}
				for _, key := range []string{"email", "type", "plan_type", "id_token", "oauth_client_id"} {
					if t := value(x, key); t != "" && !(key == "type" && strings.EqualFold(t, "codex")) {
						p[key] = t
					}
				}
				if u, ok := x["user"].(map[string]any); ok && value(p, "email") == "" {
					p["email"] = value(u, "email")
				}
				rows = append(rows, p)
				return
			}
			for _, key := range []string{"accounts", "items", "data", "results", "credentials"} {
				if child, ok := x[key]; ok {
					walk(child, depth+1)
				}
			}
		}
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		decoder := json.NewDecoder(strings.NewReader(text))
		for {
			var v any
			if err := decoder.Decode(&v); err == io.EOF {
				break
			} else if err != nil {
				return nil, errors.New("JSON 格式不正确，请检查文件内容")
			}
			walk(v, 0)
		}
	} else {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "{") {
				var v any
				if json.Unmarshal([]byte(line), &v) != nil {
					return nil, errors.New("JSON 行格式不正确")
				}
				walk(v, 0)
				continue
			}
			key := "access_token"
			if mode == "refresh_token" || strings.HasPrefix(line, "rt.") {
				key = "refresh_token"
			}
			rows = append(rows, map[string]any{key: line, "source_type": "chatgpt_web", "image_quota_unknown": true})
		}
	}
	unique := []map[string]any{}
	seen := map[string]bool{}
	for _, row := range rows {
		if value(row, "plan_type") == "" {
			if plan := openaiidentity.Plan(value(row, "access_token")); plan != "" {
				row["plan_type"] = plan
			}
		}
		key := value(row, "refresh_token")
		if key == "" {
			key = value(row, "access_token")
		}
		sum := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(sum[:])
		if !seen[hash] {
			seen[hash] = true
			row["id"] = "import-" + hash[:32]
			unique = append(unique, row)
		}
	}
	if len(unique) == 0 {
		return nil, errors.New("没有找到 access_token / accessToken 或 refresh_token / refreshToken")
	}
	if len(unique) > 50000 {
		return nil, errors.New("单次最多导入 50000 个账号")
	}
	return unique, nil
}
func value(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
