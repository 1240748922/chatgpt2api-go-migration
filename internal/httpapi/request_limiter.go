package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errLocalCapacity = errors.New("local request concurrency capacity is busy")

// Resizing never replaces the semaphore while an existing request holds a slot.
type requestLimiter struct {
	mu            sync.Mutex
	active, limit int
	changed       chan struct{}
}

func makeImageSlots(limit int) *requestLimiter {
	return &requestLimiter{limit: limit, changed: make(chan struct{})}
}
func (l *requestLimiter) resize(limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit != limit {
		l.limit = limit
		close(l.changed)
		l.changed = make(chan struct{})
	}
}
func (l *requestLimiter) acquire(ctx context.Context) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.limit <= 0 || l.active < l.limit {
			l.active++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					defer l.mu.Unlock()
					l.active--
					close(l.changed)
					l.changed = make(chan struct{})
				})
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (s *Server) acquireCapacity(ctx context.Context, r *http.Request, image bool) (func(), error) {
	limiter := s.textSlots
	fallback := 120
	key := "text_concurrency_limit"
	if image {
		limiter = s.imageSlots
		fallback = s.cfg.ImageMaxConcurrency
		key = "image_concurrency_limit"
	}
	if limiter == nil {
		return func() {}, nil
	}
	queue := time.Duration(0)
	if s.store != nil {
		cfg, err := s.store.Config()
		if err != nil {
			return nil, err
		}
		settings := mapValue(cfg["runtime_capacity"])
		limit := fallback
		if settings[key] != nil {
			limit = max(1, min(10000, intValue(settings[key])))
		}
		limiter.resize(limit)
		queue = imageSeconds(settings, "request_queue_timeout_seconds", 2, 0.1, 120)
	}
	waitCtx := ctx
	cancel := func() {}
	if queue > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, queue)
	}
	defer cancel()
	started := time.Now()
	release, err := limiter.acquire(waitCtx)
	if err != nil && ctx.Err() == nil {
		return nil, errLocalCapacity
	}
	if err == nil && r != nil {
		s.stageRequestMonitor(r, "handler_queue_done", 10, map[string]any{"handler_queue_ms": time.Since(started).Milliseconds()})
	}
	return release, err
}
func (s *Server) acquireImageSlot(ctx context.Context, r *http.Request) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	return s.acquireCapacity(ctx, r, true)
}
