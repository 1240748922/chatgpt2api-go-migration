package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func jwtExpiry(expiry time.Time) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix()))) + ".test"
}

func TestRejectedRTDoesNotInvalidateValidAT(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token_reused"}`))
	}))
	defer up.Close()
	s := importTestServer(t)
	s.cfg.OpenAIOAuthURL = up.URL
	for _, valid := range []bool{true, false} {
		id := fmt.Sprint(valid)
		exp := time.Now().Add(-time.Hour)
		if valid {
			exp = time.Now().Add(time.Hour)
		}
		_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"id": id, "access_token": jwtExpiry(exp), "refresh_token": "rt." + id, "source_type": "chatgpt_web", "last_remote_check_result": "valid", "quota": 5}})
		if err != nil {
			t.Fatal(err)
		}
		a, _ := s.store.FindAccount(id)
		if _, err = s.syncCredential(context.Background(), a, true); err == nil {
			t.Fatal("bad RT succeeded")
		}
		a, err = s.store.FindAccount(id)
		if err != nil || stringValue(a["refresh_token_invalid_at"]) == "" || boolValue(a["refresh_uncertain"], false) {
			t.Fatal("rejected RT state not persisted", err)
		}
		state := accounts.CredentialReadiness(a, time.Now(), 5*time.Minute).State
		if valid && state != "ready" || !valid && state != "invalid" {
			t.Fatalf("valid=%v state=%s", valid, state)
		}
	}
}

func TestAdmissionMaintenanceAndDisplayedReadinessAgree(t *testing.T) {
	s := importTestServer(t)
	minimum := s.configuredImageMinimumValidity()
	if minimum <= 5*time.Minute {
		t.Fatal("generation budget omitted")
	}
	a := map[string]any{"access_token": jwtExpiry(time.Now().Add(7 * time.Minute)), "refresh_token": "rt.test", "quota": 9}
	if accounts.ImageEligible(a, false, time.Now(), minimum) {
		t.Fatal("near-expiry credential admitted")
	}
	api := accountForAPI(a, minimum)
	if api["credential_readiness"].(accounts.Readiness).State != "expiring" {
		t.Fatal("display contradicts admission")
	}
	if next := nextCredentialMaintenance(a, time.Now(), minimum); next.After(time.Now().Add(time.Minute)) {
		t.Fatal("near-expiry refresh deferred", next)
	}
}

func TestQuotaCleanupRequiresConfirmedExhaustion(t *testing.T) {
	for _, a := range []map[string]any{{"quota": 0}, {"quota": 0, "image_quota_unknown": true, "last_remote_check_result": "valid"}, {"quota": 0, "upload_cooldown_until": "2099-01-01T00:00:00Z"}, {"quota": 0, "last_error_status": 429}, {"quota": 0, "image_quota_unlimited": true, "last_remote_check_result": "valid"}} {
		if accountQuotaExhausted(a) {
			t.Fatal("temporary/unknown account selected for deletion", a)
		}
	}
	if !accountQuotaExhausted(map[string]any{"quota": 0, "last_remote_check_result": "valid"}) {
		t.Fatal("confirmed zero quota omitted")
	}
}
