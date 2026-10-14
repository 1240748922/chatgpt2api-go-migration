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
			response := object(x["response"])
			auth := object(x["auth_session"])
			if auth == nil {
				auth = object(response["auth_session"])
			}
			session := object(response["session"])
			tokens := object(x["tokens"])
			at := first(value(x, "access_token", "accessToken"), value(auth, "accessToken", "access_token"), value(response, "access_token", "accessToken"), value(session, "access_token", "accessToken"), value(tokens, "access_token", "accessToken"))
			// Registration exports may use refresh_token for the mailbox. Only
			// oauth_refresh_token/auth_session is an OpenAI RT in that format.
			rt := first(value(x, "oauth_refresh_token"), value(auth, "refreshToken", "refresh_token"), value(tokens, "refresh_token", "refreshToken"))
			registration := x["mailbox"] != nil || x["oauth_refresh_token"] != nil || x["refresh_token_status"] != nil
			if rt == "" && !registration {
				rt = first(value(x, "refresh_token", "refreshToken"), value(response, "refresh_token", "refreshToken"), value(session, "refresh_token", "refreshToken"))
			}
			if at != "" || rt != "" {
				p := map[string]any{"access_token": at, "refresh_token": rt, "source_type": "chatgpt_web", "image_quota_unknown": true}
				if strings.EqualFold(value(x, "source_type", "export_type"), "codex") || strings.EqualFold(value(x, "type"), "codex") {
					p["source_type"] = "codex"
				}
				for _, key := range []string{"email", "type", "plan_type", "oauth_client_id"} {
					if t := value(x, key); t != "" && !(key == "type" && strings.EqualFold(t, "codex")) {
						p[key] = t
					}
				}
				if email := first(value(p, "email"), value(object(x["user"]), "email"), value(object(auth["user"]), "email"), value(object(response["user"]), "email"), value(object(session["user"]), "email"), value(object(x["mailbox"]), "email")); email != "" {
					p["email"] = email
				}
				for key, credential := range map[string]string{
					"id_token":          first(value(x, "id_token", "idToken"), value(auth, "idToken", "id_token")),
					"login_password":    value(x, "login_password", "password", "account_password"),
					"two_factor_secret": value(x, "two_factor_secret", "totp_secret", "two_fa", "2fa_secret"),
					"session_token":     first(value(x, "session_token", "sessionToken"), value(auth, "sessionToken", "session_token")),
					"cookie_header":     first(value(x, "cookie_header"), value(response, "cookie_header")),
				} {
					if credential != "" {
						p[key] = credential
					}
				}
				if value(p, "cookie_header") == "" && value(p, "session_token") != "" {
					p["cookie_header"] = "__Secure-next-auth.session-token=" + value(p, "session_token")
				}
				rows = append(rows, p)
				return
			}
			if registration {
				return // Never import an unrelated mailbox credential as an account.
			}
			for _, key := range []string{"accounts", "items", "data", "results", "credentials", "auth_session", "tokens", "response", "session"} {
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
		if mode == "session_json" {
			return nil, errors.New("Session 导入需要有效的 JSON 内容")
		}
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

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
