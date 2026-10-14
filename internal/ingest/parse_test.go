package ingest

import "testing"

func TestCredentialImportUsesKeysAndDiscardsProxy(t *testing.T) {
	rows, err := Parse(`{"accounts":[{"accessToken":"opaque-at","refresh_token":"opaque-rt","user":{"email":"a@example.test"},"proxy":"http://bad","quota":100},{"refreshToken":"rt.only"},{"access_token":"opaque-at","refresh_token":"opaque-rt"}]}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["access_token"] != "opaque-at" || rows[0]["refresh_token"] != "opaque-rt" || rows[0]["email"] != "a@example.test" {
		t.Fatalf("bad pairing: %#v", rows)
	}
	if rows[0]["proxy"] != nil || rows[0]["quota"] != nil || rows[0]["image_quota_unknown"] != true {
		t.Fatal("untrusted metadata imported")
	}
	for _, input := range []string{`{"access_token":`, `{"proxy":"x"}`} {
		if _, err := Parse(input, ""); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}

func TestImportTextAndJSONLines(t *testing.T) {
	rows, err := Parse("\ufeffopaque-rt\nopaque-rt\nsecond", "refresh_token")
	if err != nil || len(rows) != 2 || rows[0]["refresh_token"] != "opaque-rt" {
		t.Fatalf("explicit RT mode: %v %v", rows, err)
	}
	rows, err = Parse("{\"access_token\":\"at-a\"}\n{\"refresh_token\":\"rt-b\"}", "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("JSON lines: %v %v", rows, err)
	}
}

func TestCodexSourceIsNotAccountPlan(t *testing.T) {
	rows, err := Parse(`{"type":"codex","access_token":"synthetic","plan_type":"plus","proxy":"http://discard"}`, "")
	if err != nil || len(rows) != 1 || rows[0]["source_type"] != "codex" || rows[0]["type"] != nil || rows[0]["plan_type"] != "plus" {
		t.Fatalf("Codex import: %v %v", rows, err)
	}
}

func TestChatGPTSessionJSONKeepsIdentityWithoutStaleRuntimeState(t *testing.T) {
	rows, err := Parse(`{"accessToken":"session-at","user":{"email":"session@example.test"},"expires":"2099-01-01T00:00:00Z","proxy":"http://old-proxy","quota":999}`, "session_json")
	if err != nil || len(rows) != 1 {
		t.Fatalf("session JSON parse: %v %#v", err, rows)
	}
	row := rows[0]
	if row["access_token"] != "session-at" || row["email"] != "session@example.test" || row["source_type"] != "chatgpt_web" {
		t.Fatalf("session identity lost: %#v", row)
	}
	if row["proxy"] != nil || row["quota"] != nil || row["expires"] != nil {
		t.Fatalf("unverified runtime state imported: %#v", row)
	}
}

func TestRegistrationSessionPrefersOpenAIRTAndKeepsRecoveryCredentials(t *testing.T) {
	rows, err := Parse(`{"email":"registered@example.test","access_token":"registered-at","refresh_token":"mailbox-rt","oauth_refresh_token":"openai-rt","auth_session":{"accessToken":"registered-at","refreshToken":"openai-rt"},"mailbox":{"refresh_token":"mailbox-rt"},"password":"login-password","totp_secret":"2fa-secret","session_token":"session-cookie","proxy":"http://old-proxy","quota":999}`, "session_json")
	if err != nil || len(rows) != 1 {
		t.Fatalf("registration JSON parse: %v %#v", err, rows)
	}
	row := rows[0]
	if row["refresh_token"] != "openai-rt" || row["login_password"] != "login-password" || row["two_factor_secret"] != "2fa-secret" || row["cookie_header"] != "__Secure-next-auth.session-token=session-cookie" {
		t.Fatalf("registration credential mismatch: %#v", row)
	}
	if row["proxy"] != nil || row["quota"] != nil {
		t.Fatalf("unverified runtime state imported: %#v", row)
	}
	rows, err = Parse(`{"access_token":"registered-at","refresh_token":"mailbox-rt","refresh_token_status":"legacy_present","auth_session":{},"mailbox":{"refresh_token":"mailbox-rt"}}`, "session_json")
	if err != nil || len(rows) != 1 || rows[0]["refresh_token"] != "" {
		t.Fatalf("mailbox RT imported as OpenAI RT: %v %#v", err, rows)
	}
}

func TestNestedAuthSessionJSON(t *testing.T) {
	rows, err := Parse(`{"email":"nested@example.test","auth_session":{"accessToken":"nested-at","refreshToken":"nested-rt","idToken":"nested-id"}}`, "session_json")
	if err != nil || len(rows) != 1 || rows[0]["access_token"] != "nested-at" || rows[0]["refresh_token"] != "nested-rt" || rows[0]["id_token"] != "nested-id" || rows[0]["email"] != "nested@example.test" {
		t.Fatalf("nested auth session: %v %#v", err, rows)
	}
}

func TestSessionResponseAndTopLevelRefreshToken(t *testing.T) {
	rows, err := Parse(`{"response":{"session":{"accessToken":"response-at","refreshToken":"response-rt","user":{"email":"response@example.test"}}}}`, "session_json")
	if err != nil || len(rows) != 1 || rows[0]["access_token"] != "response-at" || rows[0]["refresh_token"] != "response-rt" || rows[0]["email"] != "response@example.test" {
		t.Fatalf("response session: %v %#v", err, rows)
	}
	rows, err = Parse(`{"auth_session":{"accessToken":"auth-at"},"refresh_token":"root-rt"}`, "session_json")
	if err != nil || len(rows) != 1 || rows[0]["refresh_token"] != "root-rt" {
		t.Fatalf("ordinary auth session lost RT: %v %#v", err, rows)
	}
}

func TestSessionImportRejectsPlainTextAndMailboxOnly(t *testing.T) {
	for _, input := range []string{"not-json", `{"mailbox":{"refresh_token":"mailbox-rt"}}`} {
		if rows, err := Parse(input, "session_json"); err == nil {
			t.Fatalf("accepted invalid session data: %#v", rows)
		}
	}
}
