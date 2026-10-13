package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/remotestore"
)

func genboxTimeout(b map[string]any) time.Duration {
	return imageSeconds(b, "timeout_secs", 60, 5, 120)
}

func genboxName(rel string) string { return fmt.Sprintf("genbox-%x", sha256.Sum256([]byte(rel))) }
func genboxDestination(b map[string]any) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimRight(stringValue(b["base_url"]), "/")+"\n"+stringValue(b["source_id"]))))
}
func (s *Server) genboxConfig() (map[string]any, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return nil, err
	}
	b := mapValue(cfg["genbox_push"])
	u, e := url.Parse(stringValue(b["base_url"]))
	if !boolValue(b["enabled"], false) || e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || stringValue(b["source_id"]) == "" || stringValue(b["push_key"]) == "" {
		return nil, fmt.Errorf("GenBox 尚未启用或配置不完整")
	}
	return b, nil
}
func (s *Server) queueGenbox(rel string, automatic bool) error {
	if !validImageRel(rel) {
		return fmt.Errorf("invalid image path")
	}
	b, err := s.genboxConfig()
	if err != nil {
		return err
	}
	if automatic && !boolValue(b["auto_push_after_studio"], false) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := s.store.AcquireLease(ctx, "genbox-push:"+genboxName(rel), 30*time.Second)
	if err != nil {
		return err
	}
	if lease == nil {
		return fmt.Errorf("GenBox push already running")
	}
	defer lease.Release()
	_, err = lease.MutateDocument(genboxName(rel), func(d map[string]any) error {
		if stringValue(d["destination"]) != "" && stringValue(d["destination"]) != genboxDestination(b) {
			return fmt.Errorf("GenBox destination changed; previous receipt retained")
		}
		if genboxTerminal(stringValue(d["status"])) || stringValue(d["status"]) == "submitting" {
			return nil
		}
		d["path"] = rel
		d["destination"] = genboxDestination(b)
		d["status"] = "queued"
		d["next_retry_at"] = ""
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	return err
}
func genboxTerminal(status string) bool {
	return status == "imported" || status == "already-imported" || status == "duplicate-local"
}
func genboxLabel(status string) string {
	switch status {
	case "imported":
		return "已推送到 GenBox"
	case "already-imported":
		return "GenBox 已存在"
	case "duplicate-local":
		return "GenBox 本地重复"
	case "submitting":
		return "正在推送"
	case "uncertain":
		return "回执待确认"
	case "queued":
		return "等待推送"
	}
	return "推送失败"
}

func genboxJSON(client *http.Client, req *http.Request) (map[string]any, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GenBox 请求失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GenBox 返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("GenBox 回执超过限制")
	}
	// Reject duplicate object keys and trailing JSON instead of accepting an
	// ambiguous receipt as proof that a source image was stored.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("receipt nesting exceeds limit")
		}
		token, e := decoder.Token()
		if e != nil {
			return e
		}
		if delimiter, ok := token.(json.Delim); ok {
			if delimiter == '{' {
				seen := map[string]bool{}
				for decoder.More() {
					key, e := decoder.Token()
					if e != nil {
						return e
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate receipt key")
					}
					seen[name] = true
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
			} else if delimiter == '[' {
				for decoder.More() {
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
			} else {
				return fmt.Errorf("invalid receipt")
			}
			_, e = decoder.Token()
			return e
		}
		return nil
	}
	if err = walk(0); err != nil {
		return nil, fmt.Errorf("GenBox 回执 JSON 无效")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("GenBox 回执 JSON 无效")
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, fmt.Errorf("GenBox 回执 JSON 无效")
	}
	return out, nil
}
func genboxRequest(ctx context.Context, b map[string]any, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(stringValue(b["base_url"]), "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-GenBox-Source", stringValue(b["source_id"]))
	req.Header.Set("X-GenBox-Key", stringValue(b["push_key"]))
	return req, nil
}
func (s *Server) pushGenbox(parent context.Context, rel string) (result map[string]any, err error) {
	if !validImageRel(rel) {
		return nil, fmt.Errorf("invalid image path")
	}
	b, err := s.genboxConfig()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, genboxTimeout(b))
	defer cancel()
	imageLease, e := s.store.AcquireLease(ctx, remoteMediaLease(rel), 30*time.Second)
	if e != nil {
		return nil, e
	}
	if imageLease == nil {
		return nil, fmt.Errorf("image storage busy")
	}
	defer imageLease.Release()
	imageLease.KeepAlive(ctx, 30*time.Second, cancel)
	lease, e := s.store.AcquireLease(ctx, "genbox-push:"+genboxName(rel), 30*time.Second)
	if e != nil {
		return nil, e
	}
	if lease == nil {
		return nil, fmt.Errorf("GenBox push already running")
	}
	defer lease.Release()
	lease.KeepAlive(ctx, 30*time.Second, cancel)
	state, err := s.store.Document(genboxName(rel))
	if err != nil {
		return nil, err
	}
	if stringValue(state["destination"]) != "" && stringValue(state["destination"]) != genboxDestination(b) {
		return nil, fmt.Errorf("GenBox destination changed")
	}
	if genboxTerminal(stringValue(state["status"])) {
		return genboxView(state), nil
	}
	defer func() {
		if err != nil {
			_, _ = lease.MutateDocument(genboxName(rel), func(d map[string]any) error {
				if stringValue(d["status"]) == "submitting" {
					d["status"] = "uncertain"
				}
				d["last_error"] = err.Error()
				d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				d["attempts"] = min(10, intValue(d["attempts"])+1)
				d["next_retry_at"] = time.Now().Add(time.Duration(min(300, 5*(1<<intValue(d["attempts"])))) * time.Second).UTC().Format(time.RFC3339Nano)
				return nil
			})
		}
	}()
	path := filepath.Join(s.cfg.ImageDataDir, rel)
	var raw []byte
	metadata := mediaMetadata(path)
	if regularFileWithin(s.cfg.ImageDataDir, path) {
		info, e := os.Stat(path)
		if e != nil || info.Size() > 64<<20 {
			return nil, fmt.Errorf("image exceeds push limits")
		}
		raw, err = os.ReadFile(path)
	} else {
		raw, err = s.remoteImageBytes(ctx, rel)
		doc, _ := s.store.Document(remoteMediaName(rel))
		metadata = mapValue(doc["metadata"])
	}
	if err != nil {
		return nil, fmt.Errorf("GenBox source image unavailable")
	}
	dimensions, _, e := image.DecodeConfig(bytes.NewReader(raw))
	if e != nil || dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > 8192 || dimensions.Height > 8192 || int64(dimensions.Width)*int64(dimensions.Height) > 32_000_000 {
		return nil, fmt.Errorf("GenBox source image format or dimensions invalid")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if old := stringValue(state["sha256"]); old != "" && old != digest {
		return nil, fmt.Errorf("source image changed since previous push")
	}
	client := remotestore.HTTPClient(genboxTimeout(b))
	req, err := genboxRequest(ctx, b, "GET", "/api/sync/push/status", nil)
	if err != nil {
		return nil, err
	}
	probe, err := genboxJSON(client, req)
	if err != nil {
		return nil, err
	}
	if probe["ok"] != true || probe["contract_version"] != "v1" || probe["source_id"] != stringValue(b["source_id"]) || intValue(probe["max_image_bytes"]) < len(raw) {
		return nil, fmt.Errorf("GenBox 连接验证未通过")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", rel)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(raw); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{"remote_path": rel, "source_sha256": digest, "prompt": stringValue(metadata["prompt"]), "created_at": firstNonEmpty(stringValue(metadata["created_at"]), stringValue(metadata["generated_at"])), "date": stringValue(metadata["date"]), "model": stringValue(metadata["model"])} {
		if value != "" {
			if err = writer.WriteField(key, value); err != nil {
				return nil, err
			}
		}
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	_, err = lease.MutateDocument(genboxName(rel), func(d map[string]any) error {
		d["path"] = rel
		d["status"] = "submitting"
		d["sha256"] = digest
		d["destination"] = genboxDestination(b)
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		return nil, err
	}
	req, err = genboxRequest(ctx, b, "POST", "/api/sync/push", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	receipt, err := genboxJSON(client, req)
	if err != nil {
		return nil, err
	}
	status := stringValue(receipt["status"])
	if receipt["ok"] != true || receipt["contract_version"] != "v1" || receipt["source_id"] != stringValue(b["source_id"]) || receipt["sha256"] != digest || !genboxTerminal(status) {
		return nil, fmt.Errorf("GenBox 回执无效")
	}
	doc, err := lease.MutateDocument(genboxName(rel), func(d map[string]any) error {
		d["status"] = status
		d["last_error"] = ""
		d["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return genboxView(doc), nil
}
func genboxView(d map[string]any) map[string]any {
	return map[string]any{"path": d["path"], "status": d["status"], "label": genboxLabel(stringValue(d["status"])), "sha256": d["sha256"], "updated_at": d["updated_at"], "source_retained": true}
}

func (s *Server) genboxPushAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.queueGenbox(body.Path, false); err != nil {
		writeError(w, 409, err.Error(), "genbox_not_configured")
		return
	}
	result, err := s.pushGenbox(r.Context(), body.Path)
	if err != nil {
		writeError(w, 502, err.Error(), "genbox_push_failed")
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) genboxScheduler() {
	if strings.EqualFold(os.Getenv("GO_REGISTRATION_WORKER"), "true") {
		return
	}
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-timer.C:
		}
		lease, err := s.store.AcquireLease(s.backgroundCtx, "genbox-worker", 60*time.Second)
		if err != nil || lease == nil {
			continue
		}
		ctx, cancel := context.WithCancel(s.backgroundCtx)
		lease.KeepAlive(ctx, 60*time.Second, cancel)
		docs, err := s.store.PendingDueDocuments("genbox-", []string{"queued", "submitting", "uncertain"}, 4)
		if err == nil {
			for _, d := range docs {
				if ctx.Err() != nil {
					break
				}
				rel := stringValue(d["path"])
				// v1 identifies images by source_sha256 and explicitly returns
				// already-imported/duplicate-local. Recover a lost receipt by resending
				// the exact same source/hash, never by selecting a replacement image.
				_, _ = s.pushGenbox(ctx, rel)
			}
		}
		cancel()
		lease.Release()
	}
}

func (s *Server) decorateGenbox(items []map[string]any) error {
	docs, err := s.store.AllDocuments("genbox-", nil)
	if err != nil {
		return err
	}
	cfg, err := s.store.Config()
	if err != nil {
		return err
	}
	enabled := boolValue(mapValue(cfg["genbox_push"])["enabled"], false)
	states := map[string]map[string]any{}
	for _, d := range docs {
		states[stringValue(d["path"])] = genboxView(d)
	}
	for _, item := range items {
		item["genbox_enabled"] = enabled
		if state := states[stringValue(item["path"])]; state != nil {
			item["genbox_push"] = state
		}
	}
	return nil
}
