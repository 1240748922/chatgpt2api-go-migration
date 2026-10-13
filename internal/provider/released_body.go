package provider

import (
	"io"
	"sync"
)

// Keep the isolated session pinned until the caller has finished reading.
type releasedBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *releasedBody) Close() error { err := b.ReadCloser.Close(); b.once.Do(b.release); return err }
