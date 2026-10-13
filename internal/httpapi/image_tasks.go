package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

type imageTaskOwnerKey struct{}
type imageTaskPublicBaseKey struct{}
type imageTaskOutput struct {
	Stage           string              `json:"stage"`
	AccountID       string              `json:"account_id,omitempty"`
	ConversationID  string              `json:"conversation_id,omitempty"`
	ResultsDocument string              `json:"results_document,omitempty"`
	Data            []map[string]string `json:"data,omitempty"`
	Error           string              `json:"error,omitempty"`
	ErrorCode       string              `json:"error_code,omitempty"`
}
type imageTaskState struct {
	ID         string            `json:"id"`
	OwnerID    string            `json:"owner_id"`
	Status     string            `json:"status"`
	Mode       string            `json:"mode"`
	Model      string            `json:"model"`
	N          int               `json:"n"`
	Size       string            `json:"size"`
	Quality    string            `json:"quality"`
	PublicBase string            `json:"public_base"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
	Outputs    []imageTaskOutput `json:"outputs"`
}
type imageTaskPayload struct {
	Prompt string                      `json:"prompt"`
	Inputs []provider.OpenAIImageInput `json:"inputs,omitempty"`
}

func imageTaskDocument(owner, id string) string {
	return fmt.Sprintf("imagetask-%x", sha256.Sum256([]byte(owner+"\x00"+id)))
}
func encodeImageInput(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
func taskDecode(doc map[string]any) (imageTaskState, error) {
	var task imageTaskState
	raw, err := json.Marshal(doc)
	if err == nil {
		err = json.Unmarshal(raw, &task)
	}
	return task, err
}
func taskMap(task imageTaskState) map[string]any {
	raw, _ := json.Marshal(task)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return doc
}

func imageTaskPublic(task imageTaskState) map[string]any {
	result := map[string]any{"id": task.ID, "status": task.Status, "mode": task.Mode, "model": task.Model, "n": task.N, "size": task.Size, "quality": task.Quality, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "can_resume_poll": false}
	data := []map[string]string{}
	done := 0
	for _, out := range task.Outputs {
		data = append(data, out.Data...)
		if out.Stage == "done" {
			done++
		}
		if (out.ResultsDocument != "" || (out.ConversationID != "" && !provider.IsCodexImageModel(task.Model))) && out.Stage != "done" {
			result["can_resume_poll"] = true
			result["conversation_id"] = out.ConversationID
		}
		if out.Error != "" {
			result["error"] = out.Error
			result["error_code"] = out.ErrorCode
		}
		if out.Stage != "done" {
			result["stage"] = out.Stage
		}
	}
	result["data"] = data
	result["progress"] = fmt.Sprintf("%d/%d", done, task.N)
	return result
}

func (s *Server) imageTasksAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost {
		var body struct {
			ClientID string `json:"client_task_id"`
			Prompt   string `json:"prompt"`
			Model    string `json:"model"`
			N        int    `json:"n"`
			Size     string `json:"size"`
			Quality  string `json:"quality"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		s.createImageTask(w, r, imageTaskState{ID: body.ClientID, Mode: "generate", Model: body.Model, N: body.N, Size: body.Size, Quality: body.Quality}, imageTaskPayload{Prompt: body.Prompt})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	owner := s.authIdentity(r)
	items := []map[string]any{}
	missing := []string{}
	if ids := strings.TrimSpace(r.URL.Query().Get("ids")); ids != "" {
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			doc, err := s.store.Document(imageTaskDocument(owner, id))
			task, e := taskDecode(doc)
			if err != nil || e != nil || task.ID == "" {
				missing = append(missing, id)
			} else {
				items = append(items, imageTaskPublic(task))
			}
		}
	} else {
		docs, err := s.store.ListDocuments("imagetask-", owner, nil, 100)
		if err != nil {
			writeError(w, 503, "读取任务失败", "storage_error")
			return
		}
		for _, doc := range docs {
			task, err := taskDecode(doc)
			if err == nil {
				items = append(items, imageTaskPublic(task))
			}
		}
	}
	quota, err := s.imageQuota()
	if err != nil {
		writeError(w, 503, "读取额度失败", "storage_error")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing, "quota_summary": quota})
}

func (s *Server) imageTaskEdits(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if err := r.ParseMultipartForm(112 << 20); err != nil {
		writeError(w, 400, "invalid multipart form", "invalid_request_error")
		return
	}
	defer r.MultipartForm.RemoveAll()
	request, err := s.parseImageEditRequest(r)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if request.HasMask {
		writeError(w, 400, "mask is not supported", "invalid_request_error")
		return
	}
	s.createImageTask(w, r, imageTaskState{ID: r.FormValue("client_task_id"), Mode: "edit", Model: request.Model, N: request.N, Size: request.Size, Quality: request.Quality}, imageTaskPayload{Prompt: request.Prompt, Inputs: request.Inputs})
}

func (s *Server) createImageTask(w http.ResponseWriter, r *http.Request, task imageTaskState, payload imageTaskPayload) {
	task.ID = strings.TrimSpace(task.ID)
	payload.Prompt = strings.TrimSpace(payload.Prompt)
	if task.ID == "" || len(task.ID) > 128 || strings.ContainsAny(task.ID, "/\\") || payload.Prompt == "" {
		writeError(w, 400, "valid client_task_id and prompt are required", "invalid_request_error")
		return
	}
	task.OwnerID = s.authIdentity(r)
	task.Model = firstNonEmpty(task.Model, "gpt-image-2")
	task.N = max(1, task.N)
	task.Size = firstNonEmpty(task.Size, "1024x1024")
	task.Quality = firstNonEmpty(task.Quality, "auto")
	spec, ok := model.Find(s.catalog, task.Model)
	capability := model.Image
	if task.Mode == "edit" {
		capability = model.ImageEdit
	}
	if !ok || spec.Capability&capability == 0 || task.N > 10 || (task.Mode == "edit" && (len(payload.Inputs) == 0 || len(payload.Inputs) > 7)) {
		writeError(w, 400, "invalid image task model, count or inputs", "invalid_request_error")
		return
	}
	if isOpenAIImageModel(task.Model) {
		task.Size = strings.TrimSpace(task.Size)
		if !validOpenAIImageSize(task.Size) {
			writeError(w, 400, "invalid image size", "invalid_request_error")
			return
		}
	}
	task.PublicBase = requestPublicBase(r)
	task.Status = "queued"
	task.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	task.UpdatedAt = task.CreatedAt
	for i := 0; i < task.N; i++ {
		task.Outputs = append(task.Outputs, imageTaskOutput{Stage: "queued"})
	}
	name := imageTaskDocument(task.OwnerID, task.ID)
	// The payload is written under the same creation lease before publishing the job.
	lease, err := s.store.AcquireLease(r.Context(), "task-create:"+name, 30*time.Second)
	if err != nil || lease == nil {
		writeError(w, 409, "task is being created; retry with the same client_task_id", "task_busy")
		return
	}
	defer lease.Release()
	previous, err := s.store.Document(name)
	if err != nil {
		writeError(w, 503, "读取任务失败", "storage_error")
		return
	}
	if previous["id"] != nil {
		old, _ := taskDecode(previous)
		writeJSON(w, 202, imageTaskPublic(old))
		return
	}
	raw, _ := json.Marshal(payload)
	var payloadDoc map[string]any
	_ = json.Unmarshal(raw, &payloadDoc)
	_, err = lease.MutateDocument("imagepayload-"+strings.TrimPrefix(name, "imagetask-"), func(d map[string]any) error {
		for k, v := range payloadDoc {
			d[k] = v
		}
		return nil
	})
	if err == nil {
		_, err = lease.MutateDocument(name, func(d map[string]any) error {
			for k, v := range taskMap(task) {
				d[k] = v
			}
			return nil
		})
	}
	if err != nil {
		writeError(w, 503, "保存图片任务失败", "storage_error")
		return
	}
	writeJSON(w, 202, imageTaskPublic(task))
}

func (s *Server) imageTaskByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/image-tasks/"), "/")
	resume := strings.HasSuffix(path, "/resume-poll")
	id := strings.TrimSuffix(path, "/resume-poll")
	name := imageTaskDocument(s.authIdentity(r), id)
	doc, err := s.store.Document(name)
	task, e := taskDecode(doc)
	if err != nil || e != nil || task.ID == "" {
		writeError(w, 404, "image task not found", "not_found")
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
			current, e := taskDecode(d)
			if e != nil {
				return e
			}
			resumable := false
			for i := range current.Outputs {
				out := &current.Outputs[i]
				if (out.ResultsDocument != "" || (out.ConversationID != "" && !provider.IsCodexImageModel(current.Model))) && out.Stage != "done" {
					out.Stage = "result_ready"
					out.Error = ""
					out.ErrorCode = ""
					resumable = true
				}
			}
			if !resumable {
				return errors.New("no existing conversation to resume")
			}
			current.Status = "queued"
			current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			for k, v := range taskMap(current) {
				d[k] = v
			}
			return nil
		})
		if err != nil {
			writeError(w, 409, "没有可恢复的上游会话", "not_resumable")
			return
		}
		task, _ = taskDecode(doc)
	} else if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, 200, imageTaskPublic(task))
}
func (s *Server) imageTaskQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	quota, err := s.imageQuota()
	if err != nil {
		writeError(w, 503, "读取额度失败", "storage_error")
		return
	}
	writeJSON(w, 200, quota)
}

func (s *Server) imageTaskScheduler() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, 4)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
		}
		docs, err := s.store.PendingDocuments("imagetask-", []string{"queued", "running"}, 100)
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
			task, err := taskDecode(doc)
			if err != nil {
				<-slots
				continue
			}
			name := imageTaskDocument(task.OwnerID, task.ID)
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
				s.runImageTask(s.backgroundCtx, name, lease)
			}()
		}
	}
}

func (s *Server) runImageTask(parent context.Context, name string, lease *store.Lease) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	lease.KeepAlive(ctx, 120*time.Second, cancel)
	var optionsErr error
	ctx, optionsErr = s.imageOptionsContext(ctx)
	if optionsErr != nil {
		return
	}
	doc, err := s.store.Document(name)
	task, e := taskDecode(doc)
	if err != nil || e != nil || (task.Status != "queued" && task.Status != "running") {
		return
	}
	payloadDoc, err := s.store.Document("imagepayload-" + strings.TrimPrefix(name, "imagetask-"))
	raw, _ := json.Marshal(payloadDoc)
	var payload imageTaskPayload
	if err != nil || json.Unmarshal(raw, &payload) != nil || strings.TrimSpace(payload.Prompt) == "" {
		task.Status = "error"
		for i := range task.Outputs {
			task.Outputs[i].Stage = "error"
			task.Outputs[i].Error = "图片任务输入不可读取"
			task.Outputs[i].ErrorCode = "image_payload_missing"
		}
		_, _ = lease.MutateDocument(name, func(d map[string]any) error {
			for k, v := range taskMap(task) {
				d[k] = v
			}
			return nil
		})
		return
	}
	task.Status = "running"
	save := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_, err := lease.MutateDocument(name, func(d map[string]any) error {
			for k, v := range taskMap(task) {
				d[k] = v
			}
			return nil
		})
		return err
	}
	if save() != nil {
		return
	}
	for i := range task.Outputs {
		out := &task.Outputs[i]
		if out.Stage == "done" || out.Stage == "error" {
			continue
		}
		if out.Stage == "submitting" && out.ConversationID == "" {
			out.Stage = "error"
			out.Error = "上游提交结果不确定，未重新生成"
			out.ErrorCode = "image_submission_uncertain"
			if save() != nil {
				return
			}
			continue
		}
		deliveryCtx := context.WithValue(ctx, imageDeliveryKey{}, fmt.Sprintf("%s:%d", name, i))
		flowCtx := provider.WithImageCheckpoint(context.WithValue(context.WithValue(deliveryCtx, imageTaskOwnerKey{}, task.OwnerID), imageTaskPublicBaseKey{}, task.PublicBase), func(event provider.ImageCheckpoint) error {
			if len(event.Results) > 0 {
				resultName := fmt.Sprintf("imageresult-%s-%d", strings.TrimPrefix(name, "imagetask-"), i)
				_, err := lease.MutateDocument(resultName, func(d map[string]any) error { d["images"] = event.Results; return nil })
				if err != nil {
					return err
				}
				out.ResultsDocument = resultName
			}
			out.Stage = event.Stage
			out.AccountID = stringValue(event.Account.Fields["id"])
			if event.ConversationID != "" {
				out.ConversationID = event.ConversationID
			}
			return save()
		})
		var data []map[string]string
		var flowErr error
		target := "/v1/images/generations"
		if task.Mode == "edit" {
			target = "/v1/images/edits"
		}
		body := map[string]any{"model": task.Model, "prompt": payload.Prompt, "size": task.Size, "quality": task.Quality, "n": 1, "response_format": "url"}
		inputs := []map[string]any{}
		for _, input := range payload.Inputs {
			inputs = append(inputs, map[string]any{"b64_json": encodeImageInput(input.Data), "mime_type": input.MIME, "filename": input.Name})
		}
		body["image"] = inputs
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(flowCtx, http.MethodPost, "http://internal"+target, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		handler := func(w http.ResponseWriter, r *http.Request) {
			if out.ResultsDocument != "" {
				data, flowErr = s.deliverSavedImageTask(r, out, task.Size, task.PublicBase)
				if flowErr != nil {
					s.writeImageError(w, r, flowErr)
				} else {
					writeJSON(w, 200, map[string]any{"data": data})
				}
				return
			}
			if out.ConversationID != "" {
				data, flowErr = s.resumeImageTaskOutput(r, out, payload.Inputs, task.PublicBase, task.Size)
				if flowErr != nil {
					s.writeImageError(w, r, flowErr)
				} else {
					writeJSON(w, 200, map[string]any{"data": data})
				}
				return
			}
			if !isOpenAIImageModel(task.Model) {
				out.Stage = "submitting"
				if err := save(); err != nil {
					flowErr = err
					return
				}
			}
			if task.Mode == "edit" {
				s.imageEdits(w, r)
			} else {
				s.imageGenerations(w, r)
			}
		}
		recorder := &responseCapture{header: make(http.Header)}
		s.withRequestMonitor(recorder, req, http.HandlerFunc(handler))
		if ctx.Err() != nil {
			return
		} // Keep the checkpoint for the next worker after shutdown/lost lease.
		var response struct {
			Data  []map[string]string `json:"data"`
			Error map[string]any      `json:"error"`
		}
		decodeErr := json.Unmarshal(recorder.body.Bytes(), &response)
		if recorder.status >= 200 && recorder.status < 300 && decodeErr == nil && len(response.Data) > 0 {
			out.Data = response.Data
			out.Stage = "done"
			out.Error = ""
			out.ErrorCode = ""
		} else {
			out.Stage = "error"
			out.Error = firstNonEmpty(stringValue(response.Error["message"]), "图片任务执行失败")
			out.ErrorCode = firstNonEmpty(stringValue(response.Error["type"]), "image_generation_failed")
		}
		if save() != nil {
			return
		}
	}
	task.Status = "success"
	for _, out := range task.Outputs {
		if out.Stage != "done" {
			task.Status = "error"
		}
	}
	_ = save()
}

// Inline Codex results are durable before quota accounting and delivery. This
// recovery path needs no AT, no upstream call and no new generation.
func (s *Server) deliverSavedImageTask(r *http.Request, out *imageTaskOutput, size, publicBase string) ([]map[string]string, error) {
	doc, err := s.store.Document(out.ResultsDocument)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(doc["images"])
	if err != nil {
		return nil, err
	}
	var images []provider.ImageResult
	if json.Unmarshal(raw, &images) != nil || len(images) == 0 {
		return nil, errors.New("saved image result unavailable")
	}
	receipt, _ := r.Context().Value(imageDeliveryKey{}).(string)
	if receipt == "" {
		return nil, errors.New("image recovery receipt is required")
	}
	if err = s.store.ConsumeImageQuota(out.AccountID, receipt); err != nil {
		return nil, err
	}
	fields, err := s.store.FindAccount(out.AccountID)
	if err != nil {
		return nil, err
	}
	a := accounts.Account{Fields: fields, Token: accountToken(fields)}
	value, localURL, err := s.resolveOpenAIImage(r.Context(), a, images[0], size, "url", publicBase)
	if err != nil {
		return nil, &provider.ImageFlowError{Stage: "storage", Submitted: true, Cause: err}
	}
	s.recordGeneratedMedia(r.Context(), map[string]string{"url": localURL})
	return []map[string]string{value}, nil
}

func (s *Server) resumeImageTaskOutput(r *http.Request, out *imageTaskOutput, inputs []provider.OpenAIImageInput, publicBase string, sizes ...string) ([]map[string]string, error) {
	configured, optionsErr := s.imageOptionsContext(r.Context())
	if optionsErr != nil {
		return nil, optionsErr
	}
	ctx, cancel := context.WithTimeout(configured, s.imageTotalBudget(configured, 1))
	defer cancel()
	release, err := s.acquireImageSlot(ctx, r)
	if err != nil {
		return nil, err
	}
	defer release()
	fields, err := s.store.FindAccount(out.AccountID)
	if err != nil {
		return nil, err
	}
	// Existing results do not require new quota. Reserve the same account's slot.
	if accounts.CredentialReadiness(fields, time.Now(), time.Minute).State != "ready" {
		return nil, errors.New("恢复原会话需要有效凭据；请先续期原账号")
	}
	var accountLease *store.Lease
	for slot := 0; slot < s.imagePolicy(ctx).AccountLimit; slot++ {
		accountLease, err = s.store.AcquireLease(ctx, store.SlotResource(fields, slot), 120*time.Second)
		if err != nil {
			return nil, err
		}
		if accountLease != nil {
			break
		}
	}
	if accountLease == nil {
		return nil, accounts.ErrUnavailable
	}
	defer accountLease.Release()
	accountLease.KeepAlive(ctx, 120*time.Second, cancel)
	gate, err := s.store.AcquireLease(ctx, "account-use:"+store.AccountResource(fields), 15*time.Second)
	if err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, accounts.ErrUnavailable
	}
	fields, err = s.store.FindAccount(out.AccountID)
	gate.Release()
	if err != nil {
		return nil, err
	}
	if accounts.CredentialReadiness(fields, time.Now(), time.Minute).State != "ready" {
		return nil, accounts.ErrUnavailable
	}
	account := accounts.Account{Token: firstNonEmpty(stringValue(fields["access_token"]), stringValue(fields["accessToken"])), Pool: stringValue(fields["pool"]), Fields: fields}
	s.enrichMonitorAccount(r, account)
	parent := ctx
	ctx = provider.WithImageCheckpoint(s.monitorOpenAIImageContext(r, ctx), func(event provider.ImageCheckpoint) error {
		if event.Stage == "result_ready" {
			if err := s.store.ConsumeImageQuota(out.AccountID, event.ConversationID); err != nil {
				return err
			}
		}
		return provider.NotifyImageCheckpoint(parent, event)
	})
	images, err := s.openAIImage.ResumeConversation(ctx, account, out.ConversationID, inputs)
	if err != nil {
		return nil, err
	}
	data := []map[string]string{}
	for _, image := range images {
		size := ""
		if len(sizes) > 0 {
			size = sizes[0]
		}
		value, localURL, e := s.resolveOpenAIImage(ctx, account, image, size, "url", publicBase)
		if e != nil {
			return nil, &provider.ImageFlowError{Stage: "storage", ConversationID: out.ConversationID, Submitted: true, Cause: e}
		}
		s.recordGeneratedMedia(ctx, map[string]string{"url": localURL})
		data = append(data, value)
		break
	}
	return data, nil
}
