package provider

import (
	"context"
	"testing"
	"time"
)

func TestBrowserSessionsIsolateIdentityAndPin(t *testing.T) {
	var pool isolatedBrowserPool
	a, release, err := pool.acquire("account-a", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	again, finish, _ := pool.acquire("account-a", "", time.Second)
	finish()
	finish()
	other, done, _ := pool.acquire("account-b", "", time.Second)
	done()
	if again != a || other == a {
		t.Fatal("cookie session identity mixed")
	}
	for i := 0; i < browserSessionCapacity+10; i++ {
		_, r, e := pool.acquire(string(rune(i+1000)), "", time.Second)
		if e != nil {
			t.Fatal(e)
		}
		r()
	}
	if pool.sessions["account-a"] == nil || len(pool.sessions) > browserSessionCapacity {
		t.Fatal("pinned session evicted or capacity exceeded")
	}
	ctx, unbind, e := pool.bind(context.Background(), "account-a", "", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer unbind()
	bound, r, e := pool.acquireForContext(ctx, "account-a", "", time.Second)
	if e != nil || bound != a {
		t.Fatal("bound context not reused")
	}
	r()
	if browserIdentity("at-a", "proxy-a") == browserIdentity("at-b", "proxy-a") || browserIdentity("at-a", "proxy-a") == browserIdentity("at-a", "proxy-b") {
		t.Fatal("identity collision")
	}
}
