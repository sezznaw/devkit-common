// Package testx is what a service's tests build on: a Runtime like the one
// the framework hands app.Setup, but on throwaway infrastructure that lives
// for one test (Redis in-process, Kafka in-process, a fake Centrifugo that
// records what was pushed, a MySQL database that is empty when the test
// starts), identities for the ctx a handler reads its uid from, and JSON
// helpers for a gateway's routes. A handler test is then: build the Runtime,
// call app.Setup, call the handler.
//
// MySQL is the one thing not in-process: WithMySQL(migrationsDir) connects
// to MYSQL_TEST_DSN_ROOT (the Makefile points it at the local stack, CI at a
// service container) and skips the test when no server answers, so a test
// never fails for the lack of a database on a laptop.
//
// testx 是服务测试的底座：一个和框架交给 app.Setup 的一样的 Runtime，但底下是只活一个测试的
// 一次性设施（进程内 Redis、进程内 Kafka、记录推送内容的假 Centrifugo、测试开始时是空的 MySQL 库），
// 还有 handler 从 ctx 取 uid 用的身份、网关路由的 JSON 助手。
// handler 测试就三步：建 Runtime、调 app.Setup、调 handler。MySQL 不在进程内：WithMySQL
// 连 MYSQL_TEST_DSN_ROOT（Makefile 指向本机一键环境，CI 指向服务容器），连不上就跳过。
package testx

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	"github.com/sezznaw/devkit-common/idx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/mysqlx"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// DSNEnv names the root DSN of the test MySQL server:
// "root:root@tcp(127.0.0.1:3306)/". Databases are created and dropped on it.
const DSNEnv = "MYSQL_TEST_DSN_ROOT"

// MigrationsEnv names the directory of the project's `*.up.sql` files; the
// template's Makefile sets it (MIGRATIONS_DIR, absolute), so a test writes
// testx.WithMySQL(testx.MigrationsDir()) wherever the migrations live.
const MigrationsEnv = "MIGRATIONS_DIR"

// RequireEnv set to 1 (CI does) turns "no MySQL answers" from a skip into a
// failure, after waiting up to a minute for the service container.
const RequireEnv = "TESTX_REQUIRE_MYSQL"

// MigrationsDir is $MIGRATIONS_DIR, the project's migrations as make test
// passes them; "" when run outside make (WithMySQL then says what to set).
func MigrationsDir() string { return os.Getenv(MigrationsEnv) }

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
	mysql      bool
	fresh      bool
	migrations string
	models     []any
	topics     []string
	realm      authx.Realm
}

// WithFreshDatabase gives the test a database of its own, created for it and
// dropped when it ends, instead of the process's shared one emptied: for a
// test about the schema itself (AutoMigrate, a table that must not exist yet,
// the same table name with different columns in different tests). Implies
// WithMySQL("").
//
// WithFreshDatabase 给这个测试一个自己的新库（测完删掉），不用本进程清空过的共用库：
// 测表结构本身的测试用（AutoMigrate、要求某张表还不存在、不同测试里同名表的列不一样）。含 WithMySQL("")。
func WithFreshDatabase() Option {
	return func(o *options) { o.mysql = true; o.fresh = true }
}

// WithMySQL gives the Runtime a database that is empty when the test starts
// (no rows, AUTO_INCREMENT at 1), with the framework's tables (outbox,
// delayed_task) in it; the service's own tables come from WithModels. The
// tests of one process take turns on one database per service, emptied
// between them (creating a database and its tables for every test was most
// of a database test's cost); a test that asks while another one still holds
// it (t.Parallel, a second Runtime in the same test) gets a fresh database,
// dropped when it ends. dir may name a directory of `*.up.sql` files to apply
// first (a project that still has SQL migrations: then every test gets a
// fresh database, so the migrations' seed rows survive), or be "". A test
// about the schema itself adds WithFreshDatabase; one that alters a table it
// found gets the shared database dropped when it ends (see watchSchema).
// Needs MYSQL_TEST_DSN_ROOT; without a server the test is skipped.
//
// WithMySQL 给 Runtime 一个测试开始时是空的库（没有数据、AUTO_INCREMENT 从 1 开始），框架的表
// （outbox、delayed_task）已建好；本服务的表用 WithModels。同一个进程的测试依次复用每个服务一个的库，
// 测试之间清空（以前每个测试都建库建表，那是数据库测试的大头开销）；别的测试还占着它时（t.Parallel、
// 同一个测试建第二个 Runtime）给一个测完就删的新库。dir 可以是 *.up.sql 的目录（仍用 SQL 迁移的项目：
// 每个测试都给新库，迁移里的种子数据不会被清掉），一般传 ""。测表结构本身的测试加 WithFreshDatabase；
// 改了已有表结构的测试，测完共用库会被删掉（见 watchSchema）。
func WithMySQL(migrationsDir string) Option {
	return func(o *options) { o.mysql = true; o.migrations = migrationsDir }
}

// WithModels creates the tables of these models (the project's
// lib/model/mysql structs) in the test database after the migrations, the
// way the service does at start-up with rt.AutoMigrate. Use it with
// WithMySQL for the tables the service owns.
//
// WithModels 在迁移之后按这些模型（项目 lib/model/mysql 的结构体）建表，和服务启动时的
// rt.AutoMigrate 一样；和 WithMySQL 一起用，列出本服务拥有的表。
func WithModels(models ...any) Option {
	return func(o *options) { o.models = append(o.models, models...) }
}

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
	config.DisableTakeover()
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
	gen, err := idx.New(ctx, rdb, service, idx.Config{}, idx.Options{})
	if err != nil {
		t.Fatalf("testx: id generator: %v", err)
	}
	t.Cleanup(func() { _ = gen.Close() })
	rt.ID = gen

	// MySQL
	if o.mysql || o.migrations != "" {
		rt.DB = openMySQL(t, service, o.migrations, o.fresh)
		rt.Config.MySQL.Enabled = true
		on := true
		rt.Config.MySQL.AutoMigrate = &on
		rt.Config.Kafka.Enabled = true // the framework tables a service would have: outbox and delayed_task
		if err := rt.EnsureFrameworkTables(); err != nil {
			t.Fatalf("testx: framework tables: %v", err)
		}
		if len(o.models) > 0 {
			if err := rt.AutoMigrate(o.models...); err != nil {
				t.Fatalf("testx: WithModels: %v", err)
			}
		}
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
	kc.SetDedupe(rdb)
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
	// long kinds (delayx.Long) run in their pool: wait for them too
	deadline := time.Now().Add(30 * time.Second)
	for rt.Delay.Running() > 0 {
		if time.Now().After(deadline) {
			rt.t.Fatal("testx: delayed tasks still running after 30s")
		}
		time.Sleep(10 * time.Millisecond)
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

// openMySQL returns the service's handle on an empty database of
// MYSQL_TEST_DSN_ROOT: the process's shared one, emptied, or a fresh one with
// the migrations applied (see WithMySQL); at the end of the test the handle's
// pool is closed and a fresh database dropped. No server: the test is skipped.
//
// openMySQL 在 MYSQL_TEST_DSN_ROOT 上给出一个空库的句柄：本进程共用的那个（先清空），或一个新库
// （跑过迁移，见 WithMySQL）；测试结束时先关这个句柄的连接池，新库再删掉（以前连接池不关，每个测试
// 留下最多 5 条空闲连接直到进程退出，go test -count 大时把本机 MySQL 的连接数打满，报 1040）。
// 连不上就跳过。
func openMySQL(t testing.TB, service, migrationsDir string, fresh bool) *gorm.DB {
	t.Helper()
	require := os.Getenv(RequireEnv) == "1"
	root := os.Getenv(DSNEnv)
	if root == "" {
		if require {
			t.Fatalf("testx: %s=1 but %s is not set", RequireEnv, DSNEnv)
		}
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
	wait := 3 * time.Second
	if require {
		wait = 60 * time.Second // a CI service container is still starting
	}
	deadline := time.Now().Add(wait)
	var pingErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pingErr = admin.PingContext(ctx)
		cancel()
		if pingErr == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if pingErr != nil {
		_ = admin.Close()
		if require {
			t.Fatalf("testx: no MySQL at %s after %s: %v", dsn.Addr, wait, pingErr)
		}
		t.Skipf("testx: no MySQL at %s (%v); start the project's local stack (its local/local.sh up) to run database tests", dsn.Addr, pingErr)
	}
	// Cleanups run last first: the pool of the handle (registered at the
	// end) is closed, then the database dropped (or released), then admin closed.
	t.Cleanup(func() { _ = admin.Close() })
	if migrationsDir == "" && !fresh {
		if name, ok := acquireShared(t, admin, root, service); ok {
			emptyDatabase(t, admin, name)
			watchSchema(t, admin, root, service, name)
			return openService(t, dsn, name)
		}
	}
	name := fmt.Sprintf("test_%s_%d", strings.ReplaceAll(service, "-", "_"), time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("testx: create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
	var files []string
	if migrationsDir != "" {
		files, err = filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
		if err != nil || len(files) == 0 {
			t.Fatalf("testx: no *.up.sql in %s (WithMySQL(\"\") for a project without SQL migrations)", migrationsDir)
		}
		sort.Strings(files)
	}
	dbDSN := *dsn
	dbDSN.DBName = name
	dbDSN.MultiStatements = true
	conn, err := sql.Open("mysql", dbDSN.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(string(b)); err != nil {
			t.Fatalf("testx: migration %s: %v", filepath.Base(f), err)
		}
	}
	return openService(t, dsn, name)
}

// openService opens the service's handle on database name, closed when the
// test ends.
func openService(t testing.TB, dsn *gomysql.Config, name string) *gorm.DB {
	t.Helper()
	db, err := mysqlx.Open(context.Background(), mysqlx.Target{Addr: dsn.Addr, DB: name, User: dsn.User, Password: dsn.Passwd}, mysqlx.Config{})
	if err != nil {
		t.Fatalf("testx: open test database: %v", err)
	}
	t.Cleanup(func() { _ = mysqlx.Close(db) })
	return db
}

// One database per test process and service, reused by its tests one after
// another. Creating a database and its tables for every test was most of what
// a database test cost: DDL is a few dozen fsyncs per table, so a handler
// package of 60 tests did 60 000 of them (minutes on a CI volume). A test now
// gets the shared database emptied (rows deleted, AUTO_INCREMENT back to 1,
// the tables kept); the AutoMigrate that follows only creates what is missing,
// such as a table an earlier test dropped. A test that asks while another one
// of the process still holds it (t.Parallel, or a second Runtime in the same
// test) gets a fresh database of its own, as before, and so does a test with
// SQL migrations, whose seed rows emptying would lose.
//
// 每个测试进程、每个服务一个库，测试依次复用。以前每个测试都建库建表，DDL 每张表几十次 fsync，
// 一个 60 个测试的 handler 包就是 6 万次，在 CI 的盘上要几分钟。现在测试拿到的是清空过的共用库
// （删数据、AUTO_INCREMENT 归 1、表保留），随后的 AutoMigrate 只补缺的表（比如前一个测试删掉的）。
// 进程里还有别的测试占着它时（t.Parallel，或同一个测试建第二个 Runtime）照旧给一个新库；
// 带 SQL 迁移的测试也照旧，因为清空会丢掉迁移里的种子数据。
var shared struct {
	mu  sync.Mutex
	dbs map[string]*sharedDB // root DSN + service
}

type sharedDB struct {
	name  string
	inUse bool
}

// acquireShared returns the process's shared database of service, creating
// it on first use, and holds it until the test ends; ok is false when another
// test holds it.
func acquireShared(t testing.TB, admin *sql.DB, root, service string) (name string, ok bool) {
	t.Helper()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.dbs == nil {
		shared.dbs = map[string]*sharedDB{}
	}
	key := sharedKey(root, service)
	s := shared.dbs[key]
	if s != nil && s.inUse {
		return "", false
	}
	if s == nil {
		dropAbandoned(admin)
		s = &sharedDB{name: fmt.Sprintf("%s%d", sharedPrefix(service), os.Getpid())}
		// one by this name can only be a finished process's that had the same pid: drop it, its schema may be old
		if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + s.name + "`"); err != nil {
			t.Fatalf("testx: drop a stale database: %v", err)
		}
		if _, err := admin.Exec("CREATE DATABASE `" + s.name + "` CHARACTER SET utf8mb4"); err != nil {
			t.Fatalf("testx: create database: %v", err)
		}
		shared.dbs[key] = s
	}
	s.inUse = true
	t.Cleanup(func() {
		shared.mu.Lock()
		s.inUse = false
		shared.mu.Unlock()
	})
	return s.name, true
}

func sharedKey(root, service string) string { return root + "\x00" + service }

// watchSchema remembers the definitions (columns, indexes) of the tables the
// test starts with and compares them when it ends. A test that altered one
// (a column changed to make writes fail, say) would leave a schema the next
// test's AutoMigrate trips over, or silently uses; so the shared database is
// dropped then and the next test gets a new one. A table the test dropped is
// fine (the next AutoMigrate creates it again), and so is one it added.
func watchSchema(t testing.TB, admin *sql.DB, root, service, name string) {
	t.Helper()
	before, err := schemaOf(admin, name)
	if err != nil {
		t.Fatalf("testx: read the schema of %s: %v", name, err)
	}
	t.Cleanup(func() {
		after, err := schemaOf(admin, name)
		changed := err != nil
		for table, def := range before {
			if now, ok := after[table]; ok && now != def {
				changed = true
				break
			}
		}
		if !changed {
			return
		}
		// under the lock: no other test of the process may create it again in between
		shared.mu.Lock()
		defer shared.mu.Unlock()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		delete(shared.dbs, sharedKey(root, service))
	})
}

// schemaOf is each table's columns and indexes as one comparable string.
func schemaOf(admin *sql.DB, name string) (map[string]string, error) {
	defs := map[string]*strings.Builder{}
	add := func(table, line string) {
		b := defs[table]
		if b == nil {
			b = &strings.Builder{}
			defs[table] = b
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	rows, err := admin.Query("SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, COLLATION_NAME, EXTRA "+
		"FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, ORDINAL_POSITION", name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, column, typ, nullable, extra string
		var def, collation sql.NullString
		if err := rows.Scan(&table, &column, &typ, &nullable, &def, &collation, &extra); err != nil {
			_ = rows.Close()
			return nil, err
		}
		add(table, fmt.Sprintf("c %s %s %s %q %q %s", column, typ, nullable, def.String+fmt.Sprint(def.Valid), collation.String, extra))
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = admin.Query("SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME "+
		"FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX", name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, index string
		var nonUnique, seq int
		var column sql.NullString
		if err := rows.Scan(&table, &index, &nonUnique, &seq, &column); err != nil {
			_ = rows.Close()
			return nil, err
		}
		add(table, fmt.Sprintf("i %s %d %d %s", index, nonUnique, seq, column.String))
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(defs))
	for table, b := range defs {
		out[table] = b.String()
	}
	return out, nil
}

// hostTag tells the test processes of two machines (or containers) sharing
// one MySQL server apart: 6 hex digits of the host name's hash.
func hostTag() string {
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:3])
}

// sharedPrefix is test_<service>_<host>_p: the process id follows.
func sharedPrefix(service string) string {
	return fmt.Sprintf("test_%s_%s_p", strings.ReplaceAll(service, "-", "_"), hostTag())
}

// dropAbandoned drops the shared databases, of any service, that this host's
// finished test processes left (a process cannot drop its own at exit:
// nothing runs then). Best effort: one whose process may still run is kept.
func dropAbandoned(admin *sql.DB) {
	marker := "_" + hostTag() + "_p"
	rows, err := admin.Query("SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?",
		`test\_%`+strings.ReplaceAll(marker, "_", `\_`)+"%")
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		i := strings.LastIndex(name, marker)
		if i < 0 {
			continue
		}
		pid, err := strconv.Atoi(name[i+len(marker):])
		if err != nil || pid == os.Getpid() || processRunning(pid) {
			continue
		}
		stale = append(stale, name)
	}
	_ = rows.Close()
	for _, name := range stale {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`")
	}
}

func processRunning(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// ESRCH: no such process; anything else (EPERM: someone else's, or a
	// platform without signal 0) counts as running, so nothing live is dropped
	return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}

// emptyDatabase drops what an earlier test may have added besides rows
// (triggers, views, routines, events: a test that makes a write fail with a
// trigger relied on the database being dropped), deletes every row of every
// table of name and sets each AUTO_INCREMENT that moved back to 1, so the
// test sees what a new database would show: ids from 1, no rows.
func emptyDatabase(t testing.TB, admin *sql.DB, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatalf("testx: empty database: %v", err)
	}
	defer func() { _ = conn.Close() }()
	for _, kind := range []struct{ list, drop string }{
		{"SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?", "DROP TRIGGER IF EXISTS"},
		{"SELECT TABLE_NAME FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?", "DROP VIEW IF EXISTS"},
		{"SELECT ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE'", "DROP PROCEDURE IF EXISTS"},
		{"SELECT ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION'", "DROP FUNCTION IF EXISTS"},
		{"SELECT EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?", "DROP EVENT IF EXISTS"},
	} {
		rows, err := conn.QueryContext(ctx, kind.list, name)
		if err != nil {
			t.Fatalf("testx: empty database: %v", err)
		}
		var objects []string
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				_ = rows.Close()
				t.Fatalf("testx: empty database: %v", err)
			}
			objects = append(objects, o)
		}
		_ = rows.Close()
		for _, o := range objects {
			if _, err := conn.ExecContext(ctx, kind.drop+" `"+name+"`.`"+o+"`"); err != nil {
				t.Fatalf("testx: empty database: %s %s: %v", kind.drop, o, err)
			}
		}
	}
	// information_schema caches table statistics (AUTO_INCREMENT among them)
	// for a day by default; 0 reads the live values. Where the variable does
	// not exist every table with an AUTO_INCREMENT is reset.
	live := true
	if _, err := conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0"); err != nil {
		live = false
	}
	rows, err := conn.QueryContext(ctx, "SELECT TABLE_NAME, AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'", name)
	if err != nil {
		t.Fatalf("testx: empty database: %v", err)
	}
	type table struct {
		name  string
		reset bool
	}
	var tables []table
	for rows.Next() {
		var tb string
		var next sql.NullInt64
		if err := rows.Scan(&tb, &next); err != nil {
			_ = rows.Close()
			t.Fatalf("testx: empty database: %v", err)
		}
		tables = append(tables, table{name: tb, reset: next.Valid && (!live || next.Int64 > 1)})
	}
	_ = rows.Close()
	if len(tables) == 0 {
		return
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION foreign_key_checks = 0"); err != nil {
		t.Fatalf("testx: empty database: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "SET SESSION foreign_key_checks = 1") }()
	// One transaction for all the deletes: one commit, one log flush.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("testx: empty database: %v", err)
	}
	for _, tb := range tables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM `"+name+"`.`"+tb.name+"`"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("testx: empty table %s: %v", tb.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("testx: empty database: %v", err)
	}
	for _, tb := range tables {
		if !tb.reset {
			continue
		}
		if _, err := conn.ExecContext(ctx, "ALTER TABLE `"+name+"`.`"+tb.name+"` AUTO_INCREMENT = 1"); err != nil {
			t.Fatalf("testx: reset AUTO_INCREMENT of %s: %v", tb.name, err)
		}
	}
}
