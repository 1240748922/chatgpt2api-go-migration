package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/ingest"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

const importChunkSize = 200

func importDocument(id string) string         { return "importjob-" + id }
func importChunk(id string, index int) string { return fmt.Sprintf("importpayload-%s-%05d", id, index) }
func (s *Server) accountImportJobs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := strings.TrimPrefix(r.URL.Path, "/api/account-import-jobs")
	id = strings.Trim(id, "/")
	if id != "" {
		parts := strings.Split(id, "/")
		id = parts[0]
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			writeError(w, 404, "task not found", "not_found")
			return
		}
		doc, err := s.store.Document(importDocument(id))
		if err != nil || doc["id"] == nil {
			writeError(w, 404, "task not found", "not_found")
			return
		}
		if len(parts) > 1 && parts[1] == "retry" && r.Method == http.MethodPost {
			if doc["status"] == "running" || doc["status"] == "queued" {
				writeError(w, 409, "导入任务尚未结束", "task_running")
				return
			}
			rows, blocked, err := s.importRetryRows(id, doc)
			if err != nil {
				writeError(w, 503, "读取重试账号失败", "storage_error")
				return
			}
			if len(rows) == 0 {
				writeError(w, 409, "没有可安全重试的账号；续期结果不确定或 RT 已失效的账号需重新导入最新凭据", "credential_uncertain")
				return
			}
			job, err := s.createImportJob(rows, true)
			if err != nil {
				writeError(w, 503, "保存重试任务失败", "storage_error")
				return
			}
			job["blocked_retry_count"] = blocked
			writeJSON(w, 202, job)
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed", "invalid_request_error")
			return
		}
		if len(parts) > 1 && parts[1] == "events" {
			offset := queryInt(r, "offset", 0, 0, 50000)
			limit := queryInt(r, "limit", 100, 1, 200)
			events, next, err := s.importEvents(id, doc, offset, limit)
			if err != nil {
				writeError(w, 503, "读取导入日志失败", "storage_error")
				return
			}
			writeJSON(w, 200, map[string]any{"events": events, "total": intValue(doc["event_count"]), "next_offset": next, "rows_total": intValue(doc["total"])})
			return
		}
		writeJSON(w, 200, doc)
		return
	}
	switch r.Method {
	case http.MethodGet:
		jobs, e := s.store.ListDocuments("importjob-", "", nil, 50)
		if e != nil {
			writeError(w, 500, "读取导入任务失败", "storage_error")
			return
		}

		writeJSON(w, 200, map[string]any{"jobs": jobs})
	case http.MethodPost:
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			writeError(w, 400, "invalid JSON", "invalid_request_error")
			return
		}
		text := firstNonEmpty(stringValue(body["text"]), stringValue(body["content"]))
		if text == "" && body["accounts"] != nil {
			raw, e := marshalImport(body["accounts"])
			if e != nil {
				writeError(w, 400, "invalid accounts", "invalid_request_error")
				return
			}
			text = raw
		}
		rows, e := ingest.Parse(text, stringValue(body["mode"]))
		if e != nil {
			writeError(w, 400, e.Error(), "invalid_credentials")
			return
		}
		doc, e := s.createImportJob(rows, false)
		if e != nil {
			writeError(w, 503, "保存导入任务失败", "storage_error")
			return
		}
		writeJSON(w, 202, doc)
	default:
		writeError(w, 405, "method not allowed", "invalid_request_error")
	}
}

// Retry refers to the current account, never the RT copied into an old import file.
func (s *Server) importRetryRows(id string, doc map[string]any) ([]map[string]any, int, error) {
	rows := []map[string]any{}
	blocked := 0
	seen := map[string]bool{}
	for i := 0; i < intValue(doc["chunks"]); i++ {
		chunk, err := s.store.Document(importChunk(id, i))
		if err != nil {
			return nil, 0, err
		}
		for _, value := range mapValue(chunk["results"]) {
			result := mapValue(value)
			ref := stringValue(result["account_id"])
			if result["status"] != "failed" || seen[ref] {
				continue
			}
			item, err := s.store.FindAccount(ref)
			if err != nil || boolValue(item["refresh_uncertain"], false) || boolValue(item["refresh_inflight"], false) || stringValue(item["refresh_token_invalid_at"]) != "" {
				blocked++
				continue
			}
			seen[ref] = true
			rows = append(rows, map[string]any{"id": newImportID(), "account_id": ref, "email": item["email"]})
		}
	}
	return rows, blocked, nil
}

func (s *Server) createImportJob(rows []map[string]any, retry bool) (map[string]any, error) {
	return s.createImportJobWithID(rows, retry, newImportID())
}
func (s *Server) createImportJobWithID(rows []map[string]any, retry bool, id string) (map[string]any, error) {
	chunks := (len(rows) + importChunkSize - 1) / importChunkSize
	for i := 0; i < chunks; i++ {
		end := min((i+1)*importChunkSize, len(rows))
		_, e := s.store.MutateDocument(importChunk(id, i), func(d map[string]any) error {
			d["accounts"] = rows[i*importChunkSize : end]
			order := []string{}
			for _, row := range rows[i*importChunkSize : end] {
				order = append(order, stringValue(row["id"]))
			}
			d["result_order"] = order
			d["total"] = end - i*importChunkSize
			if retry {
				d["imported"] = true
				d["skipped"] = end - i*importChunkSize
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	doc, e := s.store.MutateDocument(importDocument(id), func(d map[string]any) error {
		for k, v := range map[string]any{"id": id, "status": "queued", "created_at": now, "updated_at": now, "total": len(rows), "chunks": chunks, "import_processed": 0, "sync_processed": 0, "sync_total": len(rows), "added": 0, "skipped": 0, "sync_success": 0, "sync_failed": 0, "events": []any{}, "event_count": 0} {
			d[k] = v
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return doc, nil
}
func (s *Server) importJobScheduler() {
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.probeStop:
			return
		case <-timer.C:
		}
		lease, e := s.store.AcquireLease(s.backgroundCtx, "import-dispatch", 120*time.Second)
		if e != nil || lease == nil {
			continue
		}
		ctx, cancel := context.WithCancel(s.backgroundCtx)
		lease.KeepAlive(ctx, 120*time.Second, cancel)
		docs, e := s.store.PendingDocuments("importjob-", []string{"queued", "running"}, 1)
		if e == nil {
			for _, d := range docs {
				s.runImportJob(ctx, stringValue(d["id"]))
			}
		}
		cancel()
		lease.Release()
	}
}
func (s *Server) runImportJob(parent context.Context, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	lease, e := s.store.AcquireLease(ctx, "import:"+id, 120*time.Second)
	if e != nil || lease == nil {
		return
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 120*time.Second, cancel)
	doc, e := s.store.Document(importDocument(id))
	if e != nil || doc["status"] == "completed" || doc["status"] == "failed" {
		return
	}
	_, e = lease.MutateDocument(importDocument(id), func(d map[string]any) error { d["status"] = "running"; return nil })
	if e != nil {
		return
	}
	if err := s.importJobChunks(ctx, lease, id, doc); err != nil {
		if ctx.Err() == nil {
			s.importFailure(lease, id, err)
		}
		return
	}
	if !s.reconcileImportProgressWithLease(lease, id, true) {
		return
	}
	for i := 0; i < intValue(doc["chunks"]); i++ {
		if ctx.Err() != nil {
			return
		}
		chunk, e := s.store.Document(importChunk(id, i))
		if e != nil {
			return
		}
		if boolValue(chunk["done"], false) {
			continue
		}
		rows := importRows(chunk["accounts"])
		queue := make(chan map[string]any)
		var wg sync.WaitGroup
		for n := 0; n < 4; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for row := range queue {
					key := stringValue(row["id"])
					check, e := s.store.Document(importChunk(id, i))
					if e != nil {
						cancel()
						return
					}
					if mapValue(check["results"])[key] != nil {
						continue
					}
					item, err := s.store.FindCredential(row)
					if err == nil {
						_, err = s.syncCredential(ctx, item, false)
					}
					if errors.Is(err, errCredentialBusy) {
						continue
					}
					label := stringValue(row["email"])
					if item != nil {
						label = firstNonEmpty(stringValue(item["email"]), label)
					}
					if label == "" {
						label = "账号 " + key[len(key)-8:]
					}
					result := map[string]any{"key": key, "chunk": i, "email": label, "status": "success", "error": "", "time": time.Now().UTC().Format(time.RFC3339)}
					if item != nil {
						result["account_id"] = item["id"]
					}
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						result["status"] = "failed"
						result["error"] = safeRefreshError(err)
					}
					_, e = lease.MutateDocument(importChunk(id, i), func(d map[string]any) error {
						results := mapValue(d["results"])
						results[key] = result
						d["results"] = results
						return nil
					})
					if e != nil {
						cancel()
						return
					}
					if !s.reconcileImportProgressWithLease(lease, id, false, i) {
						cancel()
						return
					}
				}
			}()
		}
	sendRows:
		for _, row := range rows {
			select {
			case queue <- row:
			case <-ctx.Done():
				break sendRows
			}
		}
		close(queue)
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		chunk, e = s.store.Document(importChunk(id, i))
		if e != nil {
			return
		}
		if len(mapValue(chunk["results"])) != len(rows) {
			return
		}
		// No tokens remain in completed job documents. Accounts hold the current credentials.
		_, e = lease.MutateDocument(importChunk(id, i), func(d map[string]any) error { d["accounts"] = nil; d["done"] = true; return nil })
		if e != nil {
			return
		}
	}
	if !s.reconcileImportProgressWithLease(lease, id, true) {
		return
	}
	_, _ = lease.MutateDocument(importDocument(id), func(d map[string]any) error {
		d["status"] = "completed"
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

// Each update reads only the active chunk. Startup/final reconciliation scans
// durable checkpoints once, so a 50k-row import does not rescan all rows per second.
func (s *Server) reconcileImportProgress(id string, force bool, active ...int) bool {
	return s.reconcileImportProgressWithLease(nil, id, force, active...)
}
func (s *Server) reconcileImportProgressWithLease(lease *store.Lease, id string, force bool, active ...int) bool {
	s.importProgressMu.Lock()
	defer s.importProgressMu.Unlock()
	if !force && time.Since(s.importProgressAt) < time.Second {
		return true
	}
	s.importProgressAt = time.Now()
	doc, err := s.store.Document(importDocument(id))
	if err != nil {
		return false
	}
	indices := active
	if force {
		indices = nil
		for i := 0; i < intValue(doc["chunks"]); i++ {
			indices = append(indices, i)
		}
	}
	changes := map[string]any{}
	recent := []any{}
	for _, index := range indices {
		chunk, err := s.store.Document(importChunk(id, index))
		if err != nil {
			return false
		}
		progress := map[string]any{"added": intValue(chunk["added"]), "skipped": intValue(chunk["skipped"]), "import_processed": 0, "sync_success": 0, "sync_failed": 0}
		if boolValue(chunk["imported"], false) {
			progress["import_processed"] = intValue(chunk["total"])
		}
		for _, value := range mapValue(chunk["results"]) {
			result := mapValue(value)
			metric := "sync_failed"
			if result["status"] == "success" {
				metric = "sync_success"
			}
			progress[metric] = intValue(progress[metric]) + 1
			recent = append(recent, result)
		}
		changes[fmt.Sprint(index)] = progress
	}
	mutate := s.store.MutateDocument
	if lease != nil {
		mutate = lease.MutateDocument
	}
	_, err = mutate(importDocument(id), func(d map[string]any) error {
		checkpoints := mapValue(d["chunks_progress"])
		if force {
			checkpoints = map[string]any{}
			for _, metric := range []string{"added", "skipped", "import_processed", "sync_success", "sync_failed"} {
				d[metric] = 0
			}
		}
		for index, value := range changes {
			old, next := mapValue(checkpoints[index]), mapValue(value)
			for _, metric := range []string{"added", "skipped", "import_processed", "sync_success", "sync_failed"} {
				d[metric] = intValue(d[metric]) + intValue(next[metric]) - intValue(old[metric])
			}
			checkpoints[index] = next
		}
		d["chunks_progress"] = checkpoints
		d["sync_processed"] = intValue(d["sync_success"]) + intValue(d["sync_failed"])
		d["event_count"] = d["sync_processed"]
		events := map[string]any{}
		if !force {
			for _, result := range importRows(d["events"]) {
				events[stringValue(result["key"])] = result
			}
		}
		for _, value := range recent {
			result := mapValue(value)
			events[stringValue(result["key"])] = result
		}
		ordered := []any{}
		for _, result := range events {
			ordered = append(ordered, result)
		}
		sort.Slice(ordered, func(i, j int) bool {
			a, b := mapValue(ordered[i]), mapValue(ordered[j])
			return stringValue(a["time"])+stringValue(a["key"]) < stringValue(b["time"])+stringValue(b["key"])
		})
		if len(ordered) > 200 {
			ordered = ordered[len(ordered)-200:]
		}
		d["events"] = ordered
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	return err == nil
}

// Pagination uses original row positions, which remain stable while workers
// finish out of order. It touches only chunks intersecting the requested window.
func (s *Server) importEvents(id string, doc map[string]any, offset, limit int) ([]map[string]any, int, error) {
	end := min(offset+limit, intValue(doc["total"]))
	out := []map[string]any{}
	for index := offset / importChunkSize; index*importChunkSize < end; index++ {
		chunk, err := s.store.Document(importChunk(id, index))
		if err != nil {
			return nil, offset, err
		}
		keys := stringSliceValue(chunk["result_order"])
		results := mapValue(chunk["results"])
		for position := max(offset-index*importChunkSize, 0); position < min(len(keys), end-index*importChunkSize); position++ {
			if result := mapValue(results[keys[position]]); len(result) > 0 {
				out = append(out, result)
			}
		}
	}
	return out, end, nil
}

func (s *Server) importFailure(lease *store.Lease, id string, err error) {
	_, _ = lease.MutateDocument(importDocument(id), func(d map[string]any) error { d["status"] = "failed"; d["error"] = safeRefreshError(err); return nil })
}
func importRows(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		rows := []map[string]any{}
		for _, v := range x {
			rows = append(rows, mapValue(v))
		}
		return rows
	}
	return nil
}

func stringSliceValue(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, value := range x {
			out = append(out, stringValue(value))
		}
		return out
	}
	return nil
}

func newImportID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func marshalImport(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }
