package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DashboardRecords aggregates the entire requested period in the database.
// Raw prompts, inputs, attempts and SSE traces never travel through the overview.
func (s *Store) DashboardRecords(start, end time.Time, step time.Duration) ([]map[string]any, error) {
	if s.pg == nil {
		s.logsMu.Lock()
		defer s.logsMu.Unlock()
		return s.readLocalLogs()
	}
	ctx, cancel := dbContext()
	defer cancel()
	number := func(keys ...string) string {
		parts := []string{}
		for _, key := range keys {
			for _, prefix := range []string{"d", "d->'monitor'->'metrics'", "d->'monitor'->'perf'", "d->'monitor'"} {
				p := prefix + "->>'" + key + "'"
				parts = append(parts, fmt.Sprintf("nullif(CASE WHEN %s ~ '^[0-9]+([.][0-9]+)?$' AND length(%s)<20 THEN (%s)::double precision ELSE 0 END,0)", p, p, p))
			}
		}
		return "coalesce(" + strings.Join(parts, ",") + ",0)"
	}
	duration := number("duration_ms", "total_ms")
	ttfb := number("http_ttfb_ms", "sse_first_event_ms", "stream_first_queue_ms")
	rows, err := s.pg.QueryContext(ctx, `WITH records AS (SELECT recorded_at,data->'detail' AS d FROM go_call_records WHERE recorded_at>=$1 AND recorded_at<$2 AND data->>'type'='call'), samples AS (SELECT floor(extract(epoch FROM(recorded_at-$1))/$3)::int AS bucket,coalesce(d->>'endpoint','') AS endpoint,coalesce(d->>'model','') AS model,lower(coalesce(d->>'status','')) AS status,coalesce(nullif(d->>'failure_code',''),d->>'error_code','') AS code,coalesce(d->>'status_code','') AS http_status,coalesce(nullif(d->>'error',''),d->>'raw_error','')<>'' AS has_error,`+duration+` AS duration,`+ttfb+` AS ttfb FROM records) SELECT bucket,endpoint,model,status,code,http_status,has_error,count(*),coalesce(sum(duration),0),count(*) FILTER(WHERE duration>0),coalesce(sum(ttfb),0),count(*) FILTER(WHERE ttfb>0) FROM samples GROUP BY bucket,endpoint,model,status,code,http_status,has_error`, start, end, step.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var bucket, count, dc, tc int
		var endpoint, model, status, code, httpStatus string
		var hasError bool
		var ds, ts float64
		if err = rows.Scan(&bucket, &endpoint, &model, &status, &code, &httpStatus, &hasError, &count, &ds, &dc, &ts, &tc); err != nil {
			return nil, err
		}
		errorText := ""
		if hasError {
			errorText = "upstream error"
		}
		out = append(out, map[string]any{"type": "call", "time": start.Add(time.Duration(bucket) * step).Format(time.RFC3339Nano), "_count": count, "_duration_count": dc, "_ttfb_count": tc, "detail": map[string]any{"started_at": start.Add(time.Duration(bucket) * step).Format(time.RFC3339Nano), "endpoint": endpoint, "model": model, "status": status, "error_code": code, "status_code": quotaNumber(httpStatus), "error": errorText, "duration_ms": ds, "monitor": map[string]any{"metrics": map[string]any{"http_ttfb_ms": ts}}}})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	recent, err := s.pg.QueryContext(ctx, `SELECT data FROM go_call_records WHERE recorded_at>=$1 AND recorded_at<$2 AND data->>'type'='call' AND (lower(data->'detail'->>'status') IN ('failed','error','fail','cancelled') OR coalesce(nullif(data->'detail'->>'error',''),data->'detail'->>'raw_error','')<>'' OR coalesce(data->'detail'->>'status_code','') ~ '^[45][0-9][0-9]$') ORDER BY recorded_at DESC,id DESC LIMIT 10`, start, end)
	if err != nil {
		return nil, err
	}
	defer recent.Close()
	for recent.Next() {
		var raw []byte
		if err = recent.Scan(&raw); err != nil {
			return nil, err
		}
		var d map[string]any
		if err = json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		d["_recent_only"] = true
		out = append(out, d)
	}
	return out, recent.Err()
}
