package redisx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func testClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	zlogtest.Discard(t)
	srv := miniredis.RunT(t)
	cli, err := Open(context.Background(), Target{Addr: srv.Addr()}, Config{}, "member")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return srv, cli
}

func TestNamespace(t *testing.T) {
	_, cli := testClient(t)
	if Namespace(cli) != "member" {
		t.Fatalf("namespace %q", Namespace(cli))
	}
	if keyOf(cli, "lock", "settle:1") != "lock:member:settle:1" {
		t.Fatal(keyOf(cli, "lock", "settle:1"))
	}
	if keyOf(nil, "lock", "x") != "lock:x" {
		t.Fatal("nil client has no namespace")
	}
}

func TestWithLock(t *testing.T) {
	srv, cli := testClient(t)
	ctx := context.Background()
	// exclusive: the second caller is busy at once
	release := make(chan struct{})
	entered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = WithLock(ctx, cli, "settle:1", 2*time.Second, func(ctx context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if err := WithLock(ctx, cli, "settle:1", time.Second, func(context.Context) error { return nil }); !errors.Is(err, ErrLocked) {
		t.Fatalf("second holder: %v", err)
	}
	// another name is independent
	if err := WithLock(ctx, cli, "settle:2", time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Wait: gets it once the first releases
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	if err := WithLock(ctx, cli, "settle:1", time.Second, func(context.Context) error { return nil }, Wait(2*time.Second)); err != nil {
		t.Fatalf("waiting caller: %v", err)
	}
	wg.Wait()
	if srv.Exists("lock:member:settle:1") {
		t.Fatal("lock not released")
	}
	// fn's error comes back; the lock is released anyway
	boom := errors.New("boom")
	if err := WithLock(ctx, cli, "settle:3", time.Second, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if srv.Exists("lock:member:settle:3") {
		t.Fatal("lock not released after error")
	}
	// lost lock: the key disappears under the holder -> ctx cancelled, ErrLockLost
	var sawCancel atomic.Bool
	err := WithLock(ctx, cli, "settle:4", 3*time.Second, func(ctx context.Context) error {
		srv.Del("lock:member:settle:4")
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
		case <-time.After(5 * time.Second):
		}
		return nil
	})
	if !errors.Is(err, ErrLockLost) || !sawCancel.Load() {
		t.Fatalf("lost lock: err=%v cancelled=%v", err, sawCancel.Load())
	}
	// no redis: refused
	if err := WithLock(ctx, nil, "x", time.Second, func(context.Context) error { return nil }); err == nil {
		t.Fatal("nil client accepted")
	}
}

type member struct {
	UID  int64  `json:"uid"`
	Name string `json:"name"`
}

var errNotFound = errors.New("not found")

func TestCache(t *testing.T) {
	srv, cli := testClient(t)
	ctx := context.Background()
	c := NewCache[member](cli, "member", time.Minute).Negative(errNotFound, 10*time.Second)
	var loads atomic.Int32
	load := func(ctx context.Context) (member, error) {
		loads.Add(1)
		time.Sleep(20 * time.Millisecond)
		return member{UID: 42, Name: "alice"}, nil
	}
	// stampede: 20 concurrent misses, one load
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := c.Get(ctx, "42", load)
			if err != nil || m.Name != "alice" {
				t.Errorf("get: %v %+v", err, m)
			}
		}()
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads=%d, want 1", loads.Load())
	}
	if !srv.Exists("cache:member:member:42") {
		t.Fatal("not stored")
	}
	// hit
	if _, err := c.Get(ctx, "42", load); err != nil || loads.Load() != 1 {
		t.Fatalf("hit: err=%v loads=%d", err, loads.Load())
	}
	// Del then miss
	c.Del(ctx, "42")
	if _, err := c.Get(ctx, "42", load); err != nil || loads.Load() != 2 {
		t.Fatalf("after del: err=%v loads=%d", err, loads.Load())
	}
	// negative caching
	var nf atomic.Int32
	nfLoad := func(ctx context.Context) (member, error) { nf.Add(1); return member{}, errNotFound }
	for i := 0; i < 3; i++ {
		if _, err := c.Get(ctx, "404", nfLoad); !errors.Is(err, errNotFound) {
			t.Fatal(err)
		}
	}
	if nf.Load() != 1 {
		t.Fatalf("negative loads=%d", nf.Load())
	}
	// an error that is not the not-found is not cached
	var other atomic.Int32
	otherLoad := func(ctx context.Context) (member, error) { other.Add(1); return member{}, errors.New("db down") }
	for i := 0; i < 2; i++ {
		_, _ = c.Get(ctx, "500", otherLoad)
	}
	if other.Load() != 2 {
		t.Fatal("transient error was cached")
	}
	// Set
	if err := c.Set(ctx, "7", member{UID: 7, Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Get(ctx, "7", load); m.Name != "bob" {
		t.Fatal(m)
	}
	// redis down: read through
	srv.Close()
	before := loads.Load()
	if m, err := c.Get(ctx, "42", load); err != nil || m.Name != "alice" || loads.Load() != before+1 {
		t.Fatalf("read through: %v %+v loads=%d", err, m, loads.Load())
	}
	// nil client: plain loader
	n := NewCache[member](nil, "member", time.Minute)
	if m, err := n.Get(ctx, "1", load); err != nil || m.UID != 42 {
		t.Fatal(err)
	}
}
