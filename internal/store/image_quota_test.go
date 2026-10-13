package store

import (
	"context"
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
	"sync"
	"testing"
	"time"
)

func TestPostgresImageReceiptsAreAtomicAcrossInstances(t *testing.T) {
	dsn := testpg.DSN(t)
	a, b := pgTestStore(t, dsn), pgTestStore(t, dsn)
	_, _, _, err := a.AddAccounts(nil, []map[string]any{{"id": "image-account", "access_token": "test-at", "quota": 20}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			errs <- s.ConsumeImageQuota("image-account", "same-conversation")
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	account, err := b.FindAccount("image-account")
	if err != nil || quotaNumber(account["quota"]) != 19 || quotaNumber(account["image_generated_count"]) != 1 {
		t.Fatalf("duplicate receipt: %#v %v", account, err)
	}
	for _, receipt := range []string{"second", "third"} {
		if err = a.ConsumeImageQuota("image-account", receipt); err != nil {
			t.Fatal(err)
		}
	}
	account, _ = b.FindAccount("image-account")
	if quotaNumber(account["quota"]) != 17 {
		t.Fatalf("distinct receipts: %#v", account)
	}
}

func TestPostgresExpiredWorkerCannotOverwriteTask(t *testing.T) {
	dsn := testpg.DSN(t)
	a, b := pgTestStore(t, dsn), pgTestStore(t, dsn)
	old, err := a.AcquireLease(context.Background(), "task:test", time.Minute)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	defer old.Release()
	_, err = old.MutateDocument("imagetask-test", func(d map[string]any) error { d["status"] = "running"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.pg.Exec(`UPDATE go_leases SET expires_at=now()-interval '1 second' WHERE resource='task:test'`); err != nil {
		t.Fatal(err)
	}
	fresh, err := b.AcquireLease(context.Background(), "task:test", time.Minute)
	if err != nil || fresh == nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	_, err = fresh.MutateDocument("imagetask-test", func(d map[string]any) error { d["status"] = "success"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.MutateDocument("imagetask-test", func(d map[string]any) error { d["status"] = "error"; return nil }); err == nil {
		t.Fatal("expired worker wrote task")
	}
	doc, _ := a.Document("imagetask-test")
	if doc["status"] != "success" {
		t.Fatalf("task overwritten: %#v", doc)
	}
}
