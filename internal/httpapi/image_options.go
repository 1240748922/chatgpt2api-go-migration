package httpapi

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/mediafile"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// OpenAI images preserve the Python API default and explicit response_format.
func (s *Server) openAIImageResponseFormat(requested string) (string, error) {
	format := strings.TrimSpace(requested)
	if format == "" {
		cfg, err := s.store.Config()
		if err != nil {
			return "", err
		}
		switch stringValue(mapValue(cfg["image_generation"])["output_format"]) {
		case "url":
			format = "url"
		default:
			format = "b64_json"
		}
	}
	if format != "url" && format != "b64_json" {
		return "", &imageSettingsError{400, "invalid_response_format", "response_format must be url or b64_json."}
	}
	return format, nil
}

type imagePolicyKey struct{}
type imageSettingsError struct {
	Status        int
	Code, Message string
}

func (e *imageSettingsError) Error() string { return e.Message }

type imageRequestPolicy struct {
	AccountLimit int
	Parallel     bool
	Budget       time.Duration
}

func (s *Server) imagePolicy(ctx context.Context) imageRequestPolicy {
	if policy, ok := ctx.Value(imagePolicyKey{}).(imageRequestPolicy); ok {
		return policy
	}
	return imageRequestPolicy{AccountLimit: max(1, s.cfg.ImageAccountLimit), Parallel: true, Budget: imageRequestTotalTimeout(s.cfg.RequestTimeout)}
}
func (s *Server) imageTotalBudget(ctx context.Context, count int) time.Duration {
	p := s.imagePolicy(ctx)
	if !p.Parallel {
		return p.Budget * time.Duration(max(1, count))
	}
	return p.Budget
}
func (s *Server) imageMinimumValidity(ctx context.Context) time.Duration {
	return max(5*time.Minute, s.imagePolicy(ctx).Budget+30*time.Second)
}

// Admission, maintenance and displayed ready quota must use the same AT window.
func (s *Server) configuredImageMinimumValidity() time.Duration {
	ctx, err := s.imageOptionsContext(context.Background())
	if err != nil {
		return s.imageMinimumValidity(context.Background())
	}
	return s.imageMinimumValidity(ctx)
}

func imageSeconds(cfg map[string]any, key string, fallback float64, low, high float64) time.Duration {
	n := fallback
	switch v := cfg[key].(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case json.Number:
		if x, e := v.Float64(); e == nil {
			n = x
		}
	case string:
		if x, e := strconv.ParseFloat(v, 64); e == nil {
			n = x
		}
	}
	n = max(low, min(n, high))
	return time.Duration(n * float64(time.Second))
}
func (s *Server) imageOptionsContext(ctx context.Context) (context.Context, error) {
	if ctx.Value(imagePolicyKey{}) != nil {
		return ctx, nil
	}
	cfg, err := s.store.Config()
	if err != nil {
		return ctx, err
	}
	if !boolValue(mapValue(cfg["image_generation"])["enabled"], true) {
		return ctx, &imageSettingsError{503, "image_generation_disabled", "Image generation is disabled."}
	}
	if boolValue(cfg["image_upscale_enabled"], false) {
		if _, err := mediafile.NormalizeUpscaleEngine(stringValue(cfg["image_upscale_engine"])); err != nil {
			return ctx, &imageSettingsError{400, "invalid_image_upscale_engine", err.Error()}
		}
	}
	settle := time.Duration(0)
	if boolValue(cfg["image_settle_enabled"], true) {
		settle = imageSeconds(cfg, "image_settle_secs", 2, 0, 15)
	}
	options := provider.ImageOptions{
		StreamTimeout: imageSeconds(cfg, "image_stream_timeout_secs", 180, 10, 600),
		PollTimeout:   imageSeconds(cfg, "image_poll_timeout_secs", 120, 10, 600),
		PollInterval:  imageSeconds(cfg, "image_poll_interval_secs", 1, 0.5, 30),
		InitialWait:   imageSeconds(cfg, "image_poll_initial_wait_secs", 0, 0, 30), Settle: settle,
		CheckBeforeHit:     boolValue(cfg["image_check_before_hit_enabled"], true),
		RemoveConversation: boolValue(cfg["image_remove_conversation_after_result"], false),
	}
	// A bounded GET-only grace period recovers late results without re-submitting.
	options.PollTimeout += imageSeconds(cfg, "image_timeout_retry_secs", 30, 0, 120)
	// Include preparation and delivery time. A configured stream/poll budget
	// must not be silently truncated by an unrelated HTTP request timeout.
	policy := imageRequestPolicy{AccountLimit: int(imageSeconds(cfg, "image_account_concurrency", float64(max(1, s.cfg.ImageAccountLimit)), 1, 32) / time.Second), Parallel: boolValue(cfg["image_parallel_generation"], true), Budget: options.StreamTimeout + options.PollTimeout + options.InitialWait + options.Settle + 5*time.Minute}
	return provider.WithImageOptions(context.WithValue(ctx, imagePolicyKey{}, policy), options), nil
}
