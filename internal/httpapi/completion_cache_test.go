package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func cacheRequest(owner string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	return r.WithContext(context.WithValue(r.Context(), imageTaskOwnerKey{}, owner))
}

func TestCompletionCacheLiveStreamAndOwnerIsolation(t *testing.T) {
	s := importTestServer(t)
	req := protocol.ChatRequest{Model: "gpt-5", Stream: true}
	first, resume := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	compute := func(w http.ResponseWriter, r *http.Request, req protocol.ChatRequest) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Private-Monitor", "private")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		if n == 1 {
			close(first)
			<-resume
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	owner := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.serveCompletionCache(owner, cacheRequest("one"), req, compute) }()
	<-first
	// The original stream is forwarded before completion, not buffered until DONE.
	if !owner.Flushed || owner.Body.Len() == 0 {
		t.Fatal("first SSE event was not delivered live")
	}
	cancelCtx, cancel := context.WithCancel(cacheRequest("one").Context())
	cancel()
	s.serveCompletionCache(httptest.NewRecorder(), cacheRequest("one").WithContext(cancelCtx), req, compute)
	if calls.Load() != 1 {
		t.Fatal("cancelled follower submitted a duplicate upstream request")
	}
	close(resume)
	<-done
	follower := httptest.NewRecorder()
	s.serveCompletionCache(follower, cacheRequest("one"), req, compute)
	if calls.Load() != 1 || follower.Body.String() != owner.Body.String() || follower.Header().Get("X-Completion-Cache") != "hit" || follower.Header().Get("X-Private-Monitor") != "" {
		t.Fatal("completed stream was not safely reused")
	}
	s.serveCompletionCache(httptest.NewRecorder(), cacheRequest("two"), req, compute)
	if calls.Load() != 2 {
		t.Fatal("completion crossed the authentication boundary")
	}
}

func TestCompletionCacheSingleFlightAndFailures(t *testing.T) {
	s := importTestServer(t)
	req := protocol.ChatRequest{Model: "gpt-5"}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	compute := func(w http.ResponseWriter, r *http.Request, req protocol.ChatRequest) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}
	a, b := httptest.NewRecorder(), httptest.NewRecorder()
	ownerDone, followerDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(ownerDone); s.serveCompletionCache(a, cacheRequest("one"), req, compute) }()
	<-entered
	go func() { defer close(followerDone); s.serveCompletionCache(b, cacheRequest("one"), req, compute) }()
	select {
	case <-followerDone:
		t.Fatal("follower returned before owner completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-ownerDone
	<-followerDone
	if calls.Load() != 1 || a.Body.String() != b.Body.String() {
		t.Fatal("single flight failed")
	}
	for _, body := range []string{`{"error":{"message":"failed"}}`, `not JSON`} {
		failed := func(w http.ResponseWriter, r *http.Request, req protocol.ChatRequest) {
			calls.Add(1)
			_, _ = w.Write([]byte(body))
		}
		before := calls.Load()
		for i := 0; i < 2; i++ {
			s.serveCompletionCache(httptest.NewRecorder(), cacheRequest(body), req, failed)
		}
		if calls.Load()-before != 2 {
			t.Fatal("invalid/error response was cached")
		}
	}
	if completionCacheable([]byte("data: {\"error\":{}}\n\ndata: [DONE]\n\n"), true) || completionCacheable([]byte("data: {}\n\n"), true) {
		t.Fatal("partial/error SSE accepted")
	}
}
