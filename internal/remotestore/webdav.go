package remotestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type WebDAV struct {
	URL, Username, Password, Root string
	Client                        *http.Client
}

func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func safePath(path string) bool {
	if strings.ContainsAny(path, "\\\x00") || strings.HasPrefix(path, "/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return path != ""
}
func (d *WebDAV) endpoint(rel string) (string, error) {
	u, err := url.Parse(d.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid WebDAV URL")
	}
	joined := strings.Trim(d.Root, "/")
	if rel != "" {
		if !safePath(rel) {
			return "", fmt.Errorf("invalid remote path")
		}
		if joined != "" {
			joined += "/"
		}
		joined += rel
	}
	if joined != "" && !safePath(joined) {
		return "", fmt.Errorf("invalid WebDAV root")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + joined
	u.RawPath = ""
	return u.String(), nil
}
func (d *WebDAV) request(ctx context.Context, method, rel string, body io.Reader) (*http.Response, error) {
	endpoint, err := d.endpoint(rel)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if d.Username != "" || d.Password != "" {
		req.SetBasicAuth(d.Username, d.Password)
	}
	if method == "PUT" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	client := d.Client
	if client == nil {
		client = HTTPClient(30 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("WebDAV %s request failed", method)
	}
	allowed := resp.StatusCode >= 200 && resp.StatusCode < 300 || method == "MKCOL" && resp.StatusCode == 405 || method == "DELETE" && resp.StatusCode == 404
	if !allowed {
		resp.Body.Close()
		return nil, fmt.Errorf("WebDAV %s returned HTTP %d", method, resp.StatusCode)
	}
	return resp, nil
}
func (d *WebDAV) Put(ctx context.Context, rel string, raw []byte) error {
	parts := strings.Split(rel, "/")
	if !safePath(rel) {
		return fmt.Errorf("invalid remote path")
	}
	// Create each root component as well as the image's parent directories.
	root := strings.Trim(d.Root, "/")
	all := append(strings.Split(root, "/"), parts[:len(parts)-1]...)
	current := ""
	base := *d
	base.Root = ""
	for _, part := range all {
		if part == "" {
			continue
		}
		if current != "" {
			current += "/"
		}
		current += part
		resp, err := base.request(ctx, "MKCOL", current, nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
	}
	resp, err := d.request(ctx, "PUT", rel, bytes.NewReader(raw))
	if err == nil {
		resp.Body.Close()
	}
	return err
}
func (d *WebDAV) Get(ctx context.Context, rel string) ([]byte, error) {
	resp, err := d.request(ctx, "GET", rel, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if len(raw) > 64<<20 {
		return nil, fmt.Errorf("remote image exceeds 64 MiB")
	}
	return raw, err
}
func (d *WebDAV) Delete(ctx context.Context, rel string) error {
	resp, err := d.request(ctx, "DELETE", rel, nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}
func (d *WebDAV) Test(ctx context.Context, id string) error {
	rel := ".go-image-test-" + id + ".txt"
	if err := d.Put(ctx, rel, []byte("storage test\n")); err != nil {
		return err
	}
	return d.Delete(ctx, rel)
}
