package redisx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// WithLock runs fn while holding a lock named name across every replica of
// the service: settling one match, one member's balance change, one
// provider sync. Only one holder at a time; a second caller gets ErrLocked
// at once (or after Wait, see the options). The lock is held for ttl and
// extended in the background while fn runs, so ttl is "how long the
// process may be dead before another one takes over", not an upper bound
// on fn. If the lock is lost (Redis went away, or someone deleted the key)
// fn's ctx is cancelled and WithLock returns ErrLockLost after fn ends; a
// handler that cares checks ctx.Err() before the final write. A mutex in
// the handler locks one replica only, which is why it is not the tool.
//
// The key is "lock:<service>:<name>", the value a random token, so a slow
// holder cannot release a lock that moved on to someone else.
//
// WithLock 在整个服务的所有副本范围内持锁执行 fn：结算一场比赛、改一个会员的余额、同步一个厂商。
// 同时只有一个持有者；第二个调用方立刻得到 ErrLocked（或等 Wait 这么久）。锁的 ttl 在 fn 运行
// 期间后台自动续，所以 ttl 是"进程死掉多久后别人能接手"，不是 fn 的时限。锁丢了（Redis 不可用、
// key 被人删）fn 的 ctx 会被取消，WithLock 在 fn 结束后返回 ErrLockLost。
// handler 里的 sync.Mutex 只锁本副本，所以不是这个用途的工具。
func WithLock(ctx context.Context, rdb *redis.Client, name string, ttl time.Duration, fn func(ctx context.Context) error, opts ...LockOption) error {
	if rdb == nil {
		return errors.New("redisx: WithLock needs redis.enabled: true (a lock that is not shared is not a lock)")
	}
	if name == "" {
		return errors.New("redisx: WithLock needs a name")
	}
	if ttl < time.Second {
		ttl = 10 * time.Second
	}
	var o lockOptions
	for _, opt := range opts {
		opt(&o)
	}
	key := keyOf(rdb, "lock", name)
	token := randomToken()
	metricName := metricLabel(name)
	start := time.Now()
	deadline := start.Add(o.wait)
	for {
		ok, err := rdb.SetNX(ctx, key, token, ttl).Result()
		if err != nil {
			metricsx.Locks.WithLabelValues(metricName, "error").Inc()
			return fmt.Errorf("redisx: lock %s: %w", name, err)
		}
		if ok {
			break
		}
		if o.wait == 0 || time.Now().After(deadline) {
			metricsx.Locks.WithLabelValues(metricName, "busy").Inc()
			return fmt.Errorf("%w: %s", ErrLocked, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.retryEvery()):
		}
	}
	metricsx.Locks.WithLabelValues(metricName, "acquired").Inc()
	metricsx.LockWait.WithLabelValues(metricName).Observe(time.Since(start).Seconds())
	log := zlog.Ctx(ctx).With(zlog.Str("lock", name))
	log.Debug("lock acquired", zlog.Dur("waited", time.Since(start)))

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	lost := make(chan struct{})
	stopKeeper := make(chan struct{})
	keeperDone := make(chan struct{})
	go func() {
		defer close(keeperDone)
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-stopKeeper:
				return
			case <-t.C:
				res, err := extendScript.Run(context.WithoutCancel(ctx), rdb, []string{key}, token, ttl.Milliseconds()).Int()
				if err != nil {
					log.Warn("lock extend failed", zlog.Err(err))
					continue // Redis hiccup: the key still has time; try again next tick
				}
				if res == 0 {
					log.Error("lock lost: the key is gone or held by someone else; cancelling the work")
					metricsx.Locks.WithLabelValues(metricName, "lost").Inc()
					close(lost)
					cancel(ErrLockLost)
					return
				}
			}
		}
	}()
	err := fn(runCtx)
	close(stopKeeper)
	<-keeperDone
	select {
	case <-lost:
		if err == nil {
			err = ErrLockLost
		} else {
			err = fmt.Errorf("%w (%v)", ErrLockLost, err)
		}
		return err
	default:
	}
	if _, rerr := releaseScript.Run(context.WithoutCancel(ctx), rdb, []string{key}, token).Result(); rerr != nil && !errors.Is(rerr, redis.Nil) {
		log.Warn("lock release failed; it expires by itself", zlog.Err(rerr))
	}
	log.Debug("lock released", zlog.Dur("held", time.Since(start)))
	return err
}

// ErrLocked: someone else holds the lock (and Wait, if any, ran out).
var ErrLocked = errors.New("redisx: locked")

// ErrLockLost: the lock was lost while fn ran; its ctx was cancelled.
var ErrLockLost = errors.New("redisx: lock lost")

// LockOption tunes WithLock.
type LockOption func(*lockOptions)

type lockOptions struct {
	wait  time.Duration
	retry time.Duration
}

func (o lockOptions) retryEvery() time.Duration {
	if o.retry > 0 {
		return o.retry
	}
	return 50 * time.Millisecond
}

// Wait makes WithLock retry for up to d before giving up with ErrLocked
// (default: give up at once). A request handler usually waits a little
// (the other holder is quick); a job does not (another run is going).
func Wait(d time.Duration) LockOption { return func(o *lockOptions) { o.wait = d } }

// RetryEvery is the pause between tries while waiting. Default 50ms.
func RetryEvery(d time.Duration) LockOption { return func(o *lockOptions) { o.retry = d } }

var extendScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("pexpire", KEYS[1], ARGV[2]) else return 0 end`)
var releaseScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`)

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// metricLabel keeps the label set finite: "settle:123" becomes "settle".
func metricLabel(name string) string {
	if i := strings.IndexAny(name, ":/"); i > 0 {
		return name[:i]
	}
	return name
}
