package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

type editableTaskPayload struct {
	Prompt string                      `json:"prompt"`
	Inputs []provider.OpenAIImageInput `json:"inputs,omitempty"`
}

func editableTaskDocument(owner, id string) string {
	return fmt.Sprintf("filetask-%x", sha256.Sum256([]byte(owner+"\x00"+id)))
}
func editablePrivateDocument(name, prefix string) string {
	return prefix + strings.TrimPrefix(name, "filetask-")
}
func editableTaskDecode(doc map[string]any) (editableFileTaskState, error) {
	var task editableFileTaskState
	raw, err := json.Marshal(doc)
	if err == nil {
		err = json.Unmarshal(raw, &task)
	}
	return task, err
}
func documentMap(value any) map[string]any {
	raw, _ := json.Marshal(value)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return doc
}

func (s *Server) editableFileTasksAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost {
		s.createEditableTask(w, r, "")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	owner := s.authIdentity(r)
	items, missing := []map[string]any{}, []string{}
	if ids := strings.TrimSpace(r.URL.Query().Get("ids")); ids != "" {
		if len(strings.Split(ids, ",")) > 100 {
			writeError(w, 400, "too many task IDs", "invalid_request_error")
			return
		}
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			doc, err := s.store.Document(editableTaskDocument(owner, id))
			if err != nil {
				writeError(w, 503, "读取文件任务失败", "storage_error")
				return
			}
			task, e := editableTaskDecode(doc)
			if e != nil || task.ID == "" {
				missing = append(missing, id)
			} else {
				items = append(items, editableTaskPublic(&task))
			}
		}
	} else {
		docs, err := s.store.ListDocuments("filetask-", owner, nil, 100)
		if err != nil {
			writeError(w, 503, "读取文件任务失败", "storage_error")
			return
		}
		for _, doc := range docs {
			task, e := editableTaskDecode(doc)
			if e == nil {
				items = append(items, editableTaskPublic(&task))
			}
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing})
}
func (s *Server) pptGenerations(w http.ResponseWriter, r *http.Request) {
	s.editableGeneration(w, r, "ppt")
}
func (s *Server) psdGenerations(w http.ResponseWriter, r *http.Request) {
	s.editableGeneration(w, r, "psd")
}
func (s *Server) editableGeneration(w http.ResponseWriter, r *http.Request, kind string) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	s.createEditableTask(w, r, kind)
}
func (s *Server) createEditableTask(w http.ResponseWriter, r *http.Request, kind string) {
	var body struct {
		ID     string   `json:"client_task_id"`
		Prompt string   `json:"prompt"`
		Kind   string   `json:"kind"`
		Images []string `json:"base64_images"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		body.ID = "file_" + randomID()
	}
	kind = firstNonEmpty(kind, body.Kind, "ppt")
	if len(body.ID) > 128 || strings.ContainsAny(body.ID, "/\\") || strings.TrimSpace(body.Prompt) == "" || (kind != "ppt" && kind != "psd") || len(body.Images) > 7 || (kind == "psd" && len(body.Images) == 0) {
		writeError(w, 400, "invalid editable task ID, prompt, kind or images", "invalid_request_error")
		return
	}
	r = s.reviewRequest(w, r, body.Prompt)
	if r == nil {
		return
	}
	inputs, err := provider.DecodeEditableInputs(body.Images)
	if err != nil {
		writeError(w, 400, "invalid input images", "invalid_request_error")
		return
	}
	bytes := 0
	for _, input := range inputs {
		bytes += len(input.Data)
		if !strings.HasPrefix(input.MIME, "image/") || len(input.Data) == 0 || len(input.Data) > 20<<20 {
			writeError(w, 400, "invalid input image", "invalid_request_error")
			return
		}
	}
	if bytes > 64<<20 {
		writeError(w, 413, "editable inputs exceed 64 MiB", "invalid_request_error")
		return
	}
	owner := s.authIdentity(r)
	name := editableTaskDocument(owner, body.ID)
	lease, err := s.store.AcquireLease(r.Context(), "task-create:"+name, 30*time.Second)
	if err != nil {
		writeError(w, 503, "任务存储不可用", "storage_error")
		return
	}
	if lease == nil {
		writeError(w, 409, "task creation in progress", "task_busy")
		return
	}
	defer lease.Release()
	doc, err := s.store.Document(name)
	if err != nil {
		writeError(w, 503, "任务存储不可用", "storage_error")
		return
	}
	if doc["id"] != nil {
		task, _ := editableTaskDecode(doc)
		writeJSON(w, 202, editableTaskPublic(&task))
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	task := editableFileTaskState{ID: body.ID, TaskID: body.ID, Owner: owner, Status: "queued", Stage: "queued", Kind: kind, CreatedAt: now, UpdatedAt: now}
	payload := documentMap(editableTaskPayload{Prompt: body.Prompt, Inputs: inputs})
	_, err = lease.MutateDocument(editablePrivateDocument(name, "filepayload-"), func(d map[string]any) error {
		for k, v := range payload {
			d[k] = v
		}
		return nil
	})
	if err == nil {
		_, err = lease.MutateDocument(name, func(d map[string]any) error {
			for k, v := range documentMap(task) {
				d[k] = v
			}
			return nil
		})
	}
	if err != nil {
		writeError(w, 503, "保存文件任务失败", "storage_error")
		return
	}
	writeJSON(w, 202, editableTaskPublic(&task))
}
func (s *Server) editableFileTaskByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/editable-file-tasks/"), "/")
	resume := strings.HasSuffix(path, "/resume-poll")
	id := strings.TrimSuffix(path, "/resume-poll")
	name := editableTaskDocument(s.authIdentity(r), id)
	doc, err := s.store.Document(name)
	if err != nil {
		writeError(w, 503, "读取文件任务失败", "storage_error")
		return
	}
	task, e := editableTaskDecode(doc)
	if e != nil || task.ID == "" {
		writeError(w, 404, "editable file task not found", "not_found")
		return
	}
	if !resume && r.Method == http.MethodDelete {
		lease, err := s.store.AcquireLease(r.Context(), "task:"+name, 120*time.Second)
		if err != nil {
			writeError(w, 503, "文件任务存储不可用", "storage_error")
			return
		}
		if lease == nil {
			writeError(w, 409, "task is running", "task_busy")
			return
		}
		defer lease.Release()
		doc, err = lease.MutateDocument(name, func(d map[string]any) error {
			if d["status"] != "success" && d["status"] != "error" {
				return errors.New("task is not terminal")
			}
			d["delete_pending"] = true
			return nil
		})
		if err != nil {
			writeError(w, 409, "任务尚未结束，不能删除", "task_not_terminal")
			return
		}
		task, _ = editableTaskDecode(doc)
		if err = s.deleteEditableTaskFiles(task); err == nil {
			for _, prefix := range []string{"filepayload-", "fileexport-"} {
				if err = s.store.DeleteDocument(editablePrivateDocument(name, prefix)); err != nil {
					break
				}
			}
		}
		if err == nil {
			err = s.store.DeleteDocument(name)
		}
		if err != nil {
			writeError(w, 503, "删除文件任务失败，可重试", "storage_error")
			return
		}
		writeJSON(w, 200, map[string]any{"task_id": task.ID, "deleted": true})
		return
	}
	if resume {
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed", "invalid_request_error")
			return
		}
		lease, err := s.store.AcquireLease(r.Context(), "task:"+name, 120*time.Second)
		if err != nil || lease == nil {
			writeError(w, 409, "task is running", "task_busy")
			return
		}
		defer lease.Release()
		doc, err = lease.MutateDocument(name, func(d map[string]any) error {
			if d["status"] == "success" || (stringValue(d["conversation_id"]) == "" && stringValue(d["export_document"]) == "") {
				return errors.New("task has no existing result")
			}
			d["status"] = "queued"
			d["error"] = ""
			d["error_code"] = ""
			d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			return nil
		})
		if err != nil {
			writeError(w, 409, "没有可恢复的上游会话", "not_resumable")
			return
		}
		task, _ = editableTaskDecode(doc)
	} else if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, 200, editableTaskPublic(&task))
}

func (s *Server) deleteEditableTaskFiles(task editableFileTaskState) error {
	root := filepath.Join(s.cfg.DataDir, "files")
	directory := filepath.Join(root, editableRelativeDirectory(task))
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if !regularFileWithin(root, path) {
			return fmt.Errorf("unexpected file in task output directory")
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(directory)
}
func (s *Server) editableTaskScheduler() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, 2)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
		}
		docs, err := s.store.PendingDocuments("filetask-", []string{"queued", "running"}, 100)
		if err != nil {
			continue
		}
	dispatch:
		for _, doc := range docs {
			select {
			case slots <- struct{}{}:
			default:
				break dispatch
			}
			task, e := editableTaskDecode(doc)
			if e != nil || task.ID == "" {
				<-slots
				continue
			}
			name := editableTaskDocument(task.Owner, task.ID)
			lease, err := s.store.AcquireLease(s.backgroundCtx, "task:"+name, 120*time.Second)
			if err != nil || lease == nil {
				<-slots
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots }()
				defer lease.Release()
				s.runEditableFileTask(s.backgroundCtx, name, lease)
			}()
		}
	}
}
func (s *Server) runEditableFileTask(parent context.Context, name string, lease *store.Lease) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Minute)
	defer cancel()
	lease.KeepAlive(ctx, 120*time.Second, cancel)
	var optionsErr error
	ctx, optionsErr = s.imageOptionsContext(ctx)
	if optionsErr != nil {
		return
	}
	doc, err := s.store.Document(name)
	task, e := editableTaskDecode(doc)
	if err != nil || e != nil || (task.Status != "queued" && task.Status != "running") {
		return
	}
	task.Status = "running"
	if task.StartedAt.IsZero() {
		task.StartedAt = time.Now().UTC()
	}
	save := func() error {
		if parent.Err() != nil {
			return parent.Err()
		}
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_, err := lease.MutateDocument(name, func(d map[string]any) error {
			for k, v := range documentMap(task) {
				d[k] = v
			}
			return nil
		})
		return err
	}
	if save() != nil {
		return
	}
	var exported provider.EditableExportResult
	if task.ExportDocument != "" {
		doc, err = s.store.Document(task.ExportDocument)
		if err == nil {
			raw, _ := json.Marshal(doc)
			err = json.Unmarshal(raw, &exported)
			if err == nil && (len(exported.Primary.Data) == 0 || len(exported.Archive.Data) == 0) {
				err = errors.New("文件任务导出结果不可读取")
			}
		}
	} else if task.Stage == "submitting" && task.ConversationID == "" {
		err = errors.New("上游提交结果不确定，未重复提交")
		task.ErrorCode = "editable_submission_uncertain"
	} else {
		var account accounts.Account
		var release func()
		if task.ConversationID != "" {
			account, ctx, release, err = s.reserveExistingEditableAccount(ctx, task.AccountID)
		} else {
			var picked *accounts.Lease
			picked, err = s.accountPool.ReserveMatchingLimit(ctx, []string{"basic", "super", "heavy"}, nil, func(a accounts.Account) bool {
				if !isOpenAIAccount(a) || accounts.CredentialReadiness(a.Fields, time.Now(), 25*time.Minute+30*time.Second).State != "ready" {
					return false
				}
				if until, e := time.Parse(time.RFC3339, stringValue(a.Fields["upload_cooldown_until"])); e == nil && until.After(time.Now()) {
					return false
				}
				plan := strings.ToLower(firstNonEmpty(stringValue(a.Fields["plan_type"]), stringValue(a.Fields["account_plan_type"]), stringValue(a.Fields["subscription_plan"])))
				return plan == "plus" || plan == "team" || plan == "pro" || plan == "enterprise"
			}, s.imagePolicy(ctx).AccountLimit)
			if err == nil {
				account = picked.Account
				ctx = picked.Context
				release = func() { s.accountPool.Release(picked) }
			}
		}
		if release != nil {
			defer release()
		}
		if err == nil {
			ctx = provider.WithImageCheckpoint(ctx, func(event provider.ImageCheckpoint) error {
				task.Stage = event.Stage
				task.AccountID = stringValue(event.Account.Fields["id"])
				if event.ConversationID != "" {
					task.ConversationID = event.ConversationID
				}
				return save()
			})
			if task.ConversationID != "" {
				exported, err = s.openAIImage.ResumeEditable(ctx, account, task.ConversationID, task.Kind)
			} else {
				payloadDoc, payloadErr := s.store.Document(editablePrivateDocument(name, "filepayload-"))
				raw, _ := json.Marshal(payloadDoc)
				var payload editableTaskPayload
				if payloadErr != nil {
					err = payloadErr
				} else if json.Unmarshal(raw, &payload) != nil || strings.TrimSpace(payload.Prompt) == "" {
					err = errors.New("文件任务输入不可读取")
				} else {
					exported, err = s.openAIImage.ExportEditable(ctx, account, task.Kind, payload.Prompt, payload.Inputs)
				}
			}
		}
		if err == nil {
			exportDocument := editablePrivateDocument(name, "fileexport-")
			_, err = lease.MutateDocument(exportDocument, func(d map[string]any) error {
				for k, v := range documentMap(exported) {
					d[k] = v
				}
				return nil
			})
			if err == nil {
				task.ExportDocument = exportDocument
				task.Stage = "result_ready"
				err = save()
			}
		}
	}
	if parent.Err() != nil || (ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return
	}
	if err == nil {
		task.Result, err = s.saveEditableExport(&task, exported)
	}
	if err == nil {
		task.Status = "success"
		task.Stage = "done"
		task.Error = ""
		task.ErrorCode = ""
	} else {
		task.Status = "error"
		task.Error = safeRefreshError(err)
		if task.ErrorCode == "" {
			task.ErrorCode = "editable_export_failed"
		}
	}
	_ = save()
}
func (s *Server) reserveExistingEditableAccount(parent context.Context, id string) (accounts.Account, context.Context, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	fail := func(err error) (accounts.Account, context.Context, func(), error) {
		cancel()
		return accounts.Account{}, parent, nil, err
	}
	fields, err := s.store.FindAccount(id)
	if err != nil {
		return fail(err)
	}
	if accounts.CredentialReadiness(fields, time.Now(), time.Minute).State != "ready" {
		return fail(errors.New("恢复原会话需要先续期原账号"))
	}
	var slot *store.Lease
	for i := 0; i < s.imagePolicy(ctx).AccountLimit; i++ {
		slot, err = s.store.AcquireLease(ctx, store.SlotResource(fields, i), 120*time.Second)
		if err != nil {
			return fail(err)
		}
		if slot != nil {
			break
		}
	}
	if slot == nil {
		return fail(accounts.ErrUnavailable)
	}
	slot.KeepAlive(ctx, 120*time.Second, cancel)
	gate, err := s.store.AcquireLease(ctx, "account-use:"+store.AccountResource(fields), 15*time.Second)
	if err != nil || gate == nil {
		slot.Release()
		if err == nil {
			err = accounts.ErrUnavailable
		}
		return fail(err)
	}
	fields, err = s.store.FindAccount(id)
	gate.Release()
	if err != nil || accounts.CredentialReadiness(fields, time.Now(), time.Minute).State != "ready" {
		slot.Release()
		if err == nil {
			err = accounts.ErrUnavailable
		}
		return fail(err)
	}
	return accounts.Account{Token: accountToken(fields), Pool: stringValue(fields["pool"]), Fields: fields}, ctx, func() { cancel(); slot.Release() }, nil
}
