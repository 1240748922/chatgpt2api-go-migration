package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"time"
)

func (s *Server) sharedRuntimeScheduler() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer s.store.RemoveRuntime(s.instanceID)
	var applied [32]byte
	nextCleanup := time.Now()
	for {
		select {
		case <-s.probeStop:
			return
		case <-ticker.C:
		}
		local := s.monitor.snapshot()
		_ = s.store.PublishRuntime(s.instanceID, map[string]any{"active": local["active"]})
		if s.store.Shared() {
			ids, err := s.store.ConsumeRuntimeCancels(s.instanceID)
			if err == nil {
				for _, id := range ids {
					s.monitor.cancel(id)
				}
			}
		}
		if cfg, err := s.store.Config(); err == nil {
			s.applyAuthConfig(cfg)
			raw, _ := json.Marshal(map[string]any{"proxy": cfg["proxy"], "proxy_pool": cfg["proxy_pool"], "proxy_groups": cfg["proxy_groups"], "fallback_proxy": cfg["fallback_proxy"], "resource_proxy": cfg["resource_proxy"], "resource_proxy_pool": cfg["resource_proxy_pool"]})
			hash := sha256.Sum256(raw)
			if hash != applied && s.refreshProxyRuntime() == nil {
				applied = hash
			}
		}
		if time.Now().After(nextCleanup) {
			nextCleanup = time.Now().Add(time.Hour)
			lease, err := s.store.AcquireLease(s.backgroundCtx, "log-retention", time.Minute)
			if err == nil && lease != nil {
				cfg, _ := s.store.Config()
				days := intValue(cfg["log_retention_days"])
				if days <= 0 {
					days = 30
				}
				_, _ = s.store.DeleteCallRecords(nil, time.Now().Add(-time.Duration(days)*24*time.Hour))
				lease.Release()
			}
		}
	}
}

// Persisted keys override environment bootstrap keys, including explicit clears.
func configuredAuthKeys(values map[string]any, fallbackAPI, fallbackAdmin string) (string, string) {
	key := func(names []string, fallback string) string {
		for _, name := range names {
			if value, exists := values[name]; exists {
				return stringValue(value)
			}
		}
		return fallback
	}
	admin := key([]string{"app_key", "admin_key"}, fallbackAdmin)
	api := key([]string{"auth-key", "api_key"}, fallbackAPI)
	return api, admin
}

func (s *Server) applyAuthConfig(values map[string]any) {
	api, admin := configuredAuthKeys(values, s.cfg.APIKey, s.cfg.AdminKey)
	s.auth.UpdateKeys(api, admin)
}
func (s *Server) sharedMonitorSnapshot() map[string]any {
	local := s.monitor.snapshot()
	if !s.store.Shared() {
		return local
	}
	active := monitorRecordMaps(local["active"])
	snapshots, err := s.store.RuntimeSnapshots()
	if err != nil {
		local["shared_error"] = "共享监控暂不可用"
		return local
	}
	for _, snapshot := range snapshots {
		if stringValue(snapshot["instance_id"]) == s.instanceID {
			continue
		}
		for _, item := range monitorRecordMaps(snapshot["active"]) {
			item["instance_id"] = snapshot["instance_id"]
			active = append(active, item)
		}
	}
	local["active"] = active
	return local
}
func (s *Server) sharedMonitorDetail(id string) (map[string]any, bool) {
	if item, ok := s.monitor.detail(id); ok {
		return monitorRecordMap(item), true
	}
	for _, item := range monitorRecordMaps(s.sharedMonitorSnapshot()["active"]) {
		if stringValue(item["call_id"]) == id {
			return item, true
		}
	}
	records, _, err := s.store.QueryCallRecords(url.Values{"id": {id}}, 1, 0)
	if err == nil && len(records) > 0 && stringValue(records[0]["id"]) == id {
		return mapValue(records[0]["detail"]), true
	}
	return nil, false
}
