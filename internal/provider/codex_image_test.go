package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func TestCodexImageProtocolAndDurableCheckpoint(t *testing.T) {
	const image = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a0t8AAAAASUVORK5CYII="
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.URL.Path != "/backend-api/codex/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("Cookie") != "" {
			t.Error("incorrect Codex route or auth")
		}
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		tool := p["tools"].([]any)[0].(map[string]any)
		if tool["action"] != "edit" || tool["model"] != "gpt-image-2" || p["model"] != "gpt-5.5" || p["store"] != false {
			t.Error("incorrect tool parameters")
		}
		content := p["input"].([]any)[0].(map[string]any)["content"].([]any)
		if !strings.HasPrefix(content[1].(map[string]any)["image_url"].(string), "data:image/png;base64,") {
			t.Error("input was not inline")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":%q}}\n\n", image)
	}))
	defer upstream.Close()
	p := NewOpenAIImage(upstream.URL, upstream.Client(), nil, 0)
	a := accounts.Account{Token: "synthetic", Fields: map[string]any{"source_type": "codex"}}
	png, _ := base64.StdEncoding.DecodeString(image)
	checkpoints := []string{}
	ctx := WithImageCheckpoint(context.Background(), func(c ImageCheckpoint) error {
		checkpoints = append(checkpoints, c.Stage)
		if c.Stage == "result_ready" && (len(c.Results) != 1 || c.ConversationID != "") {
			t.Error("missing durable inline output")
		}
		return nil
	})
	result, err := p.GenerateCodex(ctx, a, "edit", "codex-gpt-image-2", "1024x1024", "auto", []OpenAIImageInput{{Data: png, MIME: "image/png"}})
	if err != nil || len(result) != 1 || result[0].Base64 != image || posts.Load() != 1 || strings.Join(checkpoints, ",") != "submitting,result_ready" {
		t.Fatalf("flow: %v %v %v", result, err, checkpoints)
	}
	// A checkpoint failure after a result exists must never permit regeneration.
	ctx = WithImageCheckpoint(context.Background(), func(c ImageCheckpoint) error {
		if c.Stage == "result_ready" {
			return errors.New("storage unavailable")
		}
		return nil
	})
	_, err = p.GenerateCodex(ctx, a, "edit", "codex-gpt-image-2", "", "", []OpenAIImageInput{{Data: png, MIME: "image/png"}})
	var flow *ImageFlowError
	if !errors.As(err, &flow) || !flow.Submitted {
		t.Fatal("delivery failure allowed a new generation")
	}
}

func TestCodexInterruptedAndRejectedSubmission(t *testing.T) {
	for _, status := range []int{200, 401, 429, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(status)
				fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
			}))
			defer upstream.Close()
			p := NewOpenAIImage(upstream.URL, upstream.Client(), nil, 0)
			_, err := p.GenerateCodex(context.Background(), accounts.Account{Token: "test", Fields: map[string]any{"source_type": "codex"}}, "test", "codex-gpt-image-2", "", "", nil)
			var flow *ImageFlowError
			if !errors.As(err, &flow) || flow.Submitted != (status == 200 || status == 502) {
				t.Fatalf("submission uncertainty: %v", err)
			}
		})
	}
}
