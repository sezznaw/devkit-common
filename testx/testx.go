// Package testx is what a service's tests build on: a Runtime like the one
// the framework hands app.Setup, but on throwaway infrastructure that lives
// for one test (Redis in-process, Kafka in-process, a fake Centrifugo that
// records what was pushed, a MySQL database created from the project's
// migrations and dropped afterwards), identities for the ctx a handler
// reads its uid from, and JSON helpers for a gateway's routes. A handler
// test is then: build the Runtime, call app.Setup, call the handler.
//
// MySQL is the one thing not in-process: WithMySQL(migrationsDir) connects
// to MYSQL_TEST_DSN_ROOT (the Makefile points it at the local stack, CI at a
// service container) and skips the test when no server answers, so a test
// never fails for the lack of a database on a laptop.
//
// testx 是服务测试的底座：一个和框架交给 app.Setup 的一样的 Runtime，但底下是只活一个测试的
// 一次性设施（进程内 Redis、进程内 Kafka、记录推送内容的假 Centrifugo、按项目迁移建出来、
// 测完就删的 MySQL 库），还有 handler 从 ctx 取 uid 用的身份、网关路由的 JSON 助手。
// handler 测试就三步：建 Runtime、调 app.Setup、调 handler。MySQL 不在进程内：WithMySQL
// 连 MYSQL_TEST_DSN_ROOT（Makefile 指向本机一键环境，CI 指向服务容器），连不上就跳过。
package testx

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"gorm.io/gorm"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/centrifugox"
	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/delayx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/mysqlx"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// DSNEnv names the root DSN of the test MySQL server:
// "root:root@tcp(127.0.0.1:3306)/". Databases are created and dropped on it.
const DSNEnv = "MYSQL_TEST_DSN_ROOT"

// Runtime is the framework Runtime plus what a test wants to look at.
type Runtime struct {
	*kitexx.Runtime
	// MiniRedis is the in-process Redis behind rt.Redis (FastForward to
	// expire keys, Keys to look).
	MiniRedis *miniredis.Miniredis
	// Brokers are the in-process Kafka's addresses, for a consumer of your own.
	Brokers []string

	t       testing.TB
	topics  []string
	pushMu  sync.Mutex
	pushes  []Push
	started bool
}

// Push is one message the service published to Centrifugo.
type Push struct {
	Channel string
	Data    json.RawMessage
}

// Option tunes New.
type Option func(*options)

type options struct {
	migrations string
	topics     []string
	realm      authx.Realm
}

// WithMySQL gives the Runtime a database created from the `*.up.sql` files
// of dir (the project's migrations, in order), dropped when the test ends.
// Needs MYSQL_TEST_DSN_ROOT; without a server the test is skipped.
func WithMySQL(migrationsDir string) Option { return func(o *options) { o.migrations = migrationsDir } }

// WithTopics creates these Kafka topics up front (the ones the service
// publishes to or subscribes); auto-creation covers the rest.
func WithTopics(topics ...string) Option {
	return func(o *options) { o.topics = append(o.topics, topics...) }
}

// New builds the Runtime of service for one test. Redis (miniredis), Kafka
// (kfake) and Centrifugo (a recorder) are always there; MySQL with WithMySQL.
// Logs are discarded (zlogtest.Discard).
//
// New 为一个测试建 service 的 Runtime：Redis、Kafka、Centrifugo 总是有（进程内 / 假的），
// MySQL 用 WithMySQL。日志丢弃。
func New(t testing.TB, service string, opts ...Option) *Runtime {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	zlogtest.Discard(t)
	ctx := context.Background()
	rt := &Runtime{Runtime: &kitexx.Runtime{}, t: t, topics: o.topics}
	rt.Config.Service.Name = service
	rt.Config.RegistryDisabled = true

	// Redis
	rt.MiniRedis = miniredis.RunT(t)
	rdb, err := redisx.Open(ctx, redisx.Target{Addr: rt.MiniRedis.Addr()}, redisx.Config{}, service)
	if err != nil {
		t.Fatalf("testx: redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	rt.Redis = rdb
	rt.Config.Redis.Enabled = true

	// MySQL
	if o.migrations != "" {
		rt.DB = openMySQL(t, service, o.migrations)
		rt.Config.MySQL.Enabled = true
		rt.Delay = delayx.New(rt.DB, delayx.Config{PollInterval: config.Duration(50 * time.Millisecond)}, service)
	}

	// Kafka
	cl, err := kfake.NewCluster(kfake.SeedTopics(-1, o.topics...), kfake.NumBrokers(1))
	if err != nil {
		t.Fatalf("testx: kafka: %v", err)
	}
	t.Cleanup(cl.Close)
	rt.Brokers = cl.ListenAddrs()
	kcfg := kafkax.Config{Enabled: true, Brokers: rt.Brokers}
	kc, err := kafkax.Open(ctx, kcfg.Static(), kcfg, service, "test")
	if err != nil {
		t.Fatalf("testx: kafka client: %v", err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	rt.Kafka = kc
	rt.Config.Kafka = kcfg

	// Centrifugo: an API server that records publishes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		var params map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&params)
		if method == "publish" {
			var ch string
			_ = json.Unmarshal(params["channel"], &ch)
			rt.pushMu.Lock()
			rt.pushes = append(rt.pushes, Push{Channel: ch, Data: params["data"]})
			rt.pushMu.Unlock()
		}
		_, _ = w.Write([]byte(`{"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	cc, err := centrifugox.Open(ctx, centrifugox.Target{APIAddr: srv.URL, APIKey: "test", TokenSecret: "test-secret-test-secret-test-secret!"}, centrifugox.Config{})
	if err != nil {
		t.Fatalf("testx: centrifugo: %v", err)
	}
	rt.Centrifugo = cc
	rt.Config.Centrifugo.Enabled = true
	return rt
}

// Start runs what the server would start after app.Setup: the Kafka
// consumers it subscribed, the outbox relay and the delayed-task poller
// (both when there is a database). Call it after app.Setup when the test
// needs events delivered or tasks run on their own; RunDue runs due tasks
// without the poller.
func (rt *Runtime) Start() {
	rt.t.Helper()
	if rt.started {
		return
	}
	rt.started = true
	ctx := context.Background()
	if err := rt.Kafka.Start(ctx); err != nil {
		rt.t.Fatalf("testx: kafka consumers: %v", err)
	}
	if rt.DB != nil {
		rt.Kafka.StartOutbox(ctx, rt.DB, kafkax.OutboxConfig{PollInterval: config.Duration(50 * time.Millisecond)})
		rt.t.Cleanup(rt.Kafka.StopOutbox)
		if rt.Delay.Handlers() > 0 {
			rt.Delay.Start(ctx)
			rt.t.Cleanup(rt.Delay.Stop)
		}
	}
}

// RunDue runs the delayed tasks whose time has come (or that were
// scheduled for the past), once, and returns how many ran. A test that
// schedules a task for "in 15 minutes" sets its run_at back and calls this
// instead of waiting: `rt.DB.Exec("UPDATE delayed_task SET run_at = NOW(3)")`.
func (rt *Runtime) RunDue() int {
	rt.t.Helper()
	if rt.Delay == nil {
		rt.t.Fatal("testx: RunDue needs WithMySQL")
	}
	n, err := rt.Delay.RunOnce(context.Background())
	if err != nil {
		rt.t.Fatalf("testx: run due tasks: %v", err)
	}
	return n
}

// Pushes are the messages published to Centrifugo so far, in order.
func (rt *Runtime) Pushes() []Push {
	rt.pushMu.Lock()
	defer rt.pushMu.Unlock()
	return append([]Push(nil), rt.pushes...)
}

// WaitPush waits up to timeout for a push on channel and returns it.
func (rt *Runtime) WaitPush(channel string, timeout time.Duration) Push {
	rt.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, p := range rt.Pushes() {
			if p.Channel == channel {
				return p
			}
		}
		if time.Now().After(deadline) {
			rt.t.Fatalf("testx: no push on %s within %s (got %v)", channel, timeout, rt.Pushes())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Events reads every event published to topic so far (from the beginning;
// the outbox relay must have run: Start, or wait). Events of a topic the
// service writes through PublishTx appear once the relay sent them.
func (rt *Runtime) Events(topic string, timeout time.Duration) []kafkax.Event {
	rt.t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(rt.Brokers...), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		rt.t.Fatalf("testx: consumer: %v", err)
	}
	defer cl.Close()
	var out []kafkax.Event
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		fetches := cl.PollFetches(ctx)
		cancel()
		fetches.EachRecord(func(r *kgo.Record) {
			var ev kafkax.Event
			if json.Unmarshal(r.Value, &ev) == nil {
				out = append(out, ev)
			}
		})
		if len(out) > 0 && fetches.Empty() {
			break
		}
	}
	return out
}

// WaitEvent waits up to timeout for an event of type typ on topic.
func (rt *Runtime) WaitEvent(topic, typ string, timeout time.Duration) kafkax.Event {
	rt.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, ev := range rt.Events(topic, 300*time.Millisecond) {
			if ev.Type == typ {
				return ev
			}
		}
		if time.Now().After(deadline) {
			rt.t.Fatalf("testx: no %s event on %s within %s", typ, topic, timeout)
		}
	}
}

// Ctx is a request context of a logged-in member with uid: what a handler
// reads with kitexx.UID / MustUID. CtxAs picks the realm.
func Ctx(uid int64) context.Context { return CtxAs(authx.RealmMember, uid) }

// CtxAs is a request context of a login in realm with uid.
func CtxAs(realm authx.Realm, uid int64) context.Context {
	return kitexx.WithIdentity(context.Background(), authx.Identity{Realm: realm, UID: uid})
}

// openMySQL creates a fresh database on MYSQL_TEST_DSN_ROOT, applies the
// migrations and returns the service's handle; the database is dropped at
// the end of the test. No server: the test is skipped.
func openMySQL(t testing.TB, service, migrationsDir string) *gorm.DB {
	t.Helper()
	root := os.Getenv(DSNEnv)
	if root == "" {
		t.Skipf("testx: %s not set; a database test needs a MySQL (make test sets it for the local stack)", DSNEnv)
	}
	dsn, err := gomysql.ParseDSN(root)
	if err != nil {
		t.Fatalf("testx: %s: %v", DSNEnv, err)
	}
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatalf("testx: open %s: %v", DSNEnv, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("testx: no MySQL at %s (%v); start the local stack (infra/local/local.sh up) to run database tests", dsn.Addr, err)
	}
	name := fmt.Sprintf("test_%s_%d", strings.ReplaceAll(service, "-", "_"), time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("testx: create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = admin.Close()
	})
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("testx: no *.up.sql in %s (the migrations directory; make test sets MIGRATIONS_DIR)", migrationsDir)
	}
	sort.Strings(files)
	dbDSN := *dsn
	dbDSN.DBName = name
	dbDSN.MultiStatements = true
	conn, err := sql.Open("mysql", dbDSN.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(string(b)); err != nil {
			t.Fatalf("testx: migration %s: %v", filepath.Base(f), err)
		}
	}
	_ = conn.Close()
	db, err := mysqlx.Open(context.Background(), mysqlx.Target{Addr: dsn.Addr, DB: name, User: dsn.User, Password: dsn.Passwd}, mysqlx.Config{})
	if err != nil {
		t.Fatalf("testx: open test database: %v", err)
	}
	return db
}
