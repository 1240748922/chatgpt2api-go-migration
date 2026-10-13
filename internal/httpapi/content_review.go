package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type contentReviewKey struct{}
type reviewError struct {
	status  int
	message string
}

func (e *reviewError) Error() string { return e.message }

var reviewDataURI = regexp.MustCompile(`data:[\w/.+;-]+;base64,[A-Za-z0-9+/=]+`)

func sanitizeReviewText(text string) string {
	text = reviewDataURI.ReplaceAllString(text, "[image]")
	chars := []rune(text)
	if len(chars) > 100000 {
		text = string(chars[:49980]) + "\n…[truncated]…\n" + string(chars[len(chars)-49980:])
	}
	return text
}

func (s *Server) checkContent(ctx context.Context, text string) error {
	if ctx.Value(contentReviewKey{}) != nil || strings.TrimSpace(text) == "" {
		return nil
	}
	cfg, err := s.store.Config()
	if err != nil {
		return err
	}
	for _, word := range stringList(cfg["sensitive_words"]) {
		if word != "" && strings.Contains(text, word) {
			return &reviewError{400, "检测到敏感词，拒绝本次任务"}
		}
	}
	review, _ := cfg["ai_review"].(map[string]any)
	if !boolValue(review["enabled"], false) {
		return nil
	}
	base, key, model := strings.TrimRight(stringValue(review["base_url"]), "/"), stringValue(review["api_key"]), stringValue(review["model"])
	if base == "" || key == "" || model == "" {
		return &reviewError{400, "AI 审核配置不完整"}
	}
	failed := func() error {
		if !boolValue(review["fail_open"], true) {
			return &reviewError{503, "AI 审核服务暂时不可用，请稍后重试"}
		}
		return ctx.Err()
	}
	prompt := firstNonEmpty(stringValue(review["prompt"]), "判断用户请求是否允许。只回答 ALLOW 或 REJECT。")
	raw, _ := json.Marshal(map[string]any{"model": model, "temperature": 0, "messages": []any{map[string]any{"role": "user", "content": prompt + "\n\n用户请求:\n" + sanitizeReviewText(text) + "\n\n只回答 ALLOW 或 REJECT。"}}})
	endpoint := base + "/v1/chat/completions"
	if strings.HasSuffix(base, "/v1") {
		endpoint = base + "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return failed()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return failed()
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failed()
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return failed()
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Choices) == 0 {
		return failed()
	}
	decision := strings.ToLower(strings.TrimSpace(result.Choices[0].Message.Content))
	for _, prefix := range []string{"allow", "pass", "true", "yes", "通过", "允许", "安全"} {
		if strings.HasPrefix(decision, prefix) {
			return nil
		}
	}
	for _, prefix := range []string{"reject", "deny", "block", "false", "no", "拒绝", "不允许", "违规", "禁止"} {
		if strings.HasPrefix(decision, prefix) {
			return &reviewError{400, "AI 审核未通过，拒绝本次任务"}
		}
	}
	return failed()
}

func (s *Server) reviewRequest(w http.ResponseWriter, r *http.Request, text string) *http.Request {
	if err := s.checkContent(r.Context(), text); err != nil {
		status := 503
		message := "内容审核暂时不可用"
		if e, ok := err.(*reviewError); ok {
			status = e.status
			message = e.message
		}
		writeError(w, status, message, "content_review_failed")
		return nil
	}
	return r.WithContext(context.WithValue(r.Context(), contentReviewKey{}, true))
}

func contentReviewStatus(err error) (int, string) {
	if e, ok := err.(*reviewError); ok {
		return e.status, e.message
	}
	return 503, fmt.Sprint(err)
}
