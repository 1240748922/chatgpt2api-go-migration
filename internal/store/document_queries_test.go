package store

import (
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresDueQueueToleratesMalformedDateAndSkipsBackoff(t *testing.T) {
	s := pgTestStore(t, testpg.DSN(t))
	for _, row := range []struct{ name, retry string }{{"due-backoff", time.Now().Add(time.Hour).Format(time.RFC3339Nano)}, {"due-malformed", "not-a-timestamp"}, {"due-ready", ""}} {
		_, err := s.MutateDocument(row.name, func(d map[string]any) error { d["status"] = "queued"; d["next_retry_at"] = row.retry; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.PendingDueDocuments("due-", []string{"queued"}, 10)
	if err != nil || len(rows) != 2 {
		t.Fatal("due queue failed", rows, err)
	}
	for _, r := range rows {
		if r["document_name"] == "due-backoff" {
			t.Fatal("backoff item dispatched")
		}
	}
}
