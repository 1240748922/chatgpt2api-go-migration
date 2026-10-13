package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

var errCredentialBusy = errors.New("credential maintenance is already running")
var errCredentialUncertain = errors.New("refresh result is uncertain; reimport current credentials or sign in again")

// Every route that can rotate an RT uses this coordinator, including import workers.
func (s *Server) syncCredential(parent context.Context, input map[string]any, force bool) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	resource := "credential:" + store.AccountResource(input)
	lease, err := s.store.AcquireLease(ctx, resource, 120*time.Second)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, errCredentialBusy
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 120*time.Second, cancel)
	useLease, e := s.store.AcquireLease(ctx, "account-use:"+store.AccountResource(input), 120*time.Second)
	if e != nil {
		return nil, e
	}
	if useLease == nil {
		return nil, errCredentialBusy
	}
	defer useLease.Release()
	useLease.KeepAlive(ctx, 120*time.Second, cancel)
	busy, e := s.store.ActiveLeases("image:" + store.AccountResource(input) + ":")
	if e != nil {
		return nil, e
	}
	if busy > 0 {
		return input, errCredentialBusy
	}
	ref := firstNonEmpty(stringValue(input["id"]), accountToken(input))
	active, err := s.store.FindAccount(ref)
	if err != nil {
		return nil, err
	}
	if boolValue(active["refresh_uncertain"], false) {
		return active, errCredentialUncertain
	}
	rt := stringValue(active["refresh_token"])
	// The same RT can appear in old JSON imports under more than one AT.
	if rt != "" {
		hash := sha256.Sum256([]byte(rt))
		rtLease, e := s.store.AcquireLease(ctx, "rt:"+hex.EncodeToString(hash[:]), 120*time.Second)
		if e != nil {
			return nil, e
		}
		if rtLease == nil {
			return active, errCredentialBusy
		}
		defer rtLease.Release()
		rtLease.KeepAlive(ctx, 120*time.Second, cancel)
	}
	client := s.openAIAccountClient()
	minimum := s.configuredImageMinimumValidity()
	client.MinimumValidity = minimum + time.Minute
	defer client.Close()
	sent, rotated := false, false
	client.OnRefreshStart = func() error {
		if stringValue(active["refresh_token_invalid_at"]) != "" {
			return errors.New("refresh credential confirmed invalid")
		}
		if sent {
			return errors.New("duplicate refresh blocked")
		}
		sent = true
		next, e := lease.UpdateAccount(ctx, active, map[string]any{"refresh_uncertain": true, "refresh_inflight": true, "refresh_started_at": time.Now().UTC().Format(time.RFC3339)})
		if e == nil {
			active = next
		}
		return e
	}
	client.OnTokenRotation = func(result provider.AccountRefreshResult) error {
		fields := map[string]any{"credential_generation": intValue(active["credential_generation"]) + 1, "refresh_uncertain": false, "refresh_inflight": false, "refresh_token_invalid_at": nil, "last_remote_check_result": nil, "status": "正常"}
		// A missing replacement RT is not evidence that the consumed RT is reusable.
		if result.RefreshToken == "" {
			fields["refresh_token"] = ""
		}
		next, e := lease.UpdateAccount(ctx, active, store.RotationUpdates(result.AccessToken, result.RefreshToken, result.IDToken, fields))
		if e == nil {
			active = next
			rotated = true
		}
		return e
	}
	var result provider.AccountRefreshResult
	if force {
		result, err = client.RefreshAccessToken(ctx, active)
	} else {
		result, err = client.RefreshAccount(ctx, active)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		updates := map[string]any{"refresh_inflight": false, "last_sync_at": now, "last_sync_error": safeRefreshError(err), "next_maintenance_at": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)}
		updates["last_refresh_warning"] = safeRefreshError(err)
		updates["last_refresh_warning_at"] = now
		var oe *provider.OAuthRefreshError
		if errors.As(err, &oe) {
			updates["refresh_uncertain"] = oe.Uncertain
			lower := strings.ToLower(oe.Message)
			if !oe.Uncertain && (oe.Status == 400 || oe.Status == 401) && (strings.Contains(lower, "refresh_token_reused") || strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "invalid_refresh_token") || strings.Contains(lower, "refresh_token_invalidated")) {
				updates["refresh_token_invalid_at"] = now
				updates["last_token_refresh_error"] = safeRefreshError(err)
				// Refresh bookkeeping must not turn a still usable AT into an
				// invalid account merely because its RT was rejected.
				expiry, known := accounts.TokenExpiry(stringValue(active["access_token"]))
				if known && !expiry.After(time.Now()) {
					updates["status"] = "异常"
					updates["last_remote_check_result"] = "invalid"
				}
			}
		} else if sent && !rotated {
			updates["refresh_uncertain"] = true
		}
		if errors.Is(err, provider.ErrInvalidAccessToken) {
			updates["last_remote_check_result"] = "pending"
			updates["status_reason_code"] = "auth_pending"
		}
		_, e := lease.UpdateAccount(ctx, active, updates)
		if e != nil {
			return active, fmt.Errorf("save credential failure state: %w", e)
		}
		return active, err
	}
	updates := cloneMap(result.Fields)
	for _, k := range []string{"invalid_count", "last_error_kind", "last_error_status", "last_error_message", "last_error_at", "status_reason_code", "cooldown_until", "last_token_refresh_error"} {
		updates[k] = nil
	}
	updates["last_sync_at"] = now
	updates["last_sync_error"] = nil
	updates["refresh_inflight"] = false
	updates["last_remote_check_result"] = "valid"
	updates["last_refresh_error"] = nil
	updates["last_refresh_warning"] = nil
	updates["last_refresh_warning_at"] = nil
	updates["next_maintenance_at"] = nextCredentialMaintenance(active, time.Now(), minimum).UTC().Format(time.RFC3339)
	if boolValue(updates["image_quota_unknown"], false) == false && intValue(updates["quota"]) > 0 {
		updates["image_quota_recovered_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		updates["image_quota_cooldown_until"] = nil
	}
	return lease.UpdateAccount(ctx, active, updates)
}

func nextCredentialMaintenance(item map[string]any, now time.Time, minimum time.Duration) time.Time {
	next := now.Add(15 * time.Minute)
	if expiry, known := accounts.TokenExpiry(stringValue(item["access_token"])); known {
		if renewAt := expiry.Add(-minimum - time.Minute); renewAt.Before(next) {
			next = renewAt
		}
	}
	if next.Before(now.Add(30 * time.Second)) {
		next = now.Add(30 * time.Second)
	}
	return next
}
