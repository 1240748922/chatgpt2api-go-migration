package openaiidentity

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// PlatformClientID matches the original application's OAuth protocol.
const PlatformClientID = "app_2SKx67EdpoN0G6j64rFvigXD"

// Claims are informational hints only. They never verify a credential or quota.
func Claims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 65536 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func ClientID(account map[string]any) string {
	for _, candidate := range []any{account["oauth_client_id"], Claims(text(account["access_token"]))["client_id"]} {
		id := text(candidate)
		if strings.HasPrefix(id, "app_") && len(id) <= 128 && !strings.ContainsAny(id, " \r\n\t") {
			return id
		}
	}
	return PlatformClientID
}

func Plan(token string) string {
	claims := Claims(token)
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	plan := strings.ToLower(text(auth["chatgpt_plan_type"]))
	switch plan {
	case "free", "plus", "team", "pro", "enterprise", "education", "business":
		return plan
	}
	return ""
}

func text(value any) string { s, _ := value.(string); return strings.TrimSpace(s) }
