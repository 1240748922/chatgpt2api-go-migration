package remotestore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type R2 struct {
	Endpoint, AccessKey, Secret, Bucket, Prefix string
	Client                                      *http.Client
}
type Object struct {
	Key      string `xml:"Key" json:"key"`
	Size     int64  `xml:"Size" json:"size"`
	Modified string `xml:"LastModified" json:"last_modified"`
}

func awsHMAC(key []byte, text string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(text))
	return h.Sum(nil)
}
func (d *R2) signed(ctx context.Context, method, key string, query url.Values, body io.ReadSeeker) (*http.Request, error) {
	u, err := url.Parse(d.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || d.AccessKey == "" || d.Secret == "" || !safePath(d.Bucket) || strings.Contains(d.Bucket, "/") {
		return nil, fmt.Errorf("R2 configuration incomplete")
	}
	if key != "" && !safePath(key) {
		return nil, fmt.Errorf("invalid R2 object key")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + d.Bucket + "/" + key
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	h := sha256.New()
	length := int64(0)
	if body != nil {
		if _, err = body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		length, err = io.Copy(h, body)
		if err != nil {
			return nil, err
		}
		if _, err = body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	digest := hex.EncodeToString(h.Sum(nil))
	now := time.Now().UTC()
	stamp, date := now.Format("20060102T150405Z"), now.Format("20060102")
	scope := date + "/auto/s3/aws4_request"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonical := method + "\n" + u.EscapedPath() + "\n" + u.RawQuery + "\n" + "host:" + u.Host + "\nx-amz-content-sha256:" + digest + "\nx-amz-date:" + stamp + "\n\n" + signedHeaders + "\n" + digest
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := awsHMAC([]byte("AWS4"+d.Secret), date)
	k = awsHMAC(k, "auto")
	k = awsHMAC(k, "s3")
	k = awsHMAC(k, "aws4_request")
	var reader io.Reader
	if body != nil {
		reader = body
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.ContentLength = length
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", digest)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+d.AccessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+hex.EncodeToString(awsHMAC(k, toSign)))
	return req, nil
}
func (d *R2) request(ctx context.Context, method, key string, q url.Values, body io.ReadSeeker) (*http.Response, error) {
	req, err := d.signed(ctx, method, key, q, body)
	if err != nil {
		return nil, err
	}
	client := d.Client
	if client == nil {
		client = HTTPClient(10 * time.Minute)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("R2 %s request failed", method)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("R2 %s returned HTTP %d", method, resp.StatusCode)
	}
	return resp, nil
}
func (d *R2) prefix() string { return strings.Trim(d.Prefix, "/") }
func (d *R2) Key(name string) string {
	if d.prefix() == "" {
		return name
	}
	return d.prefix() + "/" + name
}
func (d *R2) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	resp, err := d.request(ctx, "PUT", key, nil, body)
	if err == nil {
		resp.Body.Close()
	}
	return err
}
func (d *R2) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := d.request(ctx, "GET", key, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
func (d *R2) Delete(ctx context.Context, key string) error {
	resp, err := d.request(ctx, "DELETE", key, nil, nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}
func (d *R2) List(ctx context.Context) ([]Object, error) {
	out := []Object{}
	cursor := ""
	for page := 0; page < 100; page++ {
		q := url.Values{"list-type": {"2"}, "prefix": {d.Key("")}, "max-keys": {"1000"}}
		if cursor != "" {
			q.Set("continuation-token", cursor)
		}
		resp, err := d.request(ctx, "GET", "", q, nil)
		if err != nil {
			return nil, err
		}
		var data struct {
			Contents  []Object `xml:"Contents"`
			Truncated bool     `xml:"IsTruncated"`
			Next      string   `xml:"NextContinuationToken"`
		}
		raw, e := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		resp.Body.Close()
		if e != nil || len(raw) > 8<<20 || xml.Unmarshal(raw, &data) != nil {
			return nil, fmt.Errorf("invalid R2 object listing")
		}
		out = append(out, data.Contents...)
		if !data.Truncated {
			return out, nil
		}
		if data.Next == "" || data.Next == cursor {
			return nil, fmt.Errorf("invalid R2 pagination")
		}
		cursor = data.Next
	}
	return nil, fmt.Errorf("R2 listing exceeds 100000 objects")
}
