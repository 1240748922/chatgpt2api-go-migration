package httpapi

import (
	"context"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func maintenancePolicy(metrics map[string]any, heap uint64) (batch, concurrency int, delay time.Duration, reason string) {
	read, write := intValue(metrics["database_p95_ms"]), intValue(metrics["account_write_p95_ms"])
	switch {
	case read > 3000 || write > 3000 || heap > 1<<30:
		return 1, 1, 5 * time.Second, "存储或内存压力较高，保留恢复探针"
	case read > 500 || write > 1000:
		return 4, 1, 2 * time.Second, "存储响应较慢，降低维护速率"
	case read > 150 || write > 500:
		return 8, 2, time.Second, "性能保护"
	default:
		return 24, 4, 200 * time.Millisecond, "持续维护"
	}
}
func maintenanceDue(item map[string]any, now time.Time, minimum time.Duration) bool {
	r := accounts.CredentialReadiness(item, now, minimum)
	if r.State == "disabled" || r.State == "uncertain" || r.State == "refreshing" {
		return false
	}
	if r.State != "ready" && (!r.Recoverable && r.State != "quarantined") {
		return false
	}
	if due, e := time.Parse(time.RFC3339, stringValue(item["next_maintenance_at"])); e == nil && due.After(now) {
		return false
	}
	return true
}
func (s *Server) accountMaintenanceScheduler() {
	for {
		select {
		case <-s.probeStop:
			return
		default:
		}
		delay := s.runMaintenanceBatch()
		select {
		case <-s.probeStop:
			return
		case <-time.After(delay):
		}
	}
}
func (s *Server) runMaintenanceBatch() time.Duration {
	ctx, cancel := context.WithTimeout(s.backgroundCtx, 100*time.Second)
	defer cancel()
	lease, e := s.store.AcquireLease(ctx, "account-maintenance", 120*time.Second)
	if e != nil || lease == nil {
		return 2 * time.Second
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 120*time.Second, cancel)
	cfg, _ := s.store.Config()
	maintenance := mapValue(cfg["account_maintenance"])
	if !boolValue(maintenance["enabled"], true) {
		s.writeMaintenance(map[string]any{"status": "disabled", "running": 0, "reason": "已关闭后台维护"})
		return 5 * time.Second
	}
	metrics := s.store.LatencyMetrics()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	batch, concurrency, delay, reason := maintenancePolicy(metrics, mem.HeapAlloc)
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		s.writeMaintenance(map[string]any{"status": "degraded", "running": 0, "reason": "账号数据库不可用"})
		return 5 * time.Second
	}
	now := time.Now()
	minimum := s.configuredImageMinimumValidity()
	candidates := []map[string]any{}
	for _, item := range items {
		if isOpenAIAccount(accounts.Account{Token: accountToken(item), Fields: item}) && maintenanceDue(item, now, minimum) {
			candidates = append(candidates, item)
		}
	}
	// Oldest attempt first. Failure backoff prevents bad RTs monopolizing every batch.
	sort.SliceStable(candidates, func(i, j int) bool {
		return stringValue(candidates[i]["last_sync_at"]) < stringValue(candidates[j]["last_sync_at"])
	})
	pending := len(candidates)
	if batch > pending {
		batch = pending
	}
	status := map[string]any{"status": "running", "running": min(batch, concurrency), "batch_size": batch, "concurrency": concurrency, "pending": pending, "processed": 0, "success": 0, "failed": 0, "reason": reason, "metrics": metrics, "heap_bytes": mem.HeapAlloc, "started_at": now.UTC().Format(time.RFC3339)}
	s.writeMaintenance(status)
	if batch == 0 {
		status["status"] = "idle"
		status["running"] = 0
		s.writeMaintenance(status)
		return 2 * time.Second
	}
	type result struct{ err error }
	queue := make(chan map[string]any, batch)
	results := make(chan result, batch)
	for _, item := range candidates[:batch] {
		queue <- item
	}
	close(queue)
	for n := 0; n < min(batch, concurrency); n++ {
		go func() {
			for item := range queue {
				_, e := s.syncCredential(ctx, item, false)
				if boolValue(cfg["auto_remove_invalid_accounts"], false) || boolValue(cfg["auto_remove_rate_limited_accounts"], false) {
					_, _ = s.removeAccountSafely(ctx, item, boolValue(cfg["auto_remove_invalid_accounts"], false), boolValue(cfg["auto_remove_rate_limited_accounts"], false))
				}
				if e == errCredentialBusy {
					_, _ = s.store.UpdateAccountRuntime(accountToken(item), map[string]any{"next_maintenance_at": time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339)})
				}
				results <- result{e}
			}
		}()
	}
	for i := 0; i < batch; i++ {
		v := <-results
		status["processed"] = i + 1
		if v.err == nil {
			status["success"] = intValue(status["success"]) + 1
		} else {
			status["failed"] = intValue(status["failed"]) + 1
		}
		status["running"] = min(batch-i-1, concurrency)
		s.writeMaintenance(status)
	}
	status["status"] = "idle"
	status["finished_at"] = time.Now().UTC().Format(time.RFC3339)
	s.writeMaintenance(status)
	return delay
}
func (s *Server) writeMaintenance(fields map[string]any) {
	_, _ = s.store.MutateDocument("maintenance-status", func(d map[string]any) error {
		for k, v := range fields {
			d[k] = v
		}
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}
func (s *Server) availabilitySnapshot() (map[string]any, error) {
	items, _, e := s.store.AccountSnapshot()
	if e != nil {
		return nil, e
	}
	now := time.Now()
	minimum := s.configuredImageMinimumValidity()
	counts := map[string]int{}
	quotas := map[string]map[string]int{"generation": {"known": 0, "accounts": 0, "unknown": 0, "unlimited": 0}, "edit": {"known": 0, "accounts": 0, "unknown": 0, "unlimited": 0}}
	for _, item := range items {
		if !isOpenAIAccount(accounts.Account{Token: accountToken(item), Fields: item}) {
			continue
		}
		r := accounts.CredentialReadiness(item, now, minimum)
		counts[r.State]++
		for _, mode := range []string{"generation", "edit"} {
			if !accounts.ImageEligible(item, mode == "edit", now, minimum) {
				continue
			}
			q := quotas[mode]
			plan := strings.ToLower(firstNonEmpty(stringValue(item["plan_type"]), stringValue(item["type"])))
			if boolValue(item["image_quota_unlimited"], false) || (boolValue(item["image_quota_unknown"], false) && (plan == "pro" || plan == "prolite")) {
				q["unlimited"]++
			} else if item["quota"] == nil || boolValue(item["image_quota_unknown"], false) {
				q["unknown"]++
			} else {
				q["known"] += max(0, intValue(item["quota"]))
				q["accounts"]++
			}
		}
	}
	maintenance, e := s.store.Document("maintenance-status")
	if e != nil {
		return nil, e
	}
	running, e := s.store.ActiveLeases("credential:")
	if e != nil {
		return nil, e
	}
	maintenance["running"] = running
	active, e := s.store.ActiveLeases("image:")
	if e != nil {
		return nil, e
	}
	prewarm := map[string]any{}
	if s.openAIImage != nil {
		prewarm = s.openAIImage.PrewarmStats()
	}
	return map[string]any{"minimum_validity_seconds": int64(minimum.Seconds()), "states": counts, "labels": accounts.ReadinessLabels, "ready_quota": quotas, "maintenance": maintenance, "prewarm": prewarm, "active_images": active, "shared_storage": s.store.Shared(), "metrics": s.store.LatencyMetrics(), "sampled_at": now.UTC().Format(time.RFC3339)}, nil
}
func (s *Server) accountAvailability(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	v, e := s.availabilitySnapshot()
	if e != nil {
		writeError(w, 503, "读取账号可用性失败", "storage_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, v)
}
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.workerMu.Lock()
		s.stopping = true
		s.backgroundCancel()
		s.workerMu.Unlock()
		close(s.probeStop)
		s.taskQueue.Stop()
		s.backgroundWG.Wait()
		s.openAIImage.Close()
		s.requestClient.CloseIdleConnections()
	})
	return s.store.Close()
}

// API-triggered workers participate in graceful shutdown. No Add may race Wait.
func (s *Server) launchBackground(fn func()) bool {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.stopping {
		return false
	}
	s.backgroundWG.Add(1)
	go func() { defer s.backgroundWG.Done(); fn() }()
	return true
}
