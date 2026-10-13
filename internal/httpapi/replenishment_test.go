package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRegistrationPartialOutputImportsWithoutProxyAndResumesOnce(t *testing.T) {
	python := os.Getenv("MIGRATION_TEST_PYTHON")
	if python == "" {
		t.Skip("MIGRATION_TEST_PYTHON not set")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			json.NewEncoder(w).Encode(map[string]any{"email": "registered@example.test"})
		case "/backend-api/conversation/init":
			json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 7}}})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer upstream.Close()
	root := t.TempDir()
	cfg := adminTestConfig(root)
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	t.Cleanup(func() { s.Close() })
	tool := filepath.Join(root, "fake-registration")
	os.MkdirAll(tool, 0700)
	os.WriteFile(filepath.Join(tool, "config.example.json"), []byte(`{"email_registration":{},"phone_reuse":{}}`), 0600)
	token := migrationTestJWT("registration")
	script := `import sys,json,pathlib,os
args=sys.argv
out=pathlib.Path(args[args.index('--output-dir')+1]); batch=args[args.index('--registration-batch-id')+1]
(out/'session_result.json').write_text(json.dumps({'access_token':TOKEN,'email':'registered@example.test','registration_batch_id':batch,'proxy':'http://bad-proxy.invalid:1234'}))
print('email=registered@example.test access_token='+TOKEN,flush=True)
sys.exit(1)
`
	script = strings.ReplaceAll(script, "TOKEN", strconv.Quote(token))
	os.WriteFile(filepath.Join(tool, "chatgpt_phone_reg.py"), []byte(script), 0600)
	t.Setenv("GO_REGISTRATION_PYTHON", python)
	t.Setenv("GO_REGISTRATION_TOOL_DIR", tool)
	doc, err := s.queueReplenishment(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.store.AcquireLease(context.Background(), "replenish-worker", time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s.runReplenishment(context.Background(), lease, doc)
	name := "replenishjob-" + stringValue(doc["id"])
	doc, _ = s.store.Document(name)
	if doc["status"] != "importing" || !boolValue(doc["partial_result"], false) {
		t.Fatalf("partial output not preserved: %v", doc)
	}
	importID := stringValue(doc["import_job_id"])
	s.runImportJob(context.Background(), importID)
	s.runReplenishment(context.Background(), lease, doc)
	doc, _ = s.store.Document(name)
	if doc["status"] != "completed" || intValue(doc["sync_processed"]) != 1 || intValue(doc["sync_success"]) != 1 {
		t.Fatalf("progress: %v", doc)
	}
	rows, err := s.store.AccountList()
	if err != nil || len(rows) != 1 {
		t.Fatalf("accounts %d %v", len(rows), err)
	}
	if stringValue(rows[0]["proxy"]) != "" || intValue(rows[0]["quota"]) != 7 {
		t.Fatal("registration proxy imported or quota missing")
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), token) {
		t.Fatal("registration progress leaked AT")
	}
	// Restart paths read the durable import job without launching Python again.
	os.Remove(filepath.Join(tool, "chatgpt_phone_reg.py"))
	doc["status"] = "importing"
	s.runReplenishment(context.Background(), lease, doc)
	after, _ := s.store.Document(name)
	if after["status"] != "completed" {
		t.Fatal("restart did not reconcile")
	}
}

func TestRegistrationCrashedWorkerCollectsExistingSessions(t *testing.T) {
	s := importTestServer(t)
	s.cfg.DataDir = t.TempDir()
	doc, err := s.queueReplenishment(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	id := stringValue(doc["id"])
	output := filepath.Join(s.replenishmentRoot(), id, "sessions")
	os.MkdirAll(output, 0700)
	raw, _ := json.Marshal(map[string]any{"access_token": migrationTestJWT("recovered"), "registration_batch_id": id})
	os.WriteFile(filepath.Join(output, "session_recovered.json"), raw, 0600)
	doc["status"] = "running"
	lease, err := s.store.AcquireLease(context.Background(), "replenish-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s.runReplenishment(context.Background(), lease, doc)
	after, _ := s.store.Document("replenishjob-" + id)
	if after["status"] != "importing" || !boolValue(after["partial_result"], false) {
		t.Fatalf("recovery %v", after)
	}
}
