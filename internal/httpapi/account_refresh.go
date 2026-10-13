package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type accountRefreshProgress struct {
	Total        int            `json:"total"`
	Processed    int            `json:"processed"`
	Done         bool           `json:"done"`
	Error        string         `json:"error,omitempty"`
	StatusCounts map[string]int `json:"status_counts"`
	TotalQuota   int            `json:"total_quota"`
	Result       map[string]any `json:"result,omitempty"`
}

func (s *Server) accountAccessTokenRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessTokens []string          `json:"access_tokens"`
		IDs          []string          `json:"account_ids"`
		Selection    *accountSelection `json:"selection"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	refs, err := s.selectedOperationRefs(body.AccessTokens, body.IDs, body.Selection)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens is required", "invalid_request_error")
		return
	}
	updated := 0
	errorsOut := make([]map[string]any, 0)
	for _, ref := range refs {
		account, err := s.accountByRef(ref)
		if err != nil {
			errorsOut = append(errorsOut, map[string]any{"token": tokenPreview(ref), "error": safeRefreshError(err)})
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		_, refreshErr := s.syncCredential(ctx, account, true)
		cancel()
		if refreshErr != nil {
			errorsOut = append(errorsOut, map[string]any{"token": tokenPreview(ref), "error": safeRefreshError(refreshErr)})
			continue
		}
		updated++
	}
	items, _ := s.store.AccountList()
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "success_count": updated, "errors": errorsOut, "items": accountsForAPI(items, s.configuredImageMinimumValidity())})
}

func (s *Server) accountRefreshStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessTokens []string          `json:"access_tokens"`
		IDs          []string          `json:"account_ids"`
		Selection    *accountSelection `json:"selection"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	refs, err := s.selectedOperationRefs(body.AccessTokens, body.IDs, body.Selection)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if len(refs) == 0 && body.Selection == nil && len(body.IDs) == 0 && len(body.AccessTokens) == 0 {
		items, err := s.store.AccountList()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		for _, item := range items {
			if ref := accountPublicRef(item); ref != "" {
				refs = append(refs, ref)
			}
		}
		refs = uniqueAccountRefs(refs)
	}
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens is required", "invalid_request_error")
		return
	}

	progressID, err := s.createAccountRefresh(refs)
	if err != nil {
		writeError(w, 503, "保存同步任务失败", "storage_error")
		return
	}
	s.launchBackground(func() { s.runAccountRefresh(s.backgroundCtx, progressID) })
	writeJSON(w, 200, map[string]any{"progress_id": progressID})
}

func (s *Server) accountRefreshProgressAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/refresh/progress/")
	id = strings.TrimPrefix(id, "/api/accounts/operations/")
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeError(w, 404, "progress not found", "not_found")
		return
	}
	doc, err := s.store.Document("refreshjob-" + id)
	if err != nil {
		writeError(w, 503, "读取同步任务失败", "storage_error")
		return
	}
	if doc["id"] == nil {
		writeError(w, 404, "progress not found", "not_found")
		return
	}
	counts := map[string]int{"正常": 0, "限流": 0, "异常": 0, "禁用": 0}
	for k, v := range mapValue(doc["status_counts"]) {
		counts[k] = intValue(v)
	}
	result := map[string]any{"refreshed": intValue(doc["success"]), "errors": doc["errors"], "failed": intValue(doc["failed"])}
	if doc["status"] == "completed" {
		// Keep the legacy result shape. Detailed account lists have their own paginated API.
		result["items"] = []any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, accountRefreshProgress{Total: intValue(doc["total"]), Processed: intValue(doc["processed"]), Done: doc["status"] == "completed", StatusCounts: counts, TotalQuota: intValue(doc["total_quota"]), Result: result})
}

func (s *Server) openAIAccountClient() *provider.OpenAIAccountClient {
	client := s.requestClient
	if client == nil {
		client = &http.Client{
			Transport: proxyruntime.NewTransport(http.DefaultTransport),
			Timeout:   45 * time.Second,
		}
	}
	return provider.NewOpenAIAccountClient(
		s.cfg.OpenAIBaseURL,
		s.cfg.OpenAIOAuthURL,
		client,
		s.proxyManager,
		provider.ClearanceConfig{URL: s.cfg.FlareSolverrURL, Enabled: s.cfg.ClearanceEnabled, Timeout: s.cfg.ClearanceTimeout},
	)
}

func accountRefreshConcurrency() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GO_ACCOUNT_REFRESH_CONCURRENCY")))
	if err != nil || value < 1 {
		return 3
	}
	if value > 10 {
		return 10
	}
	return value
}

func (s *Server) accountByRef(ref string) (map[string]any, error) {
	if item, err := s.store.FindAccount(ref); err == nil {
		return item, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if accountRefMatches(item, ref) {
			return item, nil
		}
	}
	return nil, os.ErrNotExist
}

func accountRefreshFailureUpdates(err error) map[string]any {
	if err == nil {
		return nil
	}
	message := safeRefreshError(err)
	lower := strings.ToLower(message)
	now := time.Now().UTC().Format(time.RFC3339)
	if errors.Is(err, provider.ErrInvalidAccessToken) ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "http 401") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid access token") ||
		strings.Contains(lower, "access token invalid") {
		return map[string]any{
			"status":                "异常",
			"last_refresh_error":    message,
			"last_refresh_error_at": now,
			"last_error_kind":       "auth_invalid",
			"last_error_status":     401,
			"status_reason_code":    "account_invalid",
		}
	}
	if strings.Contains(lower, "http 429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "额度") {
		return map[string]any{
			"status":                "限流",
			"last_refresh_error":    message,
			"last_refresh_error_at": now,
			"last_error_kind":       "quota_exhausted",
			"status_reason_code":    "image_quota_exhausted",
		}
	}
	return map[string]any{
		"last_refresh_warning":    message,
		"last_refresh_warning_at": now,
		"last_refresh_error":      nil,
		"last_refresh_error_at":   nil,
		"last_error_kind":         nil,
		"last_error_status":       nil,
		"last_error_message":      nil,
		"status_reason_code":      nil,
	}
}

func cloneIntMap(value map[string]int) map[string]int {
	copy := map[string]int{}
	for key, item := range value {
		copy[key] = item
	}
	return copy
}

func safeRefreshError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	message = credentialErrorPattern.ReplaceAllString(message, "[redacted]")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

var credentialErrorPattern = regexp.MustCompile(`(?i)(?:Bearer\s+[^\s"',;]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|rt\.[A-Za-z0-9._-]+|(?:access_token|refresh_token|id_token|client_secret)\s*[=:]\s*"?[^\s",;}]+)`)
