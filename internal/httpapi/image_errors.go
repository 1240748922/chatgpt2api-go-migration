package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func imageErrorCode(err error) (int, string, string) {
	status := upstreamStatus(err)
	code := "image_generation_failed"
	message := "The image generation tool encountered an error. Please try again."
	var flow *provider.ImageFlowError
	errors.As(err, &flow)
	var network *net.OpError
	var networkTimeout net.Error
	errors.As(err, &network)
	errors.As(err, &networkTimeout)
	switch {
	case errors.Is(err, errLocalCapacity):
		status = 503
		code = "local_busy"
	case errors.Is(err, accounts.ErrUnavailable):
		status = 503
		code = "no_available_account"
	case errors.Is(err, context.Canceled):
		status = 499
		code = "request_cancelled"
		message = "Request cancelled."
	case errors.Is(err, provider.ErrImageTextReply):
		code = "upstream_text_reply"
	case network != nil && network.Op == "dial" && networkTimeout != nil && networkTimeout.Timeout():
		status = 504
		code = "upstream_connection_timeout"
	case network != nil && network.Op == "dial":
		code = "upstream_connection_failed"
	case errors.Is(err, context.DeadlineExceeded):
		status = 504
		code = "image_generation_timeout"
		message = "Image generation timed out. Please try again."
	case status == 504 && flow != nil && flow.Stage == "poll":
		code = "image_generation_timeout"
		message = "Image generation timed out. Please try again."
	case flow != nil && flow.Stage == "upload" && status == 429:
		code = "image_upload_rate_limited"
	case status == 429 && (strings.Contains(strings.ToLower(err.Error()), "quota") || strings.Contains(strings.ToLower(err.Error()), "image generation limit")):
		code = "image_quota_exhausted"
	case status == 429:
		code = "upstream_rate_limited"
	case status == 401:
		code = "upstream_auth_failed"
		status = 502
	case flow != nil && (flow.Stage == "download" || flow.Stage == "storage"):
		code = "image_delivery_failed"
	case flow != nil && flow.Stage == "generation" && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || network != nil):
		code = "image_stream_interrupted"
	}
	return status, code, message
}
func (s *Server) writeImageError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := imageErrorCode(err)
	var settingsError *imageSettingsError
	if errors.As(err, &settingsError) {
		status = settingsError.Status
		code = settingsError.Code
		message = settingsError.Message
	}
	if cfg, e := s.store.Config(); e == nil && boolValue(cfg["image_error_friendly_enabled"], false) {
		key := "fallback"
		switch code {
		case "local_busy":
			key = "local_busy"
		case "no_available_account":
			key = "no_account"
		case "image_quota_exhausted":
			key = "quota"
		case "image_generation_timeout":
			key = "poll_timeout"
		case "upstream_auth_failed":
			key = "token_invalid"
		case "unsupported_model":
			key = "unsupported_model"
		case "upstream_text_reply":
			key = "text_reply"
		case "upstream_connection_failed":
			key = "connection_failed"
		case "upstream_connection_timeout":
			key = "connection_timeout"
		case "image_stream_interrupted":
			key = "stream_interrupted"
		}
		defaults := map[string]string{"fallback": "图片生成请求失败，请稍后重试。", "local_busy": "本实例请求并发已满，请稍后重试。", "no_account": "当前没有符合条件的空闲图片账号，请稍后重试。", "quota": "图片账号额度已用完，请稍后重试或联系管理员。", "poll_timeout": "图片任务尚未返回结果，可恢复查询原任务。", "token_invalid": "图片账号鉴权失败，请等待后台核验。", "unsupported_model": "当前模型不支持图片生成，请检查 model 参数。"}
		defaults["text_reply"] = "上游返回了文本说明，未生成图片。请调整提示词。"
		defaults["connection_failed"] = "连接上游图片服务失败，请检查网络或代理。"
		defaults["connection_timeout"] = "连接上游图片服务超时，请稍后重试。"
		defaults["stream_interrupted"] = "图片生成连接中断；如已提交，请恢复查询原任务。"
		message = firstNonEmpty(stringValue(mapValue(cfg["image_error_messages"])[key]), defaults[key])
	}
	if e, ok := err.(*reviewError); ok {
		status = e.status
		message = e.message
		code = "content_review_failed"
	}
	s.enrichRequestMonitor(r, map[string]any{"raw_error": safeRefreshError(err), "failure_code": code})
	if status == 429 || status == 503 {
		w.Header().Set("Retry-After", "3")
	}
	writeError(w, status, message, code)
}
func (s *Server) recordImageAttempt(r *http.Request, slot, attempt int, a accounts.Account, duration time.Duration, err error) {
	if r == nil {
		return
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return
	}
	item := map[string]any{"slot": slot, "attempt": attempt, "account_email": a.Fields["email"], "account_id": a.Fields["id"], "duration_ms": duration.Milliseconds(), "status": "success"}
	if err != nil {
		status, code, _ := imageErrorCode(err)
		item["status"] = "failed"
		item["status_code"] = status
		item["failure_code"] = code
		item["raw_error"] = safeRefreshError(err)
		var flow *provider.ImageFlowError
		if errors.As(err, &flow) {
			item["stage"] = flow.Stage
			item["submitted"] = flow.Submitted
			item["conversation_id"] = flow.ConversationID
		}
	}
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if record := s.monitor.active[id]; record != nil {
		record.Attempts = append(record.Attempts, item)
	}
}
