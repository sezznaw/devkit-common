// Package idx makes the unique numbers of the business: bet numbers, order
// and statement numbers, anything that must be unique across every replica
// of a service, roughly ordered by time, and not a count anyone can read off.
//
// An id is a 53-bit integer, so that it is exact everywhere it travels: a
// JSON number, a JavaScript Number (whose integers end at 2^53), a Go int64,
// a BIGINT column. The 64-bit snowflake that Twitter, Discord and the big
// Chinese generators use has to be carried as a string past the gateway and
// breaks wherever someone forgets (a front end, a log, a spreadsheet); at our
// scale the extra bits buy nothing, so the id itself is made safe instead.
// The algorithm is the snowflake one, 30 lines; what matters is around it
// (instance leases, clock going back), which no library does for us.
//
// Layout, high to low: 41 bits of milliseconds since 2026-01-01 (good until
// 2095), then 12 bits split between instance and sequence by `id.instance_bits`
// (default 5: 32 instances per service, 128 ids per millisecond per instance,
// 128k a second; a project with few replicas and many ids sets 3 + 9). Ids
// from one instance strictly increase; across instances they are ordered to
// the millisecond. The split can change later: the time bits above it keep
// old and new ids apart.
//
// The instance number is leased in Redis (idx:<service>:<n>, renewed in the
// background, released at shutdown): a new replica takes the first free one,
// a crashed one's frees itself after the lease. Without Redis the number is
// INSTANCE_ID. Without either, only a local run may fall back to a hostname
// hash (with a warning); a deployment fails at the first Next instead of
// risking a duplicate.
//
// idx 生成业务唯一号：注单号、订单号、流水号——跨副本唯一、按时间有序、看不出数量。
// 53 位整数，所以到哪都是精确的：JSON 数字、JavaScript 的 Number（整数到 2^53 为止）、Go 的
// int64、BIGINT 列。64 位雪花过了网关必须转字串，哪里忘了就丢精度；我们的规模用不着那几位，
// 所以让号本身安全。高 41 位毫秒（起点 2026-01-01，够到 2095），低 12 位按 `id.instance_bits`
// 分给实例号和序列（默认 5 + 7：每服务 32 副本，每副本每毫秒 128 个）。实例号靠 Redis 租约；
// 没有 Redis 用 INSTANCE_ID；两者都没有只有本机允许主机名哈希兜底，线上第一次取号就报错。
package idx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/zlog"
)

// Epoch is the zero of the timestamp bits: 2026-01-01T00:00:00Z.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	timeBits   = 41
	lowBits    = 12                 // instance + sequence
	totalBits  = timeBits + lowBits // 53: exact in a float64 / JSON number
	leaseTTL   = 90 * time.Second
	renewEvery = 30 * time.Second
	// DefaultInstanceBits splits the low 12 bits 5 + 7 by default.
	DefaultInstanceBits = 5
)

// MaxID is the largest id the generator makes, 2^53 - 1: JavaScript's
// Number.MAX_SAFE_INTEGER.
const MaxID = 1<<totalBits - 1

// InstanceEnv overrides the instance number when there is no Redis to lease
// one from (0 to 2^instance_bits - 1).
const InstanceEnv = "INSTANCE_ID"

// Config is the `id:` section.
type Config struct {
	// InstanceBits of the low 12: how many replicas of one service may run
	// (2^n) against how many ids each makes per millisecond (2^(12-n)).
	// Default 5: 32 replicas, 128 a millisecond each. 1 to 10.
	InstanceBits int `yaml:"instance_bits"`
}

func (c Config) instanceBits() int {
	if c.InstanceBits == 0 {
		return DefaultInstanceBits
	}
	return c.InstanceBits
}

// Validate checks the split.
func (c Config) Validate() error {
	if b := c.instanceBits(); b < 1 || b > 10 {
		return fmt.Errorf("id.instance_bits %d: 1 to 10 (the other of the 12 low bits are the sequence)", b)
	}
	return nil
}

// Generator makes ids for one instance of a service.
type Generator struct {
	mu       sync.Mutex
	instance int64
	lastMs   int64
	seq      int64

	instanceBits uint
	seqBits      uint
	service      string
	rdb          *redis.Client
	token        string
	stop         chan struct{}
	done         chan struct{}

	// unleased: no Redis and no INSTANCE_ID in a deployment; Next refuses.
	unleased bool
}

// Options of New beyond the configuration.
type Options struct {
	// Local allows the hostname-hash fallback when there is neither Redis
	// nor INSTANCE_ID: a laptop run. A deployment leaves it false and gets
	// an error from Next instead of a possible duplicate.
	Local bool
}

// New makes the generator of service: with rdb it leases an instance number
// (the first free one) and keeps renewing it; without, it reads INSTANCE_ID.
func New(ctx context.Context, rdb *redis.Client, service string, cfg Config, opts Options) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &Generator{service: service, rdb: rdb, token: randomToken(), stop: make(chan struct{}), done: make(chan struct{})}
	g.instanceBits = uint(cfg.instanceBits())
	g.seqBits = uint(lowBits) - g.instanceBits
	if rdb != nil {
		n, err := g.lease(ctx)
		if err != nil {
			return nil, err
		}
		g.instance = n
		go g.keep()
		zlog.Info("id generator ready", zlog.Int("instance", n), zlog.Int("instances_max", g.maxInstance()+1), zlog.Int("per_ms", 1<<g.seqBits), zlog.Str("lease", g.key(n)))
		return g, nil
	}
	close(g.done)
	if v := os.Getenv(InstanceEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > g.maxInstance() {
			return nil, fmt.Errorf("idx: %s=%q: an integer 0 to %d (id.instance_bits %d)", InstanceEnv, v, g.maxInstance(), g.instanceBits)
		}
		g.instance = int64(n)
		zlog.Info("id generator ready", zlog.Int("instance", n), zlog.Str("source", InstanceEnv))
		return g, nil
	}
	if !opts.Local {
		g.unleased = true
		zlog.Warn("id generator has no instance number: redis is off and "+InstanceEnv+" is not set; rt.ID.Next() will fail (a duplicate id is worse)", zlog.Str("hint", "redis.enabled: true leases a number per replica; or the deployment sets "+InstanceEnv))
		return g, nil
	}
	host, _ := os.Hostname()
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	g.instance = int64(h.Sum32()) & int64(g.maxInstance())
	zlog.Warn("id generator without redis: the instance number is a hash of the hostname (local run only)", zlog.Int("instance", g.instance), zlog.Str("hostname", host))
	return g, nil
}

func (g *Generator) maxInstance() int { return 1<<g.instanceBits - 1 }
func (g *Generator) maxSeq() int64    { return 1<<g.seqBits - 1 }

// ErrNoInstance: Next was called in a deployment without Redis and without
// INSTANCE_ID; the generator refuses rather than risk a duplicate.
var ErrNoInstance = errors.New("idx: no instance number: set redis.enabled: true (a lease per replica) or " + InstanceEnv)

// Next is a new id, greater than every id this instance made before. It
// panics with ErrNoInstance when the instance has no number (see New); the
// framework's recovery turns that into a failed request and a clear log.
func (g *Generator) Next() int64 {
	if g.unleased {
		panic(ErrNoInstance)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Since(Epoch).Milliseconds()
	if now < g.lastMs {
		// the clock went back: wait it out rather than repeat an id
		back := time.Duration(g.lastMs-now) * time.Millisecond
		if back > 5*time.Second {
			zlog.Error("idx: clock went back by more than 5s; ids wait for it", zlog.Dur("back", back))
		}
		time.Sleep(back)
		now = g.lastMs
	}
	if now == g.lastMs {
		g.seq++
		if g.seq > g.maxSeq() {
			for now <= g.lastMs {
				time.Sleep(50 * time.Microsecond)
				now = time.Since(Epoch).Milliseconds()
			}
			g.seq = 0
		}
	} else {
		g.seq = 0
	}
	g.lastMs = now
	return now<<lowBits | g.instance<<g.seqBits | g.seq
}

// Instance is this generator's instance number.
func (g *Generator) Instance() int { return int(g.instance) }

// Close releases the instance lease (the runtime does at shutdown).
func (g *Generator) Close() error {
	if g.rdb == nil {
		return nil
	}
	select {
	case <-g.stop:
	default:
		close(g.stop)
	}
	<-g.done
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := releaseScript.Run(ctx, g.rdb, []string{g.key(g.instance)}, g.token).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

// Time is when id was made (to the millisecond).
func Time(id int64) time.Time {
	return Epoch.Add(time.Duration(id>>lowBits) * time.Millisecond)
}

// Instance is the instance number that made id, for a generator with
// instanceBits of instance bits (the default when 0).
func Instance(id int64, instanceBits int) int {
	if instanceBits == 0 {
		instanceBits = DefaultInstanceBits
	}
	return int(id >> (lowBits - uint(instanceBits)) & (1<<instanceBits - 1))
}

func (g *Generator) key(n int64) string { return "idx:" + g.service + ":" + strconv.FormatInt(n, 10) }

// lease takes the first free instance number.
func (g *Generator) lease(ctx context.Context) (int64, error) {
	host, _ := os.Hostname()
	val := host + "/" + g.token
	for n := int64(0); n <= int64(g.maxInstance()); n++ {
		ok, err := g.rdb.SetNX(ctx, g.key(n), val, leaseTTL).Result()
		if err != nil {
			return 0, fmt.Errorf("idx: lease an instance number: %w", err)
		}
		if ok {
			return n, nil
		}
	}
	return 0, fmt.Errorf("idx: all %d instance numbers of %s are leased (raise id.instance_bits, or a replica did not release its lease: they expire in %s)", g.maxInstance()+1, g.service, leaseTTL)
}

// keep renews the lease; a lost lease (Redis flushed, key expired while
// Redis was unreachable) is re-taken, or a new number is leased.
func (g *Generator) keep() {
	defer close(g.done)
	t := time.NewTicker(renewEvery)
	defer t.Stop()
	host, _ := os.Hostname()
	val := host + "/" + g.token
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			res, err := renewScript.Run(ctx, g.rdb, []string{g.key(g.instance)}, g.token, leaseTTL.Milliseconds()).Int()
			if err == nil && res == 0 {
				ok, serr := g.rdb.SetNX(ctx, g.key(g.instance), val, leaseTTL).Result()
				if serr == nil && ok {
					zlog.Warn("idx: instance lease had expired; taken again", zlog.Int("instance", g.instance))
				} else if serr == nil {
					n, lerr := g.lease(ctx)
					if lerr == nil {
						g.mu.Lock()
						g.instance = n
						g.mu.Unlock()
						zlog.Error("idx: instance number was taken by another replica; moved", zlog.Int("instance", n))
					} else {
						zlog.Error("idx: instance lease lost and none free", zlog.Err(lerr))
					}
				}
			} else if err != nil {
				zlog.Warn("idx: lease renew failed; retrying next tick", zlog.Err(err))
			}
			cancel()
		}
	}
}

var renewScript = redis.NewScript(`if string.find(redis.call("get", KEYS[1]) or "", ARGV[1], 1, true) then return redis.call("pexpire", KEYS[1], ARGV[2]) else return 0 end`)
var releaseScript = redis.NewScript(`if string.find(redis.call("get", KEYS[1]) or "", ARGV[1], 1, true) then return redis.call("del", KEYS[1]) else return 0 end`)

func randomToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
