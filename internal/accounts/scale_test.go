package accounts

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
	"github.com/auucoder/gptgrok2api-go/internal/testpg"
)

// An explicit local-only scale check: no provider traffic or real credentials.
func TestPostgresTwentyThousandAccountsEightReplicas(t *testing.T) {
	if os.Getenv("MIGRATION_SCALE_TEST") != "1" {
		t.Skip("set MIGRATION_SCALE_TEST=1 for scale validation")
	}
	dsn := testpg.DSN(t)
	stores := make([]*store.Store, 8)
	pools := make([]*Pool, 8)
	for i := range stores {
		root := t.TempDir()
		s, err := store.Open(filepath.Join(root, "a.json"), filepath.Join(root, "k.json"), filepath.Join(root, "c.json"), dsn)
		if err != nil {
			t.Fatal(err)
		}
		stores[i], pools[i] = s, New(s)
		t.Cleanup(func() { _ = s.Close() })
	}
	start := time.Now()
	for batch := 0; batch < 100; batch++ {
		rows := make([]map[string]any, 200)
		for i := range rows {
			id := fmt.Sprintf("synthetic-%05d", batch*200+i)
			claims := fmt.Sprintf(`{"sub":%q,"exp":%d}`, id, time.Now().Add(time.Hour).Unix())
			rows[i] = map[string]any{"id": id, "access_token": "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".test", "quota": 5, "email": id + "@example.test"}
		}
		added, skipped, _, err := stores[batch%8].AddAccounts(nil, rows)
		if err != nil || added != 200 || skipped != 0 {
			t.Fatalf("batch %d: added=%d skipped=%d err=%v", batch, added, skipped, err)
		}
	}
	importTime := time.Since(start)
	for _, s := range stores {
		items, _, err := s.AccountSnapshot()
		if err != nil || len(items) != 20000 {
			t.Fatalf("replica count=%d err=%v", len(items), err)
		}
	}
	var mu sync.Mutex
	timings := []time.Duration{}
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		held := map[string]bool{}
		leases := make([]*Lease, 64)
		for i := range leases {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				started := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				// Keep the reservation context alive until all contenders finish.
				lease, err := pools[i%8].ReserveMatchingLimit(ctx, []string{"basic"}, nil, func(a Account) bool { return ImageEligible(a.Fields, false, time.Now(), 11*time.Minute) }, 1)
				if err != nil {
					cancel()
					t.Error(err)
					return
				}
				mu.Lock()
				if held[lease.Account.Token] {
					t.Error("cross-instance duplicate account dispatch")
				}
				held[lease.Account.Token] = true
				leases[i] = lease
				timings = append(timings, time.Since(started))
				mu.Unlock()
				if _, err := stores[i%8].UpdateAccountRuntime(lease.Account.Token, map[string]any{"last_scale_round": round}); err != nil {
					t.Error(err)
				}
				// Release cancels the derived context; the parent deadline is bounded.
				t.Cleanup(cancel)
			}(i)
		}
		wg.Wait()
		for i, lease := range leases {
			pools[i%8].Release(lease)
		}
	}
	if len(timings) != 320 {
		t.Fatalf("successful reservations=%d", len(timings))
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	t.Logf("synthetic accounts=20000 replicas=8 concurrent reservations=64 requests=320 import=%s selection_p50=%s selection_p95=%s max=%s total=%s", importTime, timings[len(timings)/2], timings[len(timings)*95/100], timings[len(timings)-1], time.Since(start))
}
