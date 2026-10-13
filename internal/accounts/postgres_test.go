package accounts

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

func TestPostgresReplicasShareImageCapacity(t *testing.T) {
	dsn := testpg.DSN(t)
	open := func() *store.Store {
		root := t.TempDir()
		s, e := store.Open(filepath.Join(root, "a.json"), filepath.Join(root, "k.json"), filepath.Join(root, "c.json"), dsn)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, b := open(), open()
	if _, _, _, err := a.AddAccounts([]string{"test-at"}, nil); err != nil {
		t.Fatal(err)
	}
	p, q := New(a), New(b)
	first, err := p.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(first)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if second, err := q.ReserveMatchingLimit(ctx, []string{"basic"}, nil, nil, 1); second != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("double dispatch: %v %v", second, err)
	}
	p.Release(first)
	second, err := q.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Release(second)
}
