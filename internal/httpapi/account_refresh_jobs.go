package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func refreshChunk(id string, index int) string { return fmt.Sprintf("refreshchunk-%s-%06d", id, index) }
func (s *Server) createAccountRefresh(refs []string) (string, error) {
	id := newImportID()
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		return "", err
	}
	// Resolve the submitted list once. Workers still load fresh credentials by stable ID.
	index := make(map[string]map[string]any, len(items)*3)
	for _, item := range items {
		for _, ref := range []string{accountToken(item), accountPublicRef(item), stringValue(item["id"]), stringValue(item["account_ref"]), stringValue(item["account_id"]), stringValue(item["chatgpt_account_id"]), stringValue(item["user_id"]), stringValue(item["email"])} {
			key := strings.TrimSpace(ref)
			if key != "" && index[key] == nil {
				index[key] = item
			}
		}
		if email := strings.ToLower(stringValue(item["email"])); email != "" && index[email] == nil {
			index[email] = item
		}
	}
	rows := []map[string]any{}
	seen := map[string]bool{}
	for _, ref := range refs {
		row := map[string]any{"key": newImportID(), "label": tokenPreview(ref)}
		item := index[strings.TrimSpace(ref)]
		if item == nil && strings.Contains(ref, "@") {
			item = index[strings.ToLower(strings.TrimSpace(ref))]
		}
		if item != nil {
			id := stringValue(item["id"])
			if seen[id] {
				continue
			}
			seen[id] = true
			row["ref"] = id
			row["email"] = item["email"]
		} else {
			row["missing"] = true
		}
		rows = append(rows, row)
	}
	chunks := (len(rows) + importChunkSize - 1) / importChunkSize
	for i := 0; i < chunks; i++ {
		_, err := s.store.MutateDocument(refreshChunk(id, i), func(d map[string]any) error {
			d["rows"] = rows[i*importChunkSize : min((i+1)*importChunkSize, len(rows))]
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.store.MutateDocument("refreshjob-"+id, func(d map[string]any) error {
		for k, v := range map[string]any{"id": id, "status": "queued", "total": len(rows), "chunks": chunks, "processed": 0, "success": 0, "failed": 0, "created_at": now, "updated_at": now, "status_counts": map[string]int{"正常": 0, "限流": 0, "异常": 0, "禁用": 0}} {
			d[k] = v
		}
		return nil
	})
	return id, err
}
func (s *Server) accountRefreshScheduler() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
		}
		docs, err := s.store.PendingDocuments("refreshjob-", []string{"queued", "running"}, 1)
		if err == nil {
			for _, doc := range docs {
				s.runAccountRefresh(s.backgroundCtx, stringValue(doc["id"]))
			}
		}
	}
}
func (s *Server) runAccountRefresh(parent context.Context, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// One shared manual batch limits extra maintenance load across all replicas.
	lease, err := s.store.AcquireLease(ctx, "refresh-dispatch", 120*time.Second)
	if err != nil || lease == nil {
		return
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 120*time.Second, cancel)
	doc, err := s.store.Document("refreshjob-" + id)
	if err != nil || doc["status"] == "completed" {
		return
	}
	_, err = lease.MutateDocument("refreshjob-"+id, func(d map[string]any) error { d["status"] = "running"; return nil })
	if err != nil {
		return
	}
	for i := 0; i < intValue(doc["chunks"]); i++ {
		chunk, err := s.store.Document(refreshChunk(id, i))
		if err != nil || ctx.Err() != nil {
			return
		}
		if boolValue(chunk["done"], false) {
			continue
		}
		queue := make(chan map[string]any)
		var workers sync.WaitGroup
		for n := 0; n < accountRefreshConcurrency(); n++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for row := range queue {
					key := stringValue(row["key"])
					check, e := s.store.Document(refreshChunk(id, i))
					if e != nil {
						cancel()
						return
					}
					if mapValue(check["results"])[key] != nil {
						continue
					}
					var updated map[string]any
					var syncErr error
					if boolValue(row["missing"], false) {
						syncErr = errors.New("account not found")
					} else {
						updated, syncErr = s.accountByRef(stringValue(row["ref"]))
						if syncErr == nil {
							updated, syncErr = s.syncCredential(ctx, updated, false)
						}
					}
					if errors.Is(syncErr, errCredentialBusy) {
						continue
					}
					if ctx.Err() != nil {
						return
					}
					result := map[string]any{"success": syncErr == nil, "label": row["label"], "email": row["email"], "error": "", "category": "异常", "quota": 0}
					if syncErr != nil {
						result["error"] = safeRefreshError(syncErr)
						if fields, e := s.accountByRef(stringValue(row["ref"])); e == nil {
							updated = fields
						}
					}
					if updated != nil {
						result["category"] = map[string]string{"normal": "正常", "limited": "限流", "abnormal": "异常", "disabled": "禁用"}[accountStatusCategory(updated)]
						result["quota"] = max(0, intValue(updated["quota"]))
					}
					_, e = lease.MutateDocument(refreshChunk(id, i), func(d map[string]any) error {
						results := mapValue(d["results"])
						results[key] = result
						d["results"] = results
						return nil
					})
					if e != nil || !s.reconcileRefresh(lease, id, i) {
						cancel()
						return
					}
				}
			}()
		}
	send:
		for _, row := range importRows(chunk["rows"]) {
			select {
			case queue <- row:
			case <-ctx.Done():
				break send
			}
		}
		close(queue)
		workers.Wait()
		if ctx.Err() != nil {
			return
		}
		chunk, err = s.store.Document(refreshChunk(id, i))
		if err != nil || len(mapValue(chunk["results"])) != len(importRows(chunk["rows"])) {
			return
		}
		_, err = lease.MutateDocument(refreshChunk(id, i), func(d map[string]any) error { d["done"] = true; return nil })
		if err != nil {
			return
		}
	}
	// Reconcile every chunk after recovery: a crash can occur after result commit.
	for i := 0; i < intValue(doc["chunks"]); i++ {
		if !s.reconcileRefresh(lease, id, i) {
			return
		}
	}
	_, _ = lease.MutateDocument("refreshjob-"+id, func(d map[string]any) error {
		d["status"] = "completed"
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
}
func (s *Server) reconcileRefresh(lease *store.Lease, id string, index int) bool {
	chunk, err := s.store.Document(refreshChunk(id, index))
	if err != nil {
		return false
	}
	summary := map[string]any{"processed": 0, "success": 0, "failed": 0, "total_quota": 0, "counts": map[string]any{}}
	counts := mapValue(summary["counts"])
	for _, value := range mapValue(chunk["results"]) {
		result := mapValue(value)
		summary["processed"] = intValue(summary["processed"]) + 1
		metric := "failed"
		if boolValue(result["success"], false) {
			metric = "success"
		}
		summary[metric] = intValue(summary[metric]) + 1
		summary["total_quota"] = intValue(summary["total_quota"]) + intValue(result["quota"])
		category := firstNonEmpty(stringValue(result["category"]), "异常")
		counts[category] = intValue(counts[category]) + 1
	}
	_, err = lease.MutateDocument("refreshjob-"+id, func(d map[string]any) error {
		summaries := mapValue(d["chunk_progress"])
		// Concurrent callbacks never replace a newer chunk summary with an old read.
		old := mapValue(summaries[fmt.Sprint(index)])
		if intValue(old["processed"]) > intValue(summary["processed"]) {
			return nil
		}
		summaries[fmt.Sprint(index)] = summary
		d["chunk_progress"] = summaries
		for _, k := range []string{"processed", "success", "failed", "total_quota"} {
			d[k] = intValue(d[k]) + intValue(summary[k]) - intValue(old[k])
		}
		totalCounts := mapValue(d["status_counts"])
		for k, v := range counts {
			totalCounts[k] = intValue(totalCounts[k]) + intValue(v) - intValue(mapValue(old["counts"])[k])
		}
		d["status_counts"] = totalCounts
		errorRows := importRows(d["errors"])
		if intValue(summary["failed"]) > intValue(old["failed"]) && len(errorRows) < 200 {
			seen := map[string]bool{}
			for _, row := range errorRows {
				seen[stringValue(row["key"])] = true
			}
			for key, value := range mapValue(chunk["results"]) {
				result := mapValue(value)
				errorKey := fmt.Sprintf("%d:%s", index, key)
				if boolValue(result["success"], false) || seen[errorKey] || len(errorRows) >= 200 {
					continue
				}
				errorRows = append(errorRows, map[string]any{"key": errorKey, "token": result["label"], "email": result["email"], "error": result["error"]})
			}
		}
		d["errors"] = errorRows
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	return err == nil
}
