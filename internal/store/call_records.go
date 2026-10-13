package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Logs use a separate table: adding a request never invalidates account snapshots.
func (s *Store) AppendCallRecord(record map[string]any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		_, err = s.pg.ExecContext(ctx, `INSERT INTO go_call_records(id,recorded_at,data) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, stringValue(record["id"]), stringValue(record["time"]), raw)
		return err
	}
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	path := filepath.Join(filepath.Dir(s.accountsPath), "logs.jsonl")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func CallRecordMatches(record map[string]any, query url.Values) bool {
	detail, _ := record["detail"].(map[string]any)
	for _, key := range []string{"type", "status", "endpoint", "model", "account", "conversation_id", "id"} {
		want := strings.ToLower(strings.TrimSpace(query.Get(key)))
		if want == "" {
			continue
		}
		value := stringValue(detail[key])
		if key == "type" || key == "id" {
			value = stringValue(record[key])
		}
		if key == "account" {
			value = stringValue(detail["account_email"]) + " " + stringValue(detail["provider_account_id"])
		}
		if key == "id" && strings.ToLower(value) != want || key != "id" && !strings.Contains(strings.ToLower(value), want) {
			return false
		}
	}
	timestamp, _ := time.Parse(time.RFC3339Nano, stringValue(record["time"]))
	for _, key := range []string{"start_time", "end_time"} {
		boundary, err := time.Parse(time.RFC3339Nano, query.Get(key))
		if err != nil {
			continue
		}
		if key == "start_time" && timestamp.Before(boundary) || key == "end_time" && timestamp.After(boundary) {
			return false
		}
	}
	if search := strings.ToLower(strings.TrimSpace(query.Get("search"))); search != "" {
		raw, _ := json.Marshal(record)
		if !strings.Contains(strings.ToLower(string(raw)), search) {
			return false
		}
	}
	return true
}

func (s *Store) QueryCallRecords(query url.Values, limit, offset int) ([]map[string]any, int, error) {
	limit = max(0, limit)
	offset = max(0, offset)
	if s.pg == nil {
		s.logsMu.Lock()
		defer s.logsMu.Unlock()
		items, err := s.readLocalLogs()
		if err != nil {
			return nil, 0, err
		}
		filtered := []map[string]any{}
		for _, item := range items {
			if CallRecordMatches(item, query) {
				filtered = append(filtered, item)
			}
		}
		sort.SliceStable(filtered, func(i, j int) bool { return stringValue(filtered[i]["time"]) > stringValue(filtered[j]["time"]) })
		total := len(filtered)
		offset = min(offset, total)
		return filtered[offset:min(offset+limit, total)], total, nil
	}
	clauses := []string{"true"}
	args := []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	for _, key := range []string{"type", "status", "endpoint", "model", "account", "conversation_id", "id"} {
		want := strings.TrimSpace(query.Get(key))
		if want == "" {
			continue
		}
		expr := `data->'detail'->>'` + key + `'`
		if key == "type" || key == "id" {
			expr = `data->>'` + key + `'`
		}
		if key == "account" {
			expr = `concat(data->'detail'->>'account_email',' ',data->'detail'->>'provider_account_id')`
		}
		if key == "id" {
			add("id=$%d", want)
		} else {
			add("strpos(lower(coalesce("+expr+",'')),lower($%d))>0", want)
		}
	}
	if search := strings.TrimSpace(query.Get("search")); search != "" {
		add(`strpos(lower(data::text),lower($%d))>0`, search)
	}
	for _, key := range []string{"start_time", "end_time"} {
		boundary, err := time.Parse(time.RFC3339Nano, query.Get(key))
		if err != nil {
			continue
		}
		operator := ">="
		if key == "end_time" {
			operator = "<="
		}
		add("recorded_at"+operator+"$%d", boundary)
	}
	where := strings.Join(clauses, " AND ")
	ctx, cancel := dbContext()
	defer cancel()
	var total int
	if err := s.pg.QueryRowContext(ctx, `SELECT count(*) FROM go_call_records WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.pg.QueryContext(ctx, `SELECT data FROM go_call_records WHERE `+where+fmt.Sprintf(` ORDER BY recorded_at DESC,id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, 0, err
		}
		var item map[string]any
		if err = json.Unmarshal(raw, &item); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (s *Store) DeleteCallRecords(ids []string, before time.Time) (int, error) {
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		result, err := s.pg.ExecContext(ctx, `DELETE FROM go_call_records WHERE id=ANY($1::text[]) OR ($2::timestamptz IS NOT NULL AND recorded_at<$2)`, ids, optionalTime(before))
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		return int(n), err
	}
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	items, err := s.readLocalLogs()
	if err != nil {
		return 0, err
	}
	targets := map[string]bool{}
	for _, id := range ids {
		targets[id] = true
	}
	path := filepath.Join(filepath.Dir(s.accountsPath), "logs.jsonl")
	if len(items) == 0 {
		return 0, nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".logs-*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	enc := json.NewEncoder(f)
	removed := 0
	for _, item := range items {
		at, _ := time.Parse(time.RFC3339Nano, stringValue(item["time"]))
		if targets[stringValue(item["id"])] || !before.IsZero() && !at.IsZero() && at.Before(before) {
			removed++
			continue
		}
		if err = enc.Encode(item); err != nil {
			f.Close()
			return 0, err
		}
	}
	if err = f.Close(); err != nil {
		return 0, err
	}
	err = os.Rename(tmp, path)
	return removed, err
}
func optionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (s *Store) CallRetentionStats(before time.Time) (int, int64, error) {
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		var count int
		var size int64
		err := s.pg.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(octet_length(data::text)),0) FROM go_call_records WHERE recorded_at<$1`, before).Scan(&count, &size)
		return count, size, err
	}
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	items, err := s.readLocalLogs()
	if err != nil {
		return 0, 0, err
	}
	count := 0
	var size int64
	for _, item := range items {
		at, e := time.Parse(time.RFC3339Nano, stringValue(item["time"]))
		if e == nil && at.Before(before) {
			raw, e := json.Marshal(item)
			if e != nil {
				return 0, 0, e
			}
			count++
			size += int64(len(raw) + 1)
		}
	}
	return count, size, nil
}
func (s *Store) readLocalLogs() ([]map[string]any, error) {
	f, err := os.Open(filepath.Join(filepath.Dir(s.accountsPath), "logs.jsonl"))
	if os.IsNotExist(err) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	items := []map[string]any{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) == nil {
			items = append(items, item)
		}
	}
	return items, scanner.Err()
}
