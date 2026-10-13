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
