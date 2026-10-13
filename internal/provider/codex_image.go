package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func IsCodexImageModel(model string) bool {
	switch model {
	case "codex-gpt-image-2", "plus-codex-gpt-image-2", "team-codex-gpt-image-2", "pro-codex-gpt-image-2":
		return true
	}
	return false
}

// Codex uses Responses with inline image output. It does not use Web uploads,
// sentinel requirements, or Web conversation polling.
func (o *OpenAIImage) GenerateCodex(ctx context.Context, a accounts.Account, prompt, model, size, quality string, inputs []OpenAIImageInput) (results []ImageResult, err error) {
	stage, submitted := "validation", false
	defer func() {
		if err != nil {
			var upstream *protocol.UpstreamError
			if stage == "generation" && errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status < 500 {
				submitted = false
			}
			err = &ImageFlowError{Stage: stage, Submitted: submitted, Cause: err}
		}
	}()
	if !IsCodexImageModel(model) || strings.TrimSpace(a.Token) == "" || strings.TrimSpace(prompt) == "" || a.Fields["source_type"] != "codex" {
		return nil, errors.New("Codex image generation requires a Codex account and prompt")
	}
	if size == "" || size == "auto" {
		size = "1024x1024"
	}
	if quality == "" {
		quality = "auto"
	}
	if o.Proxy != nil {
		stage = "egress"
		started := time.Now()
		lease, e := o.Proxy.AcquireImageContext(ctx, a.Fields)
		notifyOpenAIImageStage(ctx, "egress_acquire_ms", started)
		if e != nil {
			return nil, e
		}
		if lease.Source == "unavailable" {
			return nil, errors.New("Codex image egress unavailable")
		}
		ctx = proxyruntime.WithURL(ctx, lease.URL)
		notifyOpenAIImageEgress(ctx, lease.URL)
		defer func() { lease.Release(openAIImageProxyFailure(err)) }()
	}
	content := []map[string]any{{"type": "input_text", "text": prompt}}
	for _, input := range inputs {
		if len(input.Data) == 0 || len(input.Data) > 32<<20 {
			return nil, errors.New("invalid Codex input image size")
		}
		mime := input.MIME
		if mime == "" {
			mime = http.DetectContentType(input.Data)
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(input.Data)})
	}
	action := "generate"
	if len(inputs) > 0 {
		action = "edit"
	}
	payload := map[string]any{
		"model": "gpt-5.5", "instructions": "Use the image_generation tool to create exactly one image for the user's request. Return the generated image result.",
		"store": false, "stream": true, "input": []map[string]any{{"role": "user", "content": content}},
		"tools":       []map[string]any{{"type": "image_generation", "model": "gpt-image-2", "action": action, "size": size, "quality": quality, "output_format": "png"}},
		"tool_choice": map[string]any{"type": "image_generation"},
	}
	raw, e := json.Marshal(payload)
	if e != nil {
		return nil, e
	}
	stage = "generation"
	if timeout := imageOptions(ctx).StreamTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err = imageCheckpoint(ctx, ImageCheckpoint{Stage: "submitting", Account: a}); err != nil {
		return nil, err
	}
	submitted = true
	started := time.Now()
	const path = "/backend-api/codex/responses"
	response, e := o.doRequest(ctx, http.MethodPost, o.BaseURL+path, path, a, bytes.NewReader(raw), map[string]string{"Authorization": "Bearer " + a.Token, "Content-Type": "application/json"}, true, false)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	notifyOpenAIImageStage(ctx, "generation_start_ms", started)
	results, err = readCodexImages(response.Body, response.Header.Get("Content-Type"))
	notifyOpenAIImageStage(ctx, "conversation_stream_ms", started)
	if err != nil {
		return nil, err
	}
	stage = "storage"
	if err = imageCheckpoint(ctx, ImageCheckpoint{Stage: "result_ready", Account: a, Results: results}); err != nil {
		return nil, err
	}
	return results, nil
}

func readCodexImages(body io.Reader, contentType string) ([]ImageResult, error) {
	// A generated image event can be much larger than the default Scanner limit.
	reader := io.LimitReader(body, 96<<20)
	var result string
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 16 || result != "" {
			return
		}
		switch x := value.(type) {
		case map[string]any:
			if x["type"] == "image_generation_call" {
				if v, ok := x["result"].(string); ok {
					result = v
					return
				}
			}
			for _, v := range x {
				walk(v, depth+1)
			}
		case []any:
			for _, v := range x {
				walk(v, depth+1)
			}
		}
	}
	if strings.Contains(contentType, "application/json") {
		var value any
		if err := json.NewDecoder(reader).Decode(&value); err != nil {
			return nil, fmt.Errorf("decode Codex response: %w", err)
		}
		walk(value, 0)
	} else {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 90<<20)
		var event bytes.Buffer
		decode := func() error {
			if event.Len() == 0 {
				return nil
			}
			raw := bytes.TrimSpace(event.Bytes())
			defer event.Reset()
			if bytes.Equal(raw, []byte("[DONE]")) {
				return nil
			}
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return errors.New("invalid Codex response event")
			}
			walk(value, 0)
			return nil
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				if err := decode(); err != nil {
					return nil, err
				}
				if result != "" {
					break
				}
				continue
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				if event.Len() > 0 {
					event.WriteByte('\n')
				}
				event.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
			}
		}
		if result == "" {
			if err := scanner.Err(); err != nil {
				return nil, err
			}
			if err := decode(); err != nil {
				return nil, err
			}
		}
	}
	if result == "" {
		return nil, errors.New("Codex response completed without an image result")
	}
	raw, err := base64.StdEncoding.DecodeString(result)
	if err != nil || len(raw) == 0 || len(raw) > 64<<20 {
		return nil, errors.New("invalid Codex image result")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 32_000_000 {
		return nil, errors.New("invalid Codex image dimensions")
	}
	mime := "image/" + format
	if format == "jpg" {
		mime = "image/jpeg"
	}
	return []ImageResult{imageResultBytes(raw, mime)}, nil
}
