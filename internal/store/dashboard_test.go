package store

import (
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
	"path/filepath"
	"testing"
	"time"
)

func TestPostgresDashboardIncludesEntireWindow(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"), testpg.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now().UTC().Truncate(time.Hour)
	_, err = s.pg.Exec(`INSERT INTO go_call_records(id,recorded_at,data) SELECT i::text,$1::timestamptz,jsonb_build_object('id',i::text,'type','call','time',$1::text,'detail',jsonb_build_object('status',CASE WHEN i%10=0 THEN 'failed' ELSE 'success' END,'raw_error',CASE WHEN i%10=0 THEN 'failure' ELSE '' END,'error','','model','gpt-image-2','duration_ms',100,'monitor',jsonb_build_object('metrics',jsonb_build_object('total_ms',999,'http_ttfb_ms',20)))) FROM generate_series(1,21000) i`, start)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.DashboardRecords(start, start.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	total, recent, failures := 0, 0, 0
	var duration float64
	for _, r := range rows {
		if r["_recent_only"] == true {
			recent++
			continue
		}
		c := int(r["_count"].(int))
		total += c
		d := r["detail"].(map[string]any)
		duration += d["duration_ms"].(float64)
		if d["error"] != "" {
			failures += c
		}
	}
	if total != 21000 || recent != 10 || failures != 2100 || duration != 2100000 {
		t.Fatalf("total=%d recent=%d failed=%d duration=%f", total, recent, failures, duration)
	}
	rows, err = s.DashboardRecords(start.Add(time.Hour), start.Add(2*time.Hour), time.Hour)
	if err != nil || len(rows) != 0 {
		t.Fatalf("outside window %v %v", rows, err)
	}
}
