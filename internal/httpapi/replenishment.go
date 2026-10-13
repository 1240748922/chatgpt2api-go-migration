package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/ingest"
	"github.com/auucoder/gptgrok2api-go/internal/replenish"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func (s *Server) replenishmentSettings() (replenish.Settings, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return replenish.Settings{}, err
	}
	settings := replenish.Defaults()
	if cfg["account_replenishment"] != nil {
		raw, _ := json.Marshal(cfg["account_replenishment"])
		if err = json.Unmarshal(raw, &settings); err != nil {
			return settings, err
		}
	}
	return settings, nil
}
func (s *Server) replenishmentRoot() string {
	if root := os.Getenv("GO_REGISTRATION_RUNTIME_DIR"); root != "" {
		return root
	}
	return filepath.Join(s.cfg.DataDir, "registration")
}
func (s *Server) replenishmentTool() string {
	if root := os.Getenv("GO_REGISTRATION_TOOL_DIR"); root != "" {
		return root
	}
	return filepath.Join(s.cfg.RootDir, "registration-tool")
}
func (s *Server) accountReplenishment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/replenishment")
	switch {
	case path == "" && r.Method == http.MethodGet:
		settings, err := s.replenishmentSettings()
		if err != nil {
			writeError(w, 503, "读取注册配置失败", "storage_error")
			return
		}
		jobs, err := s.store.ListDocuments("replenishjob-", "", nil, 30)
		if err != nil {
			writeError(w, 503, "读取注册记录失败", "storage_error")
			return
		}
		status, _ := s.store.Document("replenish-status")
		writeJSON(w, 200, map[string]any{"settings": settings, "jobs": jobs, "worker": status})
	case path == "" && (r.Method == http.MethodPatch || r.Method == http.MethodPost):
		settings, err := s.replenishmentSettings()
		if err != nil {
			writeError(w, 503, "读取配置失败", "storage_error")
			return
		}
		if !decodeJSON(w, r, &settings) {
			return
		}
		if err = settings.Validate(); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		if settings.MailboxFile != "" {
			name := filepath.Base(settings.MailboxFile)
			if name != settings.MailboxFile {
				writeError(w, 400, "请选择已上传的邮箱文件", "invalid_request_error")
				return
			}
		}
		_, err = s.store.PatchConfig(map[string]any{"account_replenishment": settings})
		if err != nil {
			writeError(w, 503, "保存注册配置失败", "storage_error")
			return
		}
		writeJSON(w, 200, settings)
	case path == "/provider-config":
		s.replenishmentProviderConfig(w, r)
	case path == "/mailbox-file" && r.Method == http.MethodPost:
		var body struct {
			Filename string `json:"filename"`
			Content  string `json:"content"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		name := filepath.Base(body.Filename)
		ext := strings.ToLower(filepath.Ext(name))
		if name != body.Filename || strings.ContainsAny(name, "/\\") || name == "." || (ext != ".txt" && ext != ".csv" && ext != ".json") || len(body.Content) > 10<<20 {
			writeError(w, 400, "邮箱文件名称、类型或大小无效", "invalid_request_error")
			return
		}
		root := filepath.Join(s.replenishmentRoot(), "mailboxes")
		err := os.MkdirAll(root, 0700)
		if err == nil {
			err = os.WriteFile(filepath.Join(root, name), []byte(body.Content), 0600)
		}
		if err != nil {
			writeError(w, 503, "保存邮箱文件失败", "storage_error")
			return
		}
		writeJSON(w, 200, map[string]any{"filename": name, "mailbox_file": name})
	case path == "/run" && r.Method == http.MethodPost:
		settings, err := s.replenishmentSettings()
		if err != nil {
			writeError(w, 503, "读取注册配置失败", "storage_error")
			return
		}
		if err = settings.Validate(); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		count := queryInt(r, "manual_count", min(settings.MaxBatch, 1), 1, 1000)
		doc, err := s.queueReplenishment(r.Context(), count, true)
		if err != nil {
			writeError(w, 409, err.Error(), "registration_busy")
			return
		}
		writeJSON(w, 202, doc)
	default:
		writeError(w, 404, "registration endpoint not found", "not_found")
	}
}

var providerFields = map[string]bool{"remail_base_url": true, "remail_api_key": true, "smailr_base_url": true, "smailr_api_key": true, "cfworker_url": true, "cfworker_admin_token": true, "cfworker_api_token": true, "smsbower_api_key": true, "smsbower_country": true, "registration_proxy": true, "proxy_pool": true}

func providerPublic(d map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range d {
		if !providerFields[key] {
			continue
		}
		if strings.Contains(key, "key") || strings.Contains(key, "token") {
			out[key] = ""
			out[key+"_configured"] = strings.TrimSpace(stringValue(value)) != ""
		} else {
			out[key] = value
		}
	}
	return out
}
func (s *Server) replenishmentProviderConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		d, e := s.store.Document("replenish-provider")
		if e != nil {
			writeError(w, 503, "读取注册服务配置失败", "storage_error")
			return
		}
		writeJSON(w, 200, providerPublic(d))
		return
	}
	if r.Method != http.MethodPatch {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	for key := range body {
		if !providerFields[key] {
			writeError(w, 400, "未知的注册服务配置字段", "invalid_request_error")
			return
		}
	}
	d, err := s.store.MutateDocument("replenish-provider", func(d map[string]any) error {
		for key, value := range body {
			if value == nil {
				delete(d, key)
				continue
			}
			if (strings.Contains(key, "key") || strings.Contains(key, "token")) && strings.TrimSpace(stringValue(value)) == "" {
				continue
			}
			d[key] = value
		}
		return nil
	})
	if err != nil {
		writeError(w, 503, "保存注册服务配置失败", "storage_error")
		return
	}
	writeJSON(w, 200, providerPublic(d))
}
func (s *Server) queueReplenishment(ctx context.Context, count int, manual bool) (map[string]any, error) {
	lease, err := s.store.AcquireLease(ctx, "replenish-create", 30*time.Second)
	if err != nil {
		return nil, errors.New("注册调度数据库不可用")
	}
	if lease == nil {
		return nil, errors.New("注册任务正在创建")
	}
	defer lease.Release()
	pending, err := s.store.PendingDocuments("replenishjob-", []string{"queued", "running", "registration_complete", "importing"}, 1)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, errors.New("已有注册或导入任务正在执行")
	}
	settings, err := s.replenishmentSettings()
	if err != nil {
		return nil, err
	}
	if err = settings.Validate(); err != nil {
		return nil, err
	}
	id := newImportID()
	return lease.MutateDocument("replenishjob-"+id, func(d map[string]any) error {
		d["id"] = id
		d["status"] = "queued"
		d["count"] = count
		d["manual"] = manual
		d["settings"] = settings
		d["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		d["logs"] = []any{}
		return nil
	})
}
func (s *Server) replenishmentScheduler() {
	if !strings.EqualFold(os.Getenv("GO_REGISTRATION_WORKER"), "true") {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
		}
		_, _ = s.store.MutateDocument("replenish-status", func(d map[string]any) error {
			d["last_seen_at"] = time.Now().UTC().Format(time.RFC3339)
			d["instance_id"] = s.instanceID
			return nil
		})
		lease, err := s.store.AcquireLease(s.backgroundCtx, "replenish-worker", 120*time.Second)
		if err != nil || lease == nil {
			continue
		}
		ctx, cancel := context.WithCancel(s.backgroundCtx)
		lease.KeepAlive(ctx, 120*time.Second, cancel)
		pending, err := s.store.PendingDocuments("replenishjob-", []string{"queued", "running", "registration_complete", "importing"}, 1)
		if err == nil && len(pending) > 0 {
			s.runReplenishment(ctx, lease, pending[0])
		} else if err == nil {
			s.maybeReplenish(ctx)
		}
		cancel()
		lease.Release()
	}
}
func (s *Server) maybeReplenish(ctx context.Context) {
	settings, err := s.replenishmentSettings()
	if err != nil || !settings.Enabled || settings.Validate() != nil {
		return
	}
	status, err := s.store.Document("replenish-auto")
	if err != nil {
		return
	}
	next, _ := time.Parse(time.RFC3339, stringValue(status["next_check_at"]))
	if time.Now().Before(next) {
		return
	}
	jobs, err := s.store.ListDocuments("replenishjob-", "", nil, 1)
	if err != nil {
		return
	}
	if len(jobs) > 0 {
		last, _ := time.Parse(time.RFC3339Nano, firstNonEmpty(stringValue(jobs[0]["finished_at"]), stringValue(jobs[0]["created_at"])))
		if time.Since(last) < time.Duration(settings.Cooldown)*time.Second {
			return
		}
	}
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		return
	}
	ready := 0
	for _, item := range items {
		if isOpenAIAccount(accounts.Account{Token: accountToken(item), Fields: item}) && accounts.ImageEligible(item, false, time.Now(), 5*time.Minute) {
			ready++
		}
	}
	_, _ = s.store.MutateDocument("replenish-auto", func(d map[string]any) error {
		d["next_check_at"] = time.Now().Add(time.Duration(settings.Interval) * time.Second).UTC().Format(time.RFC3339)
		d["available"] = ready
		return nil
	})
	if ready < settings.Minimum {
		_, _ = s.queueReplenishment(ctx, min(settings.MaxBatch, max(1, settings.Target-ready)), false)
	}
}
func (s *Server) registrationConfig(settings replenish.Settings, output string) (map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(s.replenishmentTool(), "config.example.json"))
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	providerCfg, err := s.store.Document("replenish-provider")
	if err != nil {
		return nil, err
	}
	cfg["proxy"] = map[string]any{"registration": stringValue(providerCfg["registration_proxy"]), "default": "", "pool": stringSliceValue(providerCfg["proxy_pool"])}
	cfg["output"] = map[string]any{"directory": output}
	cfg["runtime"] = map[string]any{"directory": filepath.Join(s.replenishmentRoot(), "runtime")}
	email := mapValue(cfg["email_registration"])
	for _, source := range []string{"remail", "smailr"} {
		sub := mapValue(email[source])
		for _, key := range []string{"base_url", "api_key"} {
			if v := stringValue(providerCfg[source+"_"+key]); v != "" {
				sub[key] = v
			}
		}
		email[source] = sub
	}
	for _, key := range []string{"cfworker_url", "cfworker_admin_token", "cfworker_api_token"} {
		if v := stringValue(providerCfg[key]); v != "" {
			email[key] = v
		}
	}
	cfg["email_registration"] = email
	phone := mapValue(cfg["phone_reuse"])
	sms := mapValue(phone["smsbower"])
	for _, key := range []string{"api_key", "country"} {
		if v := stringValue(providerCfg["smsbower_"+key]); v != "" {
			sms[key] = v
		}
	}
	phone["smsbower"] = sms
	cfg["phone_reuse"] = phone
	return cfg, nil
}
func (s *Server) runReplenishment(ctx context.Context, lease *store.Lease, doc map[string]any) {
	id := stringValue(doc["id"])
	name := "replenishjob-" + id
	save := func(status, reason string) bool {
		_, err := lease.MutateDocument(name, func(d map[string]any) error {
			d["status"] = status
			d["error"] = reason
			d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			if status == "failed" || status == "completed" {
				d["finished_at"] = d["updated_at"]
			}
			return nil
		})
		return err == nil
	}
	root := filepath.Join(s.replenishmentRoot(), id)
	output := filepath.Join(root, "sessions")
	settings, err := s.replenishmentSettings()
	if snapshot := doc["settings"]; snapshot != nil {
		raw, marshalErr := json.Marshal(snapshot)
		if marshalErr != nil {
			err = marshalErr
		} else {
			err = json.Unmarshal(raw, &settings)
		}
	}
	if err != nil || settings.Validate() != nil {
		save("failed", "注册配置无效")
		return
	}
	if doc["status"] == "running" {
		// A crashed subprocess is never relaunched: only collect durable partial output.
		sessions, scanErr := replenish.Sessions(output, id)
		_ = os.Remove(filepath.Join(root, "config.json"))
		if scanErr != nil || len(sessions) == 0 {
			save("failed", "registration_result_uncertain: 注册进程中断，未找到可导入结果，请检查日志")
			return
		}
		_, err = lease.MutateDocument(name, func(d map[string]any) error {
			d["status"] = "registration_complete"
			d["partial_result"] = true
			d["registration_warning"] = "注册进程中断，已恢复部分成功结果；不会自动重跑注册"
			return nil
		})
		if err != nil {
			return
		}
	}
	if doc["status"] == "importing" && stringValue(doc["import_job_id"]) != "" {
		s.publishReplenishmentImport(lease, name, stringValue(doc["import_job_id"]))
		return
	}
	if doc["status"] == "queued" {
		if err = os.MkdirAll(output, 0700); err != nil {
			save("failed", "无法创建注册目录")
			return
		}
		config, err := s.registrationConfig(settings, output)
		if err != nil {
			save("failed", "无法读取注册工具配置模板")
			return
		}
		raw, _ := json.Marshal(config)
		path := filepath.Join(root, "config.json")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			save("failed", "无法保存注册工具配置")
			return
		}
		defer os.Remove(path)
		if !save("running", "") {
			return
		}
		file, err := os.OpenFile(filepath.Join(root, "registration.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			save("failed", "无法创建注册日志")
			return
		}
		var mu sync.Mutex
		lines := []string{}
		writer := replenish.NewLogWriter(file, config, func(line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, line)
			if len(lines) > 100 {
				lines = lines[len(lines)-100:]
			}
		})
		python := firstNonEmpty(os.Getenv("GO_REGISTRATION_PYTHON"), "python3")
		if settings.Source == "mailbox_file" {
			settings.MailboxFile = filepath.Join(s.replenishmentRoot(), "mailboxes", settings.MailboxFile)
		}
		args := replenish.Command(settings, intValue(doc["count"]), filepath.Join(s.replenishmentTool(), "chatgpt_phone_reg.py"), output, id)
		runCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.Timeout)*time.Second)
		done := make(chan error, 1)
		go func() { done <- replenish.Run(runCtx, python, s.replenishmentTool(), path, args, writer) }()
		tick := time.NewTicker(time.Second)
		publish := func() {
			_, _ = s.store.MutateDocument("replenish-status", func(d map[string]any) error {
				d["last_seen_at"] = time.Now().UTC().Format(time.RFC3339)
				d["instance_id"] = s.instanceID
				return nil
			})
			mu.Lock()
			copyLines := append([]string{}, lines...)
			mu.Unlock()
			_, _ = lease.MutateDocument(name, func(d map[string]any) error {
				d["logs"] = copyLines
				d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				return nil
			})
		}
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case <-tick.C:
				publish()
			}
		}
		cancel()
		tick.Stop()
		writer.Flush()
		file.Close()
		publish()
		_ = os.Remove(path)
		if ctx.Err() != nil {
			return
		}
		sessions, scanErr := replenish.Sessions(output, id)
		if scanErr != nil {
			save("failed", "读取注册结果失败")
			return
		}
		if len(sessions) == 0 {
			save("failed", "registration_failed: 未生成可导入的账号，请查看注册日志")
			return
		}
		// Partial results survive a failed subprocess and still get imported.
		if err != nil {
			_, _ = lease.MutateDocument(name, func(d map[string]any) error { d["partial_result"] = true; return nil })
		}
		if !save("registration_complete", "") {
			return
		}
	}
	if !settings.ImportSessions {
		save("completed", "")
		return
	}
	sessions, err := replenish.Sessions(output, id)
	if err != nil {
		save("failed", "读取注册结果失败")
		return
	}
	rows := []map[string]any{}
	for _, text := range sessions {
		parsed, err := ingest.Parse(text, "auto")
		if err == nil {
			rows = append(rows, parsed...)
		}
	}
	if len(rows) == 0 {
		save("failed", "注册结果中没有有效 AT 或 RT")
		return
	}
	hash := sha256.Sum256([]byte("registration-import:" + id))
	importID := hex.EncodeToString(hash[:16])
	job, err := s.store.Document(importDocument(importID))
	if err != nil {
		return
	}
	if job["id"] == nil {
		job, err = s.createImportJobWithID(rows, false, importID)
		if err != nil {
			return
		}
	}
	_, err = lease.MutateDocument(name, func(d map[string]any) error {
		d["status"] = "importing"
		d["import_job_id"] = importID
		d["session_count"] = len(rows)
		return nil
	})
	if err != nil {
		return
	}
	s.publishReplenishmentImport(lease, name, importID)
}

func (s *Server) publishReplenishmentImport(lease *store.Lease, name, importID string) {
	job, err := s.store.Document(importDocument(importID))
	if err != nil || job["id"] == nil {
		return
	}
	_, _ = lease.MutateDocument(name, func(d map[string]any) error {
		d["imported"] = job["added"]
		d["skipped"] = job["skipped"]
		for _, key := range []string{"import_processed", "sync_processed", "sync_total", "sync_success", "sync_failed", "events"} {
			d[key] = job[key]
		}
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		if job["status"] == "completed" || job["status"] == "failed" {
			d["status"] = job["status"]
			d["error"] = job["error"]
			d["finished_at"] = d["updated_at"]
		}
		return nil
	})
}
