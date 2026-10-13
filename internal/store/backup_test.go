package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresBackupRestoresSnapshotAndRefusesOverwrite(t *testing.T) {
	source := pgTestStore(t, testpg.DSN(t))
	target := pgTestStore(t, testpg.DSN(t))
	value := "single ' quote, backslash \\, 中文\nsecond line"
	_, _, _, err := source.AddAccounts(nil, []map[string]any{{"id": "one", "access_token": "synthetic-at", "refresh_token": "synthetic-rt", "email": value, "quota": 7}})
	if err != nil {
		t.Fatal(err)
	}
	if err = source.ConsumeImageQuota("one", "receipt"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.PatchConfig(map[string]any{"label": value}); err != nil {
		t.Fatal(err)
	}
	if _, err = source.MutateDocument("imagetask-one", func(d map[string]any) error { d["status"] = "result_ready"; d["value"] = value; return nil }); err != nil {
		t.Fatal(err)
	}
	lease, err := source.AcquireLease(context.Background(), "test-lease", time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	defer lease.Release()
	var out bytes.Buffer
	if err = source.WritePostgresBackup(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "INSERT INTO go_leases") {
		t.Fatal("backup restored runtime ownership")
	}
	conn, err := target.pg.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), out.String()); err != nil {
		t.Fatal(err)
	}
	item, err := target.FindAccount("one")
	if err != nil || item["email"] != value || item["refresh_token"] != "synthetic-rt" || quotaNumber(item["quota"]) != 6 {
		t.Fatalf("bad restored account: %#v %v", item, err)
	}
	if err = target.ConsumeImageQuota("one", "receipt"); err != nil {
		t.Fatal(err)
	}
	item, _ = target.FindAccount("one")
	if quotaNumber(item["quota"]) != 6 {
		t.Fatal("restored receipt charged twice")
	}
	doc, err := target.Document("imagetask-one")
	if err != nil || doc["value"] != value {
		t.Fatalf("bad restored document: %#v %v", doc, err)
	}
	cfg, err := target.Config()
	if err != nil || cfg["label"] != value {
		t.Fatal("missing configuration")
	}
	if _, err = conn.ExecContext(context.Background(), out.String()); err == nil {
		t.Fatal("restore overwrote populated database")
	}
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	item, err = target.FindAccount("one")
	if err != nil || quotaNumber(item["quota"]) != 6 {
		t.Fatal("failed restore changed existing account")
	}
}

func TestPostgresBackupExcludesImagePayloadsWhenTasksDisabled(t *testing.T) {
	source := pgTestStore(t, testpg.DSN(t))
	target := pgTestStore(t, testpg.DSN(t))
	for _, name := range []string{"imagetask-one", "imagepayload-one", "imageresult-one", "filepayload-one", "fileexport-one", "editable-one", "retained-one"} {
		if _, err := source.MutateDocument(name, func(d map[string]any) error { d["value"] = "fixture"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := source.WritePostgresBackupIncluded(context.Background(), &out, map[string]bool{"image_tasks": false}); err != nil {
		t.Fatal(err)
	}
	conn, err := target.pg.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), out.String()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = target.pg.QueryRow(`SELECT count(*) FROM go_documents`).Scan(&count); err != nil || count != 1 {
		t.Fatal("excluded image payloads restored", count, err)
	}
}
