package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/store"
	"os"
	"time"
)

func accountQuotaExhausted(a map[string]any) bool {
	if a["quota"] == nil || boolValue(a["image_quota_unknown"], false) || boolValue(a["image_quota_unlimited"], false) || intValue(a["quota"]) > 0 {
		return false
	}
	return stringValue(a["last_remote_check_result"]) == "valid" || stringValue(a["status_reason_code"]) == "image_quota_exhausted" || stringValue(a["image_quota_exhausted_at"]) != ""
}

func (s *Server) removeAccountSafely(parent context.Context, input map[string]any, invalid, exhausted bool) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, "credential:"+store.AccountResource(input), 30*time.Second)
	if err != nil || lease == nil {
		return false, err
	}
	defer lease.Release()
	gate, err := s.store.AcquireLease(ctx, "account-use:"+store.AccountResource(input), 30*time.Second)
	if err != nil || gate == nil {
		return false, err
	}
	defer gate.Release()
	busy, err := s.store.ActiveLeases("image:" + store.AccountResource(input) + ":")
	if err != nil || busy > 0 {
		return false, err
	}
	a, err := s.store.FindAccount(firstNonEmpty(stringValue(input["id"]), accountToken(input)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !(invalid && accountAutoRemoveInvalid(a) || exhausted && accountQuotaExhausted(a)) {
		return false, nil
	}
	err = lease.DeleteAccount(ctx, a)
	if errors.Is(err, store.ErrCredentialsChanged) || errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
