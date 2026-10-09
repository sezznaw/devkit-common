package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Cache is a read-through cache of one kind of value in Redis: Get returns
// the cached copy or runs the loader (once per key per process at a time,
// so a hot key does not stampede the database) and stores what it returns
// for the TTL (± 10%, so keys written together do not expire together).
// Redis being down makes Get a plain call of the loader, logged once a
// minute; a cache is an optimisation, never the source of truth. Writes to
// the thing cached call Del after their transaction commits (before it, a
// concurrent read could re-fill the old value).
//
// With Negative, a loader error matching it (a "not found") is remembered
// for the shorter negativeTTL and returned without asking again.
//
// Keys are "cache:<service>:<name>:<id>". The value is JSON, so a cached
// type's exported fields are what is kept; a type that changes shape is
// given a new name (or a version suffix) to miss on old entries.
//
// Cache 是一种值的读穿缓存：Get 返回缓存，没有就跑 loader（同一个 key 同一进程同时只跑一次，
// 热 key 不会把数据库打穿），结果按 TTL（±10% 抖动）存起来。Redis 不可用时 Get 就是直接调
// loader，每分钟记一条日志；缓存是优化，不是真相来源。写方在事务提交之后调 Del（提交前删，
// 并发的读会把旧值填回去）。Negative 让"查无此物"也被记住一小段时间。
type Cache[T any] struct {
	rdb         *redis.Client
	name        string
	ttl         time.Duration
	notFound    error
	negativeTTL time.Duration
	flight      sync.Map // id -> *call
	warnedAt    time.Time
	warnMu      sync.Mutex
}

// NewCache makes the cache named name (one word, the kind of thing:
// "member", "match-odds") with ttl per entry. rdb may be nil (redis off):
// every Get then runs the loader.
func NewCache[T any](rdb *redis.Client, name string, ttl time.Duration) *Cache[T] {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Cache[T]{rdb: rdb, name: name, ttl: ttl}
}

// Negative makes the cache remember a loader error that errors.Is(err,
// notFound) for negativeTTL (default ttl/10, at least 5s), and return it
// from the cache meanwhile.
func (c *Cache[T]) Negative(notFound error, negativeTTL time.Duration) *Cache[T] {
	c.notFound = notFound
	if negativeTTL <= 0 {
		negativeTTL = max(c.ttl/10, 5*time.Second)
	}
	c.negativeTTL = negativeTTL
	return c
}

const tombstone = "\x00nf"

type call struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Get returns the value for id, from the cache or from load.
func (c *Cache[T]) Get(ctx context.Context, id string, load func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	key := keyOf(c.rdb, "cache", c.name+":"+id)
	if c.rdb != nil {
		raw, err := c.rdb.Get(ctx, key).Result()
		switch {
		case err == nil && raw == tombstone && c.notFound != nil:
			metricsx.CacheRequests.WithLabelValues(c.name, "negative").Inc()
			return zero, c.notFound
		case err == nil:
			var v T
			if jerr := json.Unmarshal([]byte(raw), &v); jerr == nil {
				metricsx.CacheRequests.WithLabelValues(c.name, "hit").Inc()
				return v, nil
			}
			// unreadable entry (type changed): fall through to the loader and overwrite
		case errors.Is(err, redis.Nil):
		default:
			c.warn(ctx, err)
			metricsx.CacheRequests.WithLabelValues(c.name, "error").Inc()
			return load(ctx)
		}
	}
	metricsx.CacheRequests.WithLabelValues(c.name, "miss").Inc()
	// one loader per key per process; others wait for its result
	cl := &call{}
	cl.wg.Add(1)
	if actual, loaded := c.flight.LoadOrStore(id, cl); loaded {
		ac := actual.(*call)
		ac.wg.Wait()
		if ac.err != nil {
			return zero, ac.err
		}
		return ac.val.(T), nil
	}
	v, err := load(ctx)
	cl.val, cl.err = v, err
	cl.wg.Done()
	c.flight.Delete(id)
	if err != nil {
		if c.rdb != nil && c.notFound != nil && errors.Is(err, c.notFound) {
			if serr := c.rdb.Set(context.WithoutCancel(ctx), key, tombstone, c.negativeTTL).Err(); serr != nil {
				c.warn(ctx, serr)
			}
		}
		return zero, err
	}
	if c.rdb != nil {
		b, jerr := json.Marshal(v)
		if jerr != nil {
			return v, fmt.Errorf("redisx: cache %s: encode: %w", c.name, jerr)
		}
		if serr := c.rdb.Set(context.WithoutCancel(ctx), key, b, jitter(c.ttl)).Err(); serr != nil {
			c.warn(ctx, serr)
		}
	}
	return v, nil
}

// Set stores v for id (a write path that already has the new value).
func (c *Cache[T]) Set(ctx context.Context, id string, v T) error {
	if c.rdb == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("redisx: cache %s: encode: %w", c.name, err)
	}
	return c.rdb.Set(ctx, keyOf(c.rdb, "cache", c.name+":"+id), b, jitter(c.ttl)).Err()
}

// Del drops the entries of ids, after the change that made them stale
// committed. A Redis error is logged, not returned: the entry expires by
// its TTL, and the write already happened.
func (c *Cache[T]) Del(ctx context.Context, ids ...string) {
	if c.rdb == nil || len(ids) == 0 {
		return
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = keyOf(c.rdb, "cache", c.name+":"+id)
	}
	if err := c.rdb.Del(context.WithoutCancel(ctx), keys...).Err(); err != nil {
		c.warn(ctx, err)
	}
}

func (c *Cache[T]) warn(ctx context.Context, err error) {
	c.warnMu.Lock()
	defer c.warnMu.Unlock()
	if time.Since(c.warnedAt) < time.Minute {
		return
	}
	c.warnedAt = time.Now()
	zlog.Ctx(ctx).Warn("cache unavailable; reading through", zlog.Str("cache", c.name), zlog.Err(err))
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}
