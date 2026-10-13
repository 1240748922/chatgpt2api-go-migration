package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func TestEditablePollDefinitiveFailureDoesNotWaitTwentyMinutes(t *testing.T) {
	for _, status := range []int{401, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				w.Write([]byte("invalid JSON"))
			}))
			defer up.Close()
			p := NewOpenAIImage(up.URL, up.Client(), nil, time.Second)
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := p.pollEditableArtifacts(ctx, accounts.Account{Token: "test"}, "saved-conversation", "ppt")
			if err == nil || ctx.Err() != nil || calls.Load() != 1 {
				t.Fatalf("poll failure err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
