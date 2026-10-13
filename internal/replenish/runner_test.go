package replenish

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrationLogsRedactConfigAndCredentialValues(t *testing.T) {
	var out bytes.Buffer
	cfg := map[string]any{"proxy": map[string]any{"pool": []string{"proxy.invalid:8080:user:secret-password"}}, "mail": map[string]any{"api_key": "private-key-value"}}
	w := NewLogWriter(&out, cfg, nil)
	text := "account@example.test private-key-value proxy.invalid:8080:user:secret-password access_token=eyJheader.eyJclaims.signature refresh_token=rt.secret\n"
	_, _ = w.Write([]byte(text[:40]))
	_, _ = w.Write([]byte(text[40:]))
	w.Flush()
	for _, value := range []string{"private-key-value", "secret-password", "eyJheader", "rt.secret"} {
		if strings.Contains(out.String(), value) {
			t.Fatalf("secret leaked: %s", value)
		}
	}
	if !strings.Contains(out.String(), "account@example.test") {
		t.Fatal("account label was lost")
	}
}

func TestSessionsOnlyCollectCurrentBatchAndRegularFiles(t *testing.T) {
	root := t.TempDir()
	for name, batch := range map[string]string{"session_a.json": "current", "session_b.json": "other", "session_c.json": ""} {
		raw, _ := json.Marshal(map[string]any{"batch_id": batch, "access_token": "test-value"})
		if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, "session_invalid.json"), []byte("not json"), 0600)
	rows, err := Sessions(root, "current")
	if err != nil || len(rows) != 2 {
		t.Fatalf("sessions=%d err=%v", len(rows), err)
	}
}

func TestRegistrationCommandCannotOverrideOutputBoundary(t *testing.T) {
	s := Defaults()
	s.ExtraArgs = []string{"--output-dir=/outside"}
	if s.Validate() == nil {
		t.Fatal("accepted overridden output")
	}
	s.ExtraArgs = nil
	s.Source = "mailbox_file"
	s.MailboxFile = "mailboxes.txt"
	args := Command(s, 3, "entry.py", "output path", "batch")
	joined := strings.Join(args, "|")
	if !strings.Contains(joined, "--mailbox-file|mailboxes.txt") || !strings.Contains(joined, "--output-dir|output path") {
		t.Fatal(joined)
	}
}
