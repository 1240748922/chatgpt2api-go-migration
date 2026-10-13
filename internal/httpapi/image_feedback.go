package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Commit restrictions before releasing the image slot so other replicas cannot
// dispatch stale candidates. A delivery failure says nothing about credentials.
func (s *Server) imageFailureFeedback(account accounts.Account, err error) error {
	var flow *provider.ImageFlowError
	errors.As(err, &flow)
	stage := "unknown"
	if flow != nil {
		stage = flow.Stage
	}
	if stage == "download" || stage == "storage" || stage == "poll" {
		return nil
	}
	status := upstreamStatus(err)
	now := time.Now()
	fields := map[string]any{"last_error_at": now.UTC().Format(time.RFC3339), "last_error_message": safeRefreshError(err), "last_error_status": status}
	switch {
	case status == http.StatusUnauthorized:
		fields["last_remote_check_result"] = "pending"
		fields["status_reason_code"] = "auth_pending"
		fields["last_error_kind"] = "auth_pending"
		fields["next_maintenance_at"] = now.UTC().Format(time.RFC3339)
	case status == http.StatusTooManyRequests:
		var upstream *protocol.UpstreamError
		errors.As(err, &upstream)
		wait := 5 * time.Minute
		if upstream != nil && upstream.RetryAfter > 0 {
			wait = upstream.RetryAfter
		}
		message := strings.ToLower(err.Error())
		if stage == "upload" {
			fields["upload_cooldown_until"] = now.Add(wait).UTC().Format(time.RFC3339)
			fields["last_error_kind"] = "upload_rate_limited"
		} else if strings.Contains(message, "image_quota") || strings.Contains(message, "image generation limit") || strings.Contains(message, "image quota") {
			fields["image_quota_exhausted_at"] = now.UTC().Format(time.RFC3339Nano)
			fields["image_quota_cooldown_until"] = now.Add(wait).UTC().Format(time.RFC3339)
			fields["quota"] = 0
			fields["image_quota_unknown"] = false
			fields["last_error_kind"] = "image_quota_exhausted"
			fields["next_maintenance_at"] = now.Add(wait).UTC().Format(time.RFC3339)
		} else {
			fields["cooldown_until"] = now.Add(wait).UTC().Format(time.RFC3339)
			fields["last_error_kind"] = "rate_limited"
		}
	default:
		// Network and generation errors are not evidence of account invalidity.
		return nil
	}
	_, e := s.store.UpdateAccountRuntime(account.Token, fields)
	return e
}
