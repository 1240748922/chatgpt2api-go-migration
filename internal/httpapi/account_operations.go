package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

// Replacement credentials must not race an RT rotation or a leased generation.
func (s *Server) updateManagedAccount(parent context.Context, ref string, updates map[string]any) (map[string]any, error) {
	item, err := s.store.FindAccount(ref)
	if err != nil {
		return nil, err
	}
	replacing := false
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if v, ok := updates[key]; ok && stringValue(v) != stringValue(item[key]) {
			replacing = true
		}
	}
	if !replacing {
		next, _, e := s.store.UpdateAccount(accountToken(item), updates)
		return next, e
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	var credentialLease *store.Lease
	for _, prefix := range []string{"credential:", "account-use:"} {
		lease, e := s.store.AcquireLease(ctx, prefix+store.AccountResource(item), 30*time.Second)
		if e != nil {
			return nil, e
		}
		if lease == nil {
			return nil, errCredentialBusy
		}
		defer lease.Release()
		if prefix == "credential:" {
			credentialLease = lease
		}
		lease.KeepAlive(ctx, 30*time.Second, cancel)
	}
	busy, err := s.store.ActiveLeases("image:" + store.AccountResource(item) + ":")
	if err != nil {
		return nil, err
	}
	if busy != 0 {
		return nil, errCredentialBusy
	}
	item, err = s.store.FindAccount(accountPublicRef(item))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	fields := cloneMap(updates)
	fields["credential_generation"] = intValue(item["credential_generation"]) + 1
	// Only a new RT clears a confirmed RT failure. A new AT remains usable
	// even when its unchanged RT is still invalid.
	if rt := stringValue(fields["refresh_token"]); rt != "" && rt != stringValue(item["refresh_token"]) {
		fields["refresh_token_invalid_at"] = nil
		fields["last_token_refresh_error"] = nil
	}
	for _, key := range []string{"refresh_uncertain", "refresh_inflight", "last_remote_check_result", "status_reason_code", "last_sync_error", "next_maintenance_at", "invalid_count"} {
		fields[key] = nil
	}
	if _, specified := fields["status"]; !specified {
		fields["status"] = "正常"
	}
	return credentialLease.UpdateAccount(ctx, item, fields)
}

func accountTestQuota(item map[string]any) string {
	if boolValue(item["image_quota_unknown"], false) || item["quota"] == nil {
		return "未知"
	}
	if boolValue(item["image_quota_unlimited"], false) {
		return "无限"
	}
	return fmt.Sprint(max(0, intValue(item["quota"])))
}

func (s *Server) testSelectedAccount(w http.ResponseWriter, r *http.Request, ref string) {
	var body struct {
		Mode   string `json:"mode"`
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Prompt = strings.TrimSpace(body.Prompt)
	spec, supported := model.Find(model.Catalog(), body.Model)
	if (body.Mode != "image" && body.Mode != "chat") || len([]rune(body.Prompt)) < 1 || len([]rune(body.Prompt)) > 8000 || len(body.Model) > 120 || !supported || !strings.HasPrefix(spec.OwnedBy, "openai") || (body.Mode == "image" && spec.Capability&model.Image == 0) || (body.Mode == "chat" && spec.Capability&model.Chat == 0) {
		writeError(w, 400, "invalid account test mode, model or prompt", "invalid_request_error")
		return
	}
	item, err := s.accountByRef(ref)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, 404, "account not found", "not_found")
		} else {
			writeError(w, 503, "读取账号失败", "storage_error")
		}
		return
	}
	started := time.Now()
	id := accountPublicRef(item)
	result := map[string]any{"account_id": id, "account_label": firstNonEmpty(stringValue(item["email"]), id), "mode": body.Mode, "mode_label": "对话", "model": body.Model, "quota_before_label": accountTestQuota(item), "quota_after_label": accountTestQuota(item), "quota_deducted": false, "content": "", "error_code": "", "error_message": ""}
	if body.Mode == "image" {
		result["mode_label"] = "画图"
	}
	configured, optionsErr := s.imageOptionsContext(r.Context())
	if optionsErr != nil {
		writeError(w, 503, "读取图片配置失败", "storage_error")
		return
	}
	ctx, cancel := context.WithTimeout(configured, s.imageTotalBudget(configured, 1))
	defer cancel()
	eligible := func(a accounts.Account) bool {
		if accountPublicRef(a.Fields) != id || !isOpenAIAccount(a) {
			return false
		}
		if body.Mode == "image" {
			return imageModelAccount(a, body.Model) && accounts.ImageEligible(a.Fields, false, time.Now(), s.imageMinimumValidity(ctx))
		}
		return accounts.CredentialReadiness(a.Fields, time.Now(), s.imageMinimumValidity(ctx)).State == "ready"
	}
	lease, e := s.accountPool.ReserveMatchingLimit(ctx, []string{"basic", "super", "heavy"}, nil, eligible, s.imagePolicy(ctx).AccountLimit)
	err = e
	if err == nil {
		defer s.accountPool.Release(lease)
		s.enrichMonitorAccount(r, lease.Account)
		if body.Mode == "chat" {
			result["content"], _, err = s.openAIChat.Complete(lease.Context, lease.Account, protocol.ChatRequest{Model: body.Model, Messages: []protocol.Message{{Role: "user", Content: body.Prompt}}})
		} else {
			deliveryID := newImportID()
			flowCtx := provider.WithImageCheckpoint(lease.Context, func(e provider.ImageCheckpoint) error {
				if e.Stage == "result_ready" {
					return s.store.ConsumeImageQuota(id, firstNonEmpty(e.ConversationID, deliveryID))
				}
				return nil
			})
			var images []provider.ImageResult
			if provider.IsCodexImageModel(body.Model) {
				images, err = s.openAIImage.GenerateCodex(flowCtx, lease.Account, body.Prompt, body.Model, "auto", "medium", nil)
			} else {
				images, err = s.openAIImage.Generate(flowCtx, lease.Account, body.Prompt, body.Model, "auto", "medium", nil)
			}
			if err == nil && len(images) == 0 {
				err = errors.New("image test returned no image")
			}
			if err == nil {
				value, url, e := s.resolveOpenAIImage(context.WithValue(flowCtx, imageDeliveryKey{}, deliveryID), lease.Account, images[0], "auto", "url", requestPublicBase(r))
				err = e
				if err == nil {
					result["content"] = "![image](" + value["url"] + ")"
					s.recordGeneratedMedia(ctx, map[string]string{"url": url})
				}
			}
		}
		if err != nil {
			if e := s.imageFailureFeedback(lease.Account, err); e != nil {
				err = fmt.Errorf("persist test failure: %w", e)
			}
		}
	}
	if latest, e := s.store.FindAccount(id); e == nil {
		result["quota_after_label"] = accountTestQuota(latest)
		result["quota_deducted"] = body.Mode == "image" && intValue(item["quota"]) > intValue(latest["quota"]) && !boolValue(item["image_quota_unknown"], false)
	}
	result["duration_ms"] = time.Since(started).Milliseconds()
	result["status"], result["status_label"], result["tone"] = "success", "成功", "success"
	if err != nil {
		_, code, _ := imageErrorCode(err)
		result["status"], result["status_label"], result["tone"] = "failed", "失败", "danger"
		result["error_code"] = code
		result["error_message"] = safeRefreshError(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, result)
}
