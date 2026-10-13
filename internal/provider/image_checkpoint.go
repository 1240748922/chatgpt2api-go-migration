package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"time"
)

type ImageCheckpoint struct {
	Stage          string
	Account        accounts.Account
	ConversationID string
	References     []string
	Results        []ImageResult
}
type imageCheckpointKey struct{}

func WithImageCheckpoint(ctx context.Context, fn func(ImageCheckpoint) error) context.Context {
	return context.WithValue(ctx, imageCheckpointKey{}, fn)
}
func imageCheckpoint(ctx context.Context, event ImageCheckpoint) error {
	if fn, ok := ctx.Value(imageCheckpointKey{}).(func(ImageCheckpoint) error); ok {
		return fn(event)
	}
	return nil
}
func NotifyImageCheckpoint(ctx context.Context, event ImageCheckpoint) error {
	return imageCheckpoint(ctx, event)
}

// ResumeConversation only fetches an existing result; it never creates a conversation.
func (o *OpenAIImage) ResumeConversation(ctx context.Context, a accounts.Account, id string, inputs []OpenAIImageInput) (results []ImageResult, err error) {
	if id == "" {
		return nil, errors.New("conversation id is required")
	}
	if o.Proxy != nil {
		lease, e := o.Proxy.AcquireImageContext(ctx, a.Fields)
		if e != nil {
			return nil, e
		}
		if lease.Source == "unavailable" {
			return nil, errors.New("image recovery egress unavailable")
		}
		ctx = proxyruntime.WithURL(ctx, lease.URL)
		ctx = proxyruntime.WithImageLease(ctx, lease)
		defer func() { lease.Release(openAIImageProxyFailure(err)) }()
	}
	if browserEligible(o.BaseURL) {
		bound, release, e := o.browserPool.bind(ctx, warmKey(a, proxyruntime.URLFromContext(ctx)), proxyruntime.URLFromContext(ctx), 10*time.Minute)
		if e != nil {
			return nil, e
		}
		ctx = bound
		defer release()
	}
	hashes := map[[sha256.Size]byte]bool{}
	for _, input := range inputs {
		hashes[sha256.Sum256(input.Data)] = true
	}
	refs, err := o.pollConversation(ctx, a, id)
	if err != nil {
		return nil, &ImageFlowError{Stage: "poll", ConversationID: id, Submitted: true, Cause: err}
	}
	if err = imageCheckpoint(ctx, ImageCheckpoint{Stage: "result_ready", Account: a, ConversationID: id, References: refs}); err != nil {
		return nil, err
	}
	images := []ImageResult{}
	for i := len(refs) - 1; i >= 0; i-- {
		raw, mime, e := o.downloadImageRefWithRetry(ctx, a, id, refs[i])
		if e != nil {
			err = e
			continue
		}
		if hashes[sha256.Sum256(raw)] {
			continue
		}
		images = append(images, imageResultBytes(raw, mime))
		break
	}
	if len(images) == 0 {
		if err == nil {
			err = errors.New("existing conversation has no image result")
		}
		return nil, &ImageFlowError{Stage: "download", ConversationID: id, Submitted: true, Cause: err}
	}
	return images, nil
}
func imageResultBytes(raw []byte, mime string) ImageResult {
	return ImageResult{Base64: base64.StdEncoding.EncodeToString(raw), MIME: mime}
}
func (o *OpenAIImage) Close() { o.browserPool.closeIdle() }
