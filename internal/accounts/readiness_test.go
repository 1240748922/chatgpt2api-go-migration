package accounts

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestStrictCredentialsAndUploadOnlyCooldown(t *testing.T) {
	now := time.Now()
	at := "h." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix()))) + ".s"
	item := map[string]any{"access_token": at, "quota": 4, "upload_cooldown_until": now.Add(time.Hour).Format(time.RFC3339)}
	if !ImageEligible(item, false, now, time.Minute) || ImageEligible(item, true, now, time.Minute) {
		t.Fatal("upload restriction leaked to text generation")
	}
	item["image_quota_exhausted_at"] = now.Add(-time.Hour).Format(time.RFC3339)
	if ImageEligible(item, false, now, time.Minute) {
		t.Fatal("time automatically restored quota")
	}
	if CredentialReadiness(map[string]any{"refresh_token": "rt.only"}, now, time.Minute).State != "unknown" {
		t.Fatal("RT-only incorrectly classified invalid")
	}
	item["refresh_uncertain"] = true
	if CredentialReadiness(item, now, time.Minute).State != "uncertain" {
		t.Fatal("uncertain credential admitted")
	}
}
