package accounts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

var ReadinessLabels = map[string]string{"ready": "凭据就绪", "refreshing": "正在续期", "uncertain": "续期结果待确认", "unknown": "有效期未知", "expiring": "有效期不足", "expired": "AT 已过期", "quarantined": "鉴权待核验", "invalid": "确认异常", "disabled": "已禁用"}

type Readiness struct {
	State            string     `json:"state"`
	Label            string     `json:"label"`
	ExpiresAt        *time.Time `json:"expires_at"`
	RemainingSeconds *int64     `json:"remaining_seconds"`
	Recoverable      bool       `json:"has_refresh_path"`
}

func TokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return time.Time{}, false
	}
	value, ok := claims["exp"].(float64)
	if !ok || value <= 0 || value >= 253402300800 {
		return time.Time{}, false
	}
	return time.Unix(int64(value), 0), true
}
func CredentialReadiness(item map[string]any, now time.Time, minimum time.Duration) Readiness {
	token := firstString(item, "access_token", "accessToken")
	exp, known := TokenExpiry(token)
	state := "ready"
	remote := strings.ToLower(firstString(item, "last_remote_check_result"))
	switch {
	case !boolValue(item["enabled"], true) || firstString(item, "status") == "禁用":
		state = "disabled"
	case boolValue(item["refresh_inflight"], false) && timestamp(item, "refresh_started_at").Add(120*time.Second).After(now):
		state = "refreshing"
	case boolValue(item["refresh_uncertain"], false):
		state = "uncertain"
	case remote == "pending":
		state = "quarantined"
	case remote == "invalid" || firstString(item, "status") == "异常":
		state = "invalid"
	case token == "":
		state = "unknown"
	case !known:
		state = "unknown"
	case !exp.After(now):
		state = "expired"
	case exp.Sub(now) <= minimum:
		state = "expiring"
	}
	r := Readiness{State: state, Label: ReadinessLabels[state], Recoverable: firstString(item, "refresh_token") != "" && firstString(item, "refresh_token_invalid_at") == ""}
	if known {
		remaining := int64(exp.Sub(now).Seconds())
		r.ExpiresAt = &exp
		r.RemainingSeconds = &remaining
	}
	return r
}

// Quota expiry alone never clears a confirmed exhaustion; only a remote sync can do that.
func ImageEligible(item map[string]any, edit bool, now time.Time, minimum time.Duration) bool {
	if CredentialReadiness(item, now, minimum).State != "ready" {
		return false
	}
	if timestamp(item, "cooldown_until", "next_retry_at").After(now) {
		return false
	}
	exhausted := timestamp(item, "image_quota_exhausted_at")
	recovered := timestamp(item, "image_quota_recovered_at")
	if !exhausted.IsZero() && !recovered.After(exhausted) {
		return false
	}
	if timestamp(item, "image_quota_cooldown_until").After(now) {
		return false
	}
	if edit && timestamp(item, "upload_cooldown_until").After(now) {
		return false
	}
	if q, exists := item["quota"]; exists && q != nil && !boolValue(item["image_quota_unknown"], false) && intValue(q) <= 0 {
		return false
	}
	return true
}
