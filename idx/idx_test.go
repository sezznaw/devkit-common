package idx

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestUniqueOrderedAndSafe(t *testing.T) {
	zlogtest.Discard(t)
	t.Setenv(InstanceEnv, "7")
	g, err := New(context.Background(), nil, "bet", Config{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const n = 100000
	var mu sync.Mutex
	seen := make(map[int64]struct{}, n)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			last := int64(0)
			for i := 0; i < n/8; i++ {
				id := g.Next()
				if id <= last {
					t.Errorf("not increasing: %d after %d", id, last)
				}
				if id > MaxID {
					t.Errorf("id %d exceeds 2^53-1", id)
				}
				last = id
				mu.Lock()
				seen[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("%d unique of %d", len(seen), n)
	}
	id := g.Next()
	if Instance(id, 0) != 7 || g.Instance() != 7 {
		t.Fatalf("instance %d", Instance(id, 0))
	}
	if d := time.Since(Time(id)); d < 0 || d > time.Second {
		t.Fatalf("time %s", d)
	}
	if MaxID != 9007199254740991 {
		t.Fatal("MaxID is not Number.MAX_SAFE_INTEGER")
	}
}

func TestSplit(t *testing.T) {
	zlogtest.Discard(t)
	t.Setenv(InstanceEnv, "5")
	g, err := New(context.Background(), nil, "bet", Config{InstanceBits: 3}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if g.maxInstance() != 7 || g.maxSeq() != 511 {
		t.Fatalf("3+9 split: %d %d", g.maxInstance(), g.maxSeq())
	}
	if Instance(g.Next(), 3) != 5 {
		t.Fatal("instance decode with 3 bits")
	}
	t.Setenv(InstanceEnv, "9")
	if _, err := New(context.Background(), nil, "bet", Config{InstanceBits: 3}, Options{}); err == nil {
		t.Fatal("instance 9 does not fit 3 bits")
	}
	if _, err := New(context.Background(), nil, "bet", Config{InstanceBits: 11}, Options{}); err == nil {
		t.Fatal("11 instance bits accepted")
	}
}

func TestNoInstanceInDeployment(t *testing.T) {
	zlogtest.Discard(t)
	t.Setenv(InstanceEnv, "")
	g, err := New(context.Background(), nil, "bet", Config{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		r := recover()
		if e, ok := r.(error); !ok || !errors.Is(e, ErrNoInstance) {
			t.Fatalf("want ErrNoInstance panic, got %v", r)
		}
	}()
	g.Next()
	t.Fatal("Next without an instance number produced an id")
}

func TestLocalFallback(t *testing.T) {
	zlogtest.Discard(t)
	t.Setenv(InstanceEnv, "")
	g, err := New(context.Background(), nil, "bet", Config{}, Options{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	if g.Next() <= 0 {
		t.Fatal("no id")
	}
}

func TestLease(t *testing.T) {
	zlogtest.Discard(t)
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()
	ctx := context.Background()
	a, err := New(ctx, rdb, "bet", Config{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(ctx, rdb, "bet", Config{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Instance() != 0 || b.Instance() != 1 {
		t.Fatalf("first free numbers: %d %d", a.Instance(), b.Instance())
	}
	c, _ := New(ctx, rdb, "wallet", Config{}, Options{})
	if c.Instance() != 0 {
		t.Fatal("another service has its own numbers")
	}
	seen := map[int64]bool{}
	for i := 0; i < 10000; i++ {
		for _, g := range []*Generator{a, b} {
			id := g.Next()
			if seen[id] {
				t.Fatal("collision")
			}
			seen[id] = true
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if srv.Exists("idx:bet:0") {
		t.Fatal("lease not released")
	}
	d, _ := New(ctx, rdb, "bet", Config{}, Options{})
	if d.Instance() != 0 {
		t.Fatalf("freed number not reused: %d", d.Instance())
	}
	// all numbers taken: a clear error
	var gens []*Generator
	for i := 0; i < 30; i++ {
		g, err := New(ctx, rdb, "bet", Config{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, g)
	}
	if _, err := New(ctx, rdb, "bet", Config{}, Options{}); err == nil {
		t.Fatal("33rd replica got a number")
	}
	for _, g := range append(gens, b, c, d) {
		g.Close()
	}
}
