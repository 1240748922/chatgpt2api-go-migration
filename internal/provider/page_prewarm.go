package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type warmPage struct {
	scripts []string
	build   string
	expires time.Time
}

func warmKey(account accounts.Account, egress string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(account.Token+"\x00"+fmt.Sprint(account.Fields["credential_generation"])+"\x00"+egress)))
}

func (o *OpenAIImage) pruneWarmLocked() {
	if o.warmPages == nil {
		o.warmPages = map[string]warmPage{}
		o.warmFlights = map[string]bool{}
	}
	for k, page := range o.warmPages {
		if !page.expires.After(time.Now()) {
			delete(o.warmPages, k)
		}
	}
}

// Prewarm fetches only the entry page. Requirements, conversation preparation,
// uploads and generation are always performed for the actual request.
func (o *OpenAIImage) Prewarm(ctx context.Context, account accounts.Account) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if o.Proxy != nil {
		lease, err := o.Proxy.AcquireImageContext(ctx, account.Fields)
		if err != nil {
			return err
		}
		defer lease.Release(false)
		if lease.Source == "unavailable" {
			return fmt.Errorf("prewarm egress unavailable")
		}
		ctx = proxyruntime.WithURL(ctx, lease.URL)
	}
	key := warmKey(account, proxyruntime.URLFromContext(ctx))
	if browserEligible(o.BaseURL) {
		bound, release, err := o.browserPool.bind(ctx, key, proxyruntime.URLFromContext(ctx), 10*time.Minute)
		if err != nil {
			return err
		}
		defer release()
		ctx = bound
	}
	o.warmMu.Lock()
	o.pruneWarmLocked()
	if _, exists := o.warmPages[key]; exists || o.warmFlights[key] || len(o.warmPages)+len(o.warmFlights) >= 16 {
		o.warmMu.Unlock()
		return nil
	}
	o.warmFlights[key] = true
	o.warmMu.Unlock()
	scripts, build, err := o.bootstrap(ctx, account)
	o.warmMu.Lock()
	defer o.warmMu.Unlock()
	delete(o.warmFlights, key)
	if err == nil && ctx.Err() == nil {
		o.warmPages[key] = warmPage{scripts: scripts, build: build, expires: time.Now().Add(45 * time.Second)}
	}
	return err
}

func (o *OpenAIImage) claimPage(ctx context.Context, account accounts.Account) ([]string, string, error) {
	key := warmKey(account, proxyruntime.URLFromContext(ctx))
	o.warmMu.Lock()
	o.pruneWarmLocked()
	if page, ok := o.warmPages[key]; ok {
		delete(o.warmPages, key)
		o.warmHits++
		o.warmMu.Unlock()
		return page.scripts, page.build, nil
	}
	o.warmMisses++
	o.warmMu.Unlock()
	return o.bootstrap(ctx, account)
}

func (o *OpenAIImage) PrewarmStats() map[string]any {
	o.warmMu.Lock()
	defer o.warmMu.Unlock()
	o.pruneWarmLocked()
	return map[string]any{"ready": len(o.warmPages), "capacity": 16, "inflight": len(o.warmFlights), "hits": o.warmHits, "misses": o.warmMisses}
}

func (o *OpenAIImage) prepareImageInputs(ctx context.Context, account accounts.Account, inputs []OpenAIImageInput) ([]openAIImageReference, []string, string, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var refs []openAIImageReference
	var scripts []string
	var build string
	var failure string
	var firstErr error
	var once sync.Once
	report := func(stage string, err error) {
		if err != nil {
			once.Do(func() { failure, firstErr = stage, err; cancel() })
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		started := time.Now()
		var err error
		refs, err = o.uploadInputs(ctx, account, inputs)
		report("upload", err)
		if len(inputs) > 0 {
			notifyOpenAIImageStage(ctx, "upload_ms", started)
			proxyruntime.ObserveImageLeaseStage(ctx, time.Since(started), openAIImageSlowUpload)
		}
	}()
	go func() {
		defer wg.Done()
		started := time.Now()
		var err error
		scripts, build, err = o.claimPage(ctx, account)
		report("bootstrap", err)
		notifyOpenAIImageStage(ctx, "bootstrap_ms", started)
		proxyruntime.ObserveImageLeaseStage(ctx, time.Since(started), openAIImageSlowBootstrap)
	}()
	wg.Wait()
	return refs, scripts, build, failure, firstErr
}
