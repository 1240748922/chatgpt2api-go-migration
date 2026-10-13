package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/mediafile"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

// Task summaries and private payloads use the shared document store.

type editableFileTaskState struct {
	ID             string         `json:"id"`
	TaskID         string         `json:"taskId,omitempty"`
	Owner          string         `json:"owner_id"`
	Status         string         `json:"status"`
	Kind           string         `json:"kind"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
	Error          string         `json:"error,omitempty"`
	Result         map[string]any `json:"result,omitempty"`
	StartedAt      time.Time      `json:"started_at,omitempty"`
	Stage          string         `json:"stage"`
	AccountID      string         `json:"account_id,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
	ExportDocument string         `json:"export_document,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty"`
}

func (s *Server) thirdPartyApps(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	cfg, _ := s.store.Config()
	apps := map[string]any{}
	if configured, ok := cfg["third_party_apps"].(map[string]any); ok {
		apps = configured
	}
	writeJSON(w, http.StatusOK, map[string]any{"third_party_apps": apps})
}

func (s *Server) modelCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	chat, images, videos := []string{}, []string{}, []string{}
	for _, item := range s.catalog {
		if !item.Enabled {
			continue
		}
		switch {
		case item.Capability&model.Chat != 0:
			chat = append(chat, item.ID)
		case item.Capability&model.Image != 0 || item.Capability&model.ImageEdit != 0:
			images = append(images, item.ID)
		case item.Capability&model.Video != 0:
			videos = append(videos, item.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "model_catalog", "chat_models": chat, "image_models": images,
		"image_edit_models": images, "video_models": videos, "models": []any{},
		"all_models":             append(append(append([]string{}, chat...), images...), videos...),
		"source":                 map[string]string{"chat": "go", "image": "go", "grok": "unavailable"},
		"openai_models_endpoint": "/v1/models",
	})
}

func (s *Server) proxyRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		if _, err := s.store.UpdateConfig("proxy_runtime", updates); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		if err := s.refreshProxyRuntime(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	cfg, _ := s.store.Config()
	runtime := map[string]any{}
	if value, ok := cfg["proxy_runtime"].(map[string]any); ok {
		runtime = value
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": runtime, "status": s.proxyManager.Snapshot()})
}

func (s *Server) logsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	limit := queryInt(r, "limit", 200, 1, 20000)
	offset := queryInt(r, "offset", 0, 0, 100000000)
	items, total, err := s.store.QueryCallRecords(r.URL.Query(), limit, offset)
	if err != nil {
		writeError(w, 503, "读取请求日志失败", "storage_error")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	targets := map[string]bool{}
	for _, id := range body.IDs {
		if id = strings.TrimSpace(id); id != "" {
			targets[id] = true
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"removed": 0})
		return
	}
	removed, err := s.store.DeleteCallRecords(body.IDs, time.Time{})
	if err != nil {
		writeError(w, 503, "删除日志失败", "storage_error")
		return
	}
	writeJSON(w, 200, map[string]any{"removed": removed})
}

func (s *Server) logDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	items, _, err := s.store.QueryCallRecords(url.Values{"id": {id}}, 1, 0)
	if err != nil {
		writeError(w, 503, "读取日志失败", "storage_error")
		return
	}
	if len(items) == 0 || stringValue(items[0]["id"]) != id {
		writeError(w, 404, "log not found", "not_found")
		return
	}
	writeJSON(w, 200, map[string]any{"detail": items[0]["detail"], "item": items[0]})
}

func (s *Server) loadCallLogs() []map[string]any {
	items, _, err := s.store.QueryCallRecords(url.Values{}, 20000, 0)
	if err != nil {
		return []map[string]any{}
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items
}

func logMatches(item map[string]any, query url.Values) bool {
	return store.CallRecordMatches(item, query)
}

func (s *Server) runtimeLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	limit := queryInt(r, "limit", 300, 1, 2000)
	paths := []string{filepath.Join(s.cfg.DataDir, "runtime.log"), filepath.Join(s.cfg.DataDir, "app.log"), filepath.Join(s.cfg.RootDir, "logs", "runtime.log"), filepath.Join(s.cfg.RootDir, "logs", "app.log")}
	items := []map[string]any{}
	for _, path := range paths {
		items = append(items, tailRuntimeLog(path, limit)...)
		if len(items) >= limit {
			break
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items), "limit": limit, "sources": map[string]any{"memory": false, "files": paths}})
}

func tailRuntimeLog(path string, limit int) []map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	result := []map[string]any{}
	for i := len(lines) - 1; i >= 0 && len(result) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		level := "info"
		upper := strings.ToUpper(line)
		for _, candidate := range []string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"} {
			if strings.HasPrefix(upper, "["+candidate+"]") {
				level = strings.ToLower(candidate)
				line = strings.TrimSpace(line[len(candidate)+2:])
				break
			}
		}
		result = append(result, map[string]any{"id": fmt.Sprintf("file-%d", i), "time": "", "level": level, "message": line, "source": "file", "path": path})
	}
	return result
}

func (s *Server) prompts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	items := s.loadPromptItems()
	sources := s.loadPromptSources()
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "prompt_count": len(items), "sources": sources, "source_count": len(sources), "synced": len(items) > 0, "cached_source_count": len(sources), "enabled_source_count": len(sources)})
}

func (s *Server) loadPromptItems() []map[string]any {
	path := filepath.Join(s.cfg.RootDir, "services", "default_prompt_library.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}
	}
	var doc struct {
		Prompts []map[string]any `json:"prompts"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return []map[string]any{}
	}
	return doc.Prompts
}

func (s *Server) loadPromptSources() []map[string]any {
	path := filepath.Join(s.cfg.DataDir, "prompt_sources.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) == nil {
			if list, ok := doc["sources"].([]any); ok {
				result := []map[string]any{}
				for _, value := range list {
					if item, ok := value.(map[string]any); ok {
						result = append(result, item)
					}
				}
				if len(result) > 0 {
					return result
				}
			}
		}
	}
	return []map[string]any{{"id": "banana-prompt-quicker", "name": "Banana Prompt Quicker", "url": "local://default_prompt_library.json", "adapter": "json", "enabled": true, "built_in": true, "prompt_count": len(s.loadPromptItems())}}
}

func (s *Server) promptSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	sources := s.loadPromptSources()
	writeJSON(w, 200, map[string]any{"sources": sources, "source_count": len(sources)})
}

func (s *Server) promptSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/prompt-sources/"), "/")
	if id == "refresh" && r.Method == http.MethodPost {
		items := s.loadPromptItems()
		sources := s.loadPromptSources()
		writeJSON(w, 200, map[string]any{"items": items, "prompt_count": len(items), "sources": sources, "source_count": len(sources), "source_error_count": 0, "source_errors": []any{}})
		return
	}
	if strings.HasSuffix(id, "/refresh") {
		id = strings.TrimSuffix(id, "/refresh")
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed", "invalid_request_error")
			return
		}
		writeJSON(w, 200, map[string]any{"items": s.loadPromptItems(), "prompt_count": len(s.loadPromptItems()), "sources": s.loadPromptSources(), "source_count": len(s.loadPromptSources()), "source_error_count": 0, "source_errors": []any{}})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	sources := s.loadPromptSources()
	found := false
	for _, source := range sources {
		if stringValue(source["id"]) == id {
			found = true
			if body.Enabled != nil {
				source["enabled"] = *body.Enabled
			}
			source["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		}
	}
	if !found {
		writeError(w, 404, "prompt source not found", "not_found")
		return
	}
	path := filepath.Join(s.cfg.DataDir, "prompt_sources.json")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	raw, _ := json.MarshalIndent(map[string]any{"sources": sources}, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	for _, source := range sources {
		if stringValue(source["id"]) == id {
			writeJSON(w, 200, map[string]any{"sources": sources, "source_count": len(sources), "source": source})
			return
		}
	}
}

func queryInt(r *http.Request, key string, fallback, min, max int) int {
	value := fallback
	if raw := r.URL.Query().Get(key); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
			value = fallback
		}
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

func (s *Server) retentionCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		LogRetentionDays   int `json:"log_retention_days"`
		ImageRetentionDays int `json:"image_retention_days"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	logDays, imageDays := body.LogRetentionDays, body.ImageRetentionDays
	if logDays <= 0 {
		logDays = 30
	}
	if imageDays <= 0 {
		imageDays = 15
	}
	dryRun := strings.HasSuffix(r.URL.Path, "/preview")
	result, err := s.cleanupRetentionFiles(logDays, imageDays, dryRun)
	if err != nil {
		writeError(w, 503, "清理未完成："+err.Error(), "storage_error")
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) cleanupRetentionFiles(logDays, imageDays int, dryRun bool) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, "image-storage-maintenance", 30*time.Second)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, fmt.Errorf("存储清理正在执行")
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 30*time.Second, cancel)
	cutoff := time.Now().Add(-time.Duration(logDays) * 24 * time.Hour)
	logCount, logBytes, err := s.store.CallRetentionStats(cutoff)
	if err != nil {
		return nil, err
	}
	if !dryRun {
		logCount, err = s.store.DeleteCallRecords(nil, cutoff)
		if err != nil {
			return nil, err
		}
	}
	imageCount, imageBytes, err := s.cleanupImageFiles(ctx, time.Now().Add(-time.Duration(imageDays)*24*time.Hour), dryRun)
	if err != nil {
		return nil, err
	}
	return map[string]any{"dry_run": dryRun, "logs": map[string]any{"removed": logCount, "removed_size_bytes": logBytes, "retention_days": logDays}, "images": map[string]any{"removed": imageCount, "removed_size_bytes": imageBytes, "retention_days": imageDays}, "total_removed": logCount + imageCount, "total_size_bytes": logBytes + imageBytes}, nil
}

func (s *Server) accountCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AutoRemoveInvalidAccounts     *bool `json:"auto_remove_invalid_accounts"`
		AutoRemoveRateLimitedAccounts *bool `json:"auto_remove_rate_limited_accounts"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	removeInvalid, removeLimited := true, false
	if body.AutoRemoveInvalidAccounts != nil {
		removeInvalid = *body.AutoRemoveInvalidAccounts
	}
	if body.AutoRemoveRateLimitedAccounts != nil {
		removeLimited = *body.AutoRemoveRateLimitedAccounts
	}
	accounts, err := s.store.AccountList()
	if err != nil {
		writeError(w, 503, "账号读取失败", "storage_error")
		return
	}
	candidates := []map[string]any{}
	invalidCandidates := 0
	limitedCandidates := 0
	for _, account := range accounts {
		// Only remove an abnormal account when the upstream has explicitly
		// rejected its credential (or marked the access token expired) and no
		// refresh token is available. Transient upstream errors must not delete
		// an otherwise recoverable account.
		invalid := removeInvalid && accountAutoRemoveInvalid(account)
		limited := removeLimited && accountQuotaExhausted(account)
		if invalid || limited {
			candidates = append(candidates, account)
			if invalid {
				invalidCandidates++
			}
			if limited {
				limitedCandidates++
			}
		}
	}
	dryRun := strings.HasSuffix(r.URL.Path, "/preview")
	removed := 0
	if !dryRun {
		for _, candidate := range candidates {
			deleted, err := s.removeAccountSafely(r.Context(), candidate, removeInvalid, removeLimited)
			if err != nil {
				writeError(w, 503, "账号清理未完成，请重试", "storage_error")
				return
			}
			if deleted {
				removed++
			}
		}
	}
	total := len(candidates)
	if !dryRun {
		total = removed
	}
	writeJSON(w, 200, map[string]any{
		"dry_run":             dryRun,
		"checked":             len(accounts),
		"candidates":          len(candidates),
		"removed":             removed,
		"total_removed":       total,
		"invalid":             invalidCandidates,
		"rate_limited":        limitedCandidates,
		"remove_invalid":      removeInvalid,
		"remove_rate_limited": removeLimited,
	})
}

func (s *Server) imageQuota() (map[string]any, error) {
	availability, err := s.availabilitySnapshot()
	if err != nil {
		return nil, err
	}
	states := mapValue(availability["states"])
	quota := mapValue(availability["ready_quota"])
	gen := mapValue(quota["generation"])
	return map[string]any{"total_quota": gen["known"], "unlimited_quota_count": gen["unlimited"], "unknown_quota_count": gen["unknown"], "active_accounts": states["ready"], "abnormal_accounts": intValue(states["invalid"]) + intValue(states["quarantined"]), "disabled_accounts": states["disabled"], "providers": map[string]any{"gpt": quota}, "ready_quota": quota, "available": intValue(gen["accounts"])+intValue(gen["unknown"])+intValue(gen["unlimited"]) > 0}, nil
}

func (s *Server) saveEditableExport(task *editableFileTaskState, exported provider.EditableExportResult) (map[string]any, error) {
	relativeDir := filepath.ToSlash(editableRelativeDirectory(*task))
	root := filepath.Join(s.cfg.DataDir, "files")
	directory := filepath.Join(root, filepath.FromSlash(relativeDir))
	if !isWithin(root, directory) {
		return nil, fmt.Errorf("invalid editable file output path")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	primaryName := safeEditableOutputName(exported.Primary.Name, "."+task.Kind)
	if task.Kind == "ppt" {
		primaryName = safeEditableOutputName(exported.Primary.Name, ".pptx")
	}
	archiveName := safeEditableOutputName(exported.Archive.Name, ".zip")
	primaryPath := filepath.Join(directory, primaryName)
	archivePath := filepath.Join(directory, archiveName)
	if err := writeEditableOutput(primaryPath, exported.Primary.Data); err != nil {
		return nil, err
	}
	if err := writeEditableOutput(archivePath, exported.Archive.Data); err != nil {
		return nil, err
	}
	primaryRelative := filepath.ToSlash(filepath.Join(relativeDir, primaryName))
	archiveRelative := filepath.ToSlash(filepath.Join(relativeDir, archiveName))
	apiKey, adminKey := s.auth.Keys()
	signingKey := firstNonEmpty(apiKey, adminKey)
	return map[string]any{
		"conversation_id": exported.ConversationID,
		"primary_url":     editableDownloadURL(primaryRelative, signingKey),
		"zip_url":         editableDownloadURL(archiveRelative, signingKey),
		"primary_name":    primaryName,
		"zip_name":        archiveName,
	}, nil
}

func editableRelativeDirectory(task editableFileTaskState) string {
	ownerDigest := sha256.Sum256([]byte(task.Owner))
	taskDigest := sha256.Sum256([]byte(task.ID))
	return filepath.Join(task.Kind, hex.EncodeToString(ownerDigest[:16]), hex.EncodeToString(taskDigest[:16]))
}

func safeEditableOutputName(name, fallbackExtension string) string {
	name = strings.TrimSpace(strings.ReplaceAll(filepath.Base(name), "\x00", ""))
	if name == "" {
		name = "artifact" + fallbackExtension
	}
	if filepath.Ext(name) == "" {
		name += fallbackExtension
	}
	return name
}

func writeEditableOutput(path string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("editable output is empty")
	}
	return mediafile.AtomicWrite(path, data, 0o640)
}

func editableDownloadURL(relative, secret string) string {
	return "/files/" + strings.ReplaceAll(url.PathEscape(relative), "%2F", "/") + "?signature=" + url.QueryEscape(editableFileSignature(secret, relative))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) downloadEditableFile(w http.ResponseWriter, r *http.Request) {
	relative := strings.TrimPrefix(r.URL.Path, "/files/")
	providedSignature := strings.TrimSpace(r.URL.Query().Get("signature"))
	apiKey, adminKey := s.auth.Keys()
	expectedSignature := editableFileSignature(firstNonEmpty(apiKey, adminKey), relative)
	if providedSignature == "" || expectedSignature == "" || !hmac.Equal([]byte(providedSignature), []byte(expectedSignature)) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	root := filepath.Join(s.cfg.DataDir, "files")
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	if !isWithin(root, path) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	if !regularFileWithin(root, path) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

func editableFileSignature(secret, relative string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write([]byte(relative))
	return hex.EncodeToString(digest.Sum(nil))
}

func editableTaskPublic(task *editableFileTaskState) map[string]any {
	end := time.Now()
	if task.Status == "success" || task.Status == "error" {
		if updated, err := time.Parse(time.RFC3339Nano, task.UpdatedAt); err == nil {
			end = updated
		}
	}
	start := task.StartedAt
	if start.IsZero() {
		start, _ = time.Parse(time.RFC3339Nano, task.CreatedAt)
	}
	elapsed := 0
	if !start.IsZero() {
		elapsed = maxInt(0, int(end.Sub(start).Seconds()))
	}
	value := map[string]any{"id": task.ID, "taskId": task.TaskID, "status": task.Status, "kind": task.Kind, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "elapsed_seconds": elapsed}
	value["stage"] = task.Stage
	value["can_resume_poll"] = task.Status != "success" && (task.ConversationID != "" || task.ExportDocument != "")
	if task.Error != "" {
		value["error"] = task.Error
		value["error_code"] = task.ErrorCode
	}
	if task.Result != nil {
		value["result"] = task.Result
	}
	return value
}

func (t *editableFileTaskState) OwnerID() string { return t.Owner }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Server) searchAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, 400, "prompt is required", "invalid_request_error")
		return
	}
	if body.Model == "" {
		body.Model = "gpt-4o-search-preview"
	}
	request := protocol.ChatRequest{Model: body.Model, Messages: []protocol.Message{{Role: "user", Content: body.Prompt}}}
	route, ok := model.ResolveChat(body.Model)
	if !ok {
		route = model.ChatRoute{Mode: "normal", PoolCandidates: []string{"basic", "super", "heavy"}}
	}
	recorder := &responseCapture{header: make(http.Header)}
	if route.Console {
		s.completeConsoleChat(recorder, r, request)
	} else {
		s.completeChat(recorder, r, request, route)
	}
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if json.Unmarshal(recorder.body.Bytes(), &chat) != nil {
		writeError(w, 502, "invalid search response", "upstream_error")
		return
	}
	answer := ""
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				answer = stringValue(message["content"])
			}
		}
	}
	writeJSON(w, 200, map[string]any{"answer": answer, "sources": []any{}, "status": "completed", "model": body.Model})
}

func (s *Server) authIdentity(r *http.Request) string {
	if owner, ok := r.Context().Value(imageTaskOwnerKey{}).(string); ok {
		return owner
	}
	token := s.auth.APIKey(r)
	if identity, ok := s.auth.Identity(token); ok && identity.ID != "" {
		return identity.ID
	}
	return "anonymous"
}
