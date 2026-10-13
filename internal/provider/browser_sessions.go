package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

const browserSessionCapacity = 256

type browserSession struct {
	client *browserHTTP
	users  int
	used   time.Time
}
type isolatedBrowserPool struct {
	mu       sync.Mutex
	sessions map[string]*browserSession
}
type browserSessionContextKey struct{}

func (p *isolatedBrowserPool) closeIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, session := range p.sessions {
		if session.users == 0 {
			session.client.CloseIdleConnections()
			delete(p.sessions, key)
		}
	}
}
type boundBrowser struct {
	key    string
	client *browserHTTP
}

// Only idle entries can be evicted. A generation pins its cookie jar through
// upload, initialization, stream and polling; account identity is always hashed.
func (p *isolatedBrowserPool) acquire(key, proxyURL string, timeout time.Duration) (*browserHTTP, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[string]*browserSession{}
	}
	session := p.sessions[key]
	cached := true
	if session == nil {
		if len(p.sessions) >= browserSessionCapacity {
			oldest := ""
			for k, entry := range p.sessions {
				if entry.users == 0 && (oldest == "" || entry.used.Before(p.sessions[oldest].used)) {
					oldest = k
				}
			}
			if oldest != "" {
				p.sessions[oldest].client.CloseIdleConnections()
				delete(p.sessions, oldest)
			} else {
				cached = false
			}
		}
		client, err := newBrowserHTTP(proxyURL, timeout)
		if err != nil {
			return nil, nil, err
		}
		session = &browserSession{client: client}
		if cached {
			p.sessions[key] = session
		}
	}
	session.users++
	session.used = time.Now()
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			session.users--
			session.used = time.Now()
			if !cached {
				session.client.CloseIdleConnections()
			}
		})
	}
	return session.client, release, nil
}
func (p *isolatedBrowserPool) acquireForContext(ctx context.Context, key, proxyURL string, timeout time.Duration) (*browserHTTP, func(), error) {
	if bound, ok := ctx.Value(browserSessionContextKey{}).(boundBrowser); ok && bound.key == key {
		return bound.client, func() {}, nil
	}
	return p.acquire(key, proxyURL, timeout)
}
func (p *isolatedBrowserPool) bind(ctx context.Context, key, proxyURL string, timeout time.Duration) (context.Context, func(), error) {
	client, release, err := p.acquire(key, proxyURL, timeout)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, browserSessionContextKey{}, boundBrowser{key, client}), release, nil
}
func browserIdentity(token, proxyURL string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token+"\x00"+proxyURL)))
}
