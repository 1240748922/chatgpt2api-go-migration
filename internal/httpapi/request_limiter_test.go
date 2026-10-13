package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRequestLimiterResizePreservesHeldSlots(t *testing.T) {
	l := makeImageSlots(2)
	a, _ := l.acquire(context.Background())
	b, _ := l.acquire(context.Background())
	l.resize(1)
	a()
	a()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shrinking lost existing occupancy", err)
	}
	l.resize(2)
	c, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b()
	c()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active != 0 {
		t.Fatal("release not idempotent", l.active)
	}
}

func TestQueueTimeoutDoesNotShortenActiveGeneration(t *testing.T) {
	s := importTestServer(t)
	s.imageSlots = makeImageSlots(1)
	s.cfg.ImageMaxConcurrency = 1
	_, err := s.store.PatchConfig(map[string]any{"runtime_capacity": map[string]any{"image_concurrency_limit": 1, "request_queue_timeout_seconds": 0.1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active, err := s.acquireCapacity(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer active()
	if _, err := s.acquireCapacity(ctx, nil, true); !errors.Is(err, errLocalCapacity) {
		t.Fatal("queue did not time out", err)
	}
	if ctx.Err() != nil {
		t.Fatal("queue timer cancelled an active generation")
	}
}
