package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"net/http"
	"sync"
	"time"
)

type completionEntry struct {
	body    []byte
	header  http.Header
	status  int
	expires time.Time
}
type completionFlight struct {
	done  chan struct{}
	entry *completionEntry
}
type completionCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*completionEntry
	flights map[[32]byte]*completionFlight
}
type completionCapture struct {
	http.ResponseWriter
	body     bytes.Buffer
	status   int
	overflow bool
}

func (w *completionCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *completionCapture) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.body.Len()+len(raw) <= 2<<20 && !w.overflow {
		w.body.Write(raw)
	} else {
		w.overflow = true
		w.body.Reset()
	}
	return w.ResponseWriter.Write(raw)
}
func (w *completionCapture) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func normalizeCompletionMessages(req protocol.ChatRequest, cfg map[string]any) protocol.ChatRequest {
	if !boolValue(cfg["normalize_messages"], true) {
		return req
	}
	messages := make([]protocol.Message, 0, len(req.Messages))
	previous := ""
	for _, m := range req.Messages {
		if boolValue(cfg["drop_assistant_history"], false) && m.Role == "assistant" {
			continue
		}
		raw, _ := json.Marshal(m)
		signature := string(raw)
		if boolValue(cfg["drop_adjacent_duplicates"], true) && signature == previous {
			continue
		}
		messages = append(messages, m)
		previous = signature
	}
	req.Messages = messages
	return req
}
func completionCacheable(body []byte, stream bool) bool {
	if !stream {
		var value map[string]any
		return json.Unmarshal(body, &value) == nil && value["error"] == nil
	}
	done := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		raw := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data: ")))
		if string(raw) == "[DONE]" {
			done = true
			continue
		}
		var value map[string]any
		if json.Unmarshal(raw, &value) != nil || value["error"] != nil {
			return false
		}
	}
	return done
}
func replayCompletion(w http.ResponseWriter, e *completionEntry) {
	for k, v := range e.header {
		w.Header()[k] = append([]string(nil), v...)
	}
	w.Header().Set("X-Completion-Cache", "hit")
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
func (c *completionCache) prune(now time.Time, maxEntries int) {
	total := 0
	for key, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, key)
		} else {
			total += len(e.body)
		}
	}
	for len(c.entries) > maxEntries || total > 16<<20 {
		var oldest [32]byte
		var expiry time.Time
		for k, e := range c.entries {
			if expiry.IsZero() || e.expires.Before(expiry) {
				oldest = k
				expiry = e.expires
			}
		}
		total -= len(c.entries[oldest].body)
		delete(c.entries, oldest)
	}
}
func (s *Server) serveCompletionCache(w http.ResponseWriter, r *http.Request, req protocol.ChatRequest, compute func(http.ResponseWriter, *http.Request, protocol.ChatRequest)) bool {
	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, 503, "设置读取失败", "storage_error")
		return true
	}
	settings := mapValue(cfg["chat_completion_cache"])
	req = normalizeCompletionMessages(req, settings)
	ttl := imageSeconds(settings, "ttl_seconds", 60, 0, 86400)
	if !boolValue(settings["enabled"], true) || ttl <= 0 || (req.Stream && !boolValue(settings["stream_cache"], true)) {
		compute(w, r, req)
		return true
	}
	raw, _ := json.Marshal(map[string]any{"request": req, "owner": s.authIdentity(r), "path": r.URL.Path, "settings": settings})
	key := sha256.Sum256(raw)
	c := &s.completionCache
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[[32]byte]*completionEntry)
		c.flights = make(map[[32]byte]*completionFlight)
	}
	capacity := 256
	if settings["max_entries"] != nil {
		capacity = max(1, min(4096, intValue(settings["max_entries"])))
	}
	c.prune(time.Now(), capacity)
	if e := c.entries[key]; e != nil {
		c.mu.Unlock()
		replayCompletion(w, e)
		return true
	}
	dedupe := boolValue(settings["dedupe_inflight"], true)
	if flight := c.flights[key]; flight != nil && dedupe {
		c.mu.Unlock()
		select {
		case <-r.Context().Done():
			return true
		case <-flight.done:
		}
		if flight.entry != nil {
			replayCompletion(w, flight.entry)
		} else {
			writeError(w, 502, "Shared upstream request failed. Please retry.", "upstream_error")
		}
		return true
	}
	flight := &completionFlight{done: make(chan struct{})}
	tracked := dedupe && len(c.flights) < 128
	if tracked {
		c.flights[key] = flight
	}
	c.mu.Unlock()
	capture := &completionCapture{ResponseWriter: w}
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if tracked {
			delete(c.flights, key)
		}
		close(flight.done)
	}()
	compute(capture, r, req)
	if capture.status == 200 && !capture.overflow && r.Context().Err() == nil && completionCacheable(capture.body.Bytes(), req.Stream) {
		header := make(http.Header)
		for _, k := range []string{"Content-Type", "Cache-Control", "X-Accel-Buffering"} {
			if v := w.Header().Values(k); len(v) > 0 {
				header[k] = append([]string(nil), v...)
			}
		}
		entry := &completionEntry{body: append([]byte(nil), capture.body.Bytes()...), header: header, status: 200, expires: time.Now().Add(ttl)}
		c.mu.Lock()
		flight.entry = entry
		c.entries[key] = entry
		c.prune(time.Now(), capacity)
		c.mu.Unlock()
	}
	return true
}
