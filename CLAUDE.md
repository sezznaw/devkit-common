# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The shared Go library imported by every service that `devkit ngs` generates.
Three names to keep straight:

- Go module path: `github.com/sezznaw/devkit-common`
- GitHub repository: `sezznaw/devkit-common` (public)
- Local directory: `devkit-common` (sibling of `../devkit` and `../devkit-registry`; renamed from `common` on 2026-10-08)

It is a runtime dependency of production services, which is why it is its own
module rather than part of the devkit CLI or the template registry.

## Commands

```sh
go test ./...
go test ./kitexx/ -run TestLoggingMiddleware    # single test
go vet ./... && test -z "$(gofmt -l .)"         # what CI runs
```

## Packages

- `zlog`: the company logger on zap. Design decisions that are easy to undo
  by accident:
  - *Two kinds of `*Logger`.* `zlog.With`, `Default()` and `Ctx(ctx)` follow
    the default, resolving the logger installed by `Init` each time they log
    (`Logger.core`, cached per installed default), because a package-level
    `var log = zlog.With(...)` runs before `main` calls `Init` and would
    otherwise stay on the start-up defaults. Loggers from `New`/`Init` and
    their children own their configuration.
  - *Fields are typed*, `zlog.Info("login", zlog.Int("uid", uid), zlog.Err(err))`.
    The alternating `"key", value` form was dropped on purpose: nothing checks
    it before run time. `Str`, `Int`, `Float` are generic (Thrift mixes int32
    and int64). `Field` is our own struct around `zap.Field`, not an alias,
    because it has methods (`.Blue()`).
  - *Caller.* The number of zlog frames between the caller and zap is fixed,
    and `z2` skips them: the logging function plus `core.log`/`core.logf`.
    `TestCallerOnEveryPath` guards it. The console prints the caller short
    (`callerPath`): relative to the working directory for files inside it,
    which GoLand's run window turns into a link (confirmed by the user); from
    the project directory for a sibling of the working directory
    (`common/nacosx/services.go`, `ser-user/handler/handler.go`); from the
    module on for the module cache (`github.com/cloudwego/kitex@v0.16.3/...`);
    absolute for the rest. The owner asked for the short forms on 2026-09-21
    because absolute paths made every framework record a very wide line.
    Until then everything outside the working directory was absolute, for the
    sake of the link; the owner confirmed on 2026-09-21 that GoLand's run window links the
    new forms too (with the project directory open). JSON uses zap's short form.
  - *No key twice in a record.* zap does not deduplicate; parsers keep the
    last duplicate and Elasticsearch rejects the record. So (1) a field named
    like a key of the record becomes `fields.<key>` (`fieldKey`, logrus's
    convention; `TestEveryRecordKeyIsProtected` and `TestIdentityFields` fail
    when a new record key is not added there), and (2) of two fields with the
    same key the later one wins, which is why the fields of `With` are kept in
    `core.added` and written with every record instead of being handed to
    zap's `With`, where they could no longer be replaced. Fields passed to the
    raw `Zap()`/`L()` logger bypass both.
  - *Two formats, same content*, except `identityFields` (`service`, `env`,
    `host`, `version`), which are JSON-only; that function is the single place
    for JSON-only content. The console encoder is our own (`console.go`):
    `key=value`, dimmed keys, values of several lines and stacks as a block
    below the record so that every frame is a link.
  - *Colors* are on unless `NO_COLOR` is set and deliberately do not test for a
    TTY: the GoLand run window is not one. GoLand's *test* window prints the
    escapes as text; judge the console format with `go run ./examples/zlog`.
    A colored field travels as `zap.Inline(coloredField)`; colored printf
    arguments are a `fmt.Formatter` (`color.go`), taken off in `core.logf` when
    the output is not a colored console. `logf` must not assign to `args`: go
    vet stops treating a function as a printf wrapper when it does.
  - *`Blocker`*: a value passed to `Any` that implements it is a block below
    the record on the console (`AddReflected`) and itself in JSON. `clipField`
    leaves it as it is instead of pre-encoding it to `json.RawMessage`, or the
    console could no longer ask it for its lines. It must not be a Stringer or
    an error: `zap.Any` would write those as a string.
  - *Cost.* `core.log` checks the level before converting fields, so a call
    below the level allocates nothing; otherwise cost equals raw zap
    (benchmarks in the test file).
  - `Init` also redirects `log/slog` (and with it the std `log`) through
    `slogHandler`, which resolves the default logger per record and passes
    `testing/slogtest`. Before `Init` the logger is configured by `ZLOG_*`
    environment variables (`envOptions`): a service that cannot read its
    config logs before `Init`, and that line must be JSON in production.
  - `Init` (not `New`) announces the settings in effect as its first record,
    "logger configured" (`describe`, `core.announce`): a block below the record
    on the console with `(default)` behind what was not set, an object
    `"config": {...}` in JSON so that its keys cannot clash with the record's.
    It and the typo warnings of `New` go to the zap core directly
    (`core.write`), past the level: at `level: error` a warning about a typo
    would otherwise hide itself. Tests that count records after `Init` reset
    their buffer first. `Options.ServiceNote`/`EnvNote` (not from YAML) let the
    code that fills in those two values say where they are from; `kitexx` does
    ("from service.name", "from APP_ENV", "APP_ENV is not set"), because zlog
    can only tell "default" for what it defaults itself. In JSON the remarks
    are `config_notes`, placed before the `config` namespace, which swallows
    every field after it.
  - Sampling reports what it drops (`sampling.go`, zap's `SamplerHook`): per
    second, level and message one record "zlog: records dropped by sampling"
    at the level of the dropped records, written to the core *behind* the
    sampler so that it is never sampled itself, with the identity fields. No
    goroutine while nothing is dropped: the first drop of a window arms a
    one-shot timer. `sync` and `stop` flush the pending count and stop that
    timer, which is also why a test that samples must `Sync` before it reads
    its buffer (the timer would write into it later, a data race). At most
    `maxDropKeys` messages are tracked per window, the rest counts as "(other
    messages)".
  - `Sync` drops EINVAL/ENOTTY/EBADF: stdout as a terminal or pipe cannot be
    synced. `Options.JSON` is the deprecated key of the old `log` package,
    still read so that an upgraded service does not silently turn to console
    output. `zlog/zlogtest` captures or silences the default logger in tests.
  - Deliberately not done: JSON keys stay in zap's order (`level` before
    `time`; changing it means writing a JSON encoder of our own, and collectors
    do not care), and a message of several lines stays as it is on the console
    (JSON is one line regardless). There is no limit on the size of a whole
    record, only per value (`max_field_bytes`).
- `config`: `Load(dir, &cfg)` reads `<dir>/<APP_ENV>.yaml` (default `local`: a developer's machine; the
  deployed environments are `dev`, `uat` and `prod`)
  and expands `${VAR}` from the environment *before* YAML parsing; `${VAR:-default}`
  (v0.23.0, `Expand`) takes the default when unset or empty, so conf/local.yaml
  carries the local stack's values (nacos/nacos, tenant_a, ...) and `make run`
  needs no exports. The owner hit "using password: NO" on 2026-10-07 because
  MYSQL_PASSWORD was not exported; this is the fix.
- `kitexx` shutdown design, which is easy to get wrong: Kitex's `Stop()` runs
  its own shutdown hooks, then deregisters, then closes the listener and
  drains, and `svr.Run()` returns only after all that. Therefore (1)
  `delayedRegistry` wraps the Nacos registry and sleeps *after* the real
  `Deregister`, because callers still hold the instance in their cache;
  (2) `OnShutdown` hooks are ours and run after `svr.Run()` returns, never via
  `server.RegisterShutdownHook`, which fires while requests are still served;
  (3) `shutdown.deregister_wait` is a `*config.Duration` so an explicit `0s`
  differs from "unset" (default 3s).
- `nacosx/services.go`: `WatchServices`, the owner's requirement that every service logs who came,
  who went and everything that is alive (name, number of instances). It needs a naming client of
  its own with `UpdateCacheWhenEmpty`: measured against Nacos 2.4.3, the default client is never
  pushed the departure of a service's last instance and `SelectAllInstances` keeps returning it.
  Do not "simplify" it onto the shared client, and do not turn the protection off for the calling
  path. Nacos keeps a service name in its list after the last instance is gone, so the overview
  counts instances, not names. New names are polled (`serviceListInterval`, 3s), instances pushed.
- `nacosx`: one `Client` per process (`Shared`, reached from services through
  `kitexx.Nacos`) for registration, discovery and configuration. What it is
  built around are facts of nacos-sdk-go v2.3.5 that are easy to forget:
  - *`Probe` runs in `New` before the SDK client is created*: the SDK does not
    fail on an unreachable server, it retries in the background and the first
    call fails with "client not connected, current status:STARTING". Nacos 2.x
    needs the main port and main port + 1000 (gRPC); both are dialled, any one
    reachable server is enough. `kitexx.Nacos` adds the `registry_disabled:
    true` hint, because only it knows that setting.
  - *SDK log* (`sdklog.go`): `logger.SetLogger` before the first SDK client
    exists is kept, because the SDK's `InitLogger` returns when a logger is
    there. One logger per process, so the level of the first client counts. Default threshold warn: at
    info the SDK prints the content of every configuration it receives.
    `sdkFrames` works like `klogFrames`; `TestSDKLogGoesThroughZlog` guards it.
  - *Configuration listeners* (`config.go`, type `watch`): the SDK runs a
    listener as `go listener(...)`, with no recover, no order between two quick
    changes, and once right after `ListenConfig` when the snapshot file differs.
    So there is one SDK listener per configuration, ours; it serializes,
    deduplicates by md5, recovers, and fetches the content from the server
    instead of trusting the pushed one (of two pushes that overtook each other
    the older would win). A deleted configuration arrives as "".
  - *`DisableUseSnapShot(true)`*: otherwise `GetConfig` silently answers from
    the cache directory when the server does not. Decided with the user: no
    Nacos at start means no start.
  - *`Value[T]`* (`value.go`): every change decodes into a new `T` and swaps an
    `atomic.Pointer`; never decode into the `T` in effect (data race, and a
    failed decode would leave half of it). Strict decode first, lenient second:
    unknown keys are a WARN naming them, not a rejection. The record of a
    change is written before the hooks run. Lock order is `Value.mu` then
    `watch.mu` in `Watch`, the reverse in a change; that is safe only because
    the Value is not a subscriber before `follow` returns.
  - *Diff and masking* (`diff.go`): the two documents are flattened to
    `a.b.c` and compared as strings, lists as one value. Keys that look like
    secrets are masked in "config loaded" and in changes (`isSecret`); a list
    that contains such a key is masked as a whole. `Changes`, `settings` and
    `InstanceChanges` are `zlog.Blocker`s: lines on the console, values in JSON.
  - *Naming* (`naming.go`): `Subscribe` of the SDK calls the callback from
    inside the call, on the same goroutine, hence `service.subMu` apart from
    `service.mu`. The SDK does not process a pushed empty instance list
    (`updateCacheWhenEmpty` false, its protection against a Nacos that lost its
    data), so the last instance of a service never "leaves" for callers;
    `observe` therefore ignores an empty look-up. Instances are registered in
    cluster "DEFAULT" because kitex-contrib/registry-nacos did, and services on
    common <= v0.3 still do; look-ups do not filter by cluster.
  - *Connection state* can only be polled (`ServerHealthy`): listeners exist
    on the SDK's internal rpc client only. After a reconnect the SDK redoes
    registrations and subscriptions itself (seen with a restarted server).
  - *Outage* (`outage.go`, process-wide like the SDK's logger): measured
    against a stopped server the SDK wrote 119 warn/error records in 25 s. Now
    the first record that matches `lostSigns` starts the outage at once (the
    poll would be up to 2 s late), "nacos connection lost" carries it as
    `cause`, everything the SDK says at warn or above until the clients report
    healthy again is counted instead of written (`sdk_records_hidden`), with a
    reminder every 30 s. `sdk_log_level: debug` switches all of that off.
    `routine` in `sdklog.go` is the other filter: warn records the SDK writes
    with every read of a configuration (no failover file, no encrypted-data-key
    file, "config data not exist"), taken down to debug. Both lists are
    substrings of SDK messages: check them when the SDK is upgraded, against a
    real server (`docker stop nacos` while `examples/nacosx` runs).
  - *One instance per service and client*: Nacos 2.x ties an ephemeral instance
    to the gRPC connection, a second `Register` for the same service replaces
    the first (the integration test first got this wrong).
  - Fields are `name=`, not `service=`: `service` is a key of the record and
    would come out as `fields.service`.
  - *Offline client* (`Offline`, what `kitexx.Nacos` returns with
    `registry_disabled`): configurations are files `<conf dir>/nacos/<data id>`,
    polled once a second, through the same `watch` as the real thing.
  - Tests put fakes underneath (`fake_test.go`; `NewFrom` for other packages).
    `TestAgainstNacos` needs `NACOS_ADDR` and runs in the CI job `nacos`
    against a real server. Since 2026-09-21 the user's Mac has Docker Desktop
    and a container `nacos` (nacos-server v2.4.3, standalone, 8848 + 9848):
    `docker start nacos`, then `NACOS_ADDR=127.0.0.1:8848 go test ./nacosx/`.
- `kitexx/nacos.go`: registry and resolver of our own on top of `nacosx`
  (kitex-contrib/registry-nacos is no longer a dependency; it logged nothing).
  Resolve returns an error for an empty list, or Kitex would cache it.
- `kitexx/klog.go` routes Kitex's klog through zlog, using the raw zap logger
  with `AddCallerSkip(klogFrames)` so that `caller` is the line of Kitex that
  logged, and so that the panic stack keeps the key `stack` (a zlog field of
  that name would become `fields.stack`). A multi-line message is split into
  message + `stack` so it stays one record.
- `kitexx` trace_id: `LoggingMiddleware` reads the persistent metainfo value
  `TRACE_ID` or creates one, puts it back into the context and into the logger
  of the request (`zlog.Ctx(ctx)`). It only crosses the wire because
  `ClientOptions` selects the TTHeader transport and `Options` adds the server
  meta handler; verified with two real Kitex services, not only unit tests.
  `log_level_data_id` makes the level follow a Nacos configuration
  (`loglevel.go`, through `Client.Get` and `Client.OnChange`); failing to set that up is a warning, not a start failure.
- `hertzx`: kitexx for API services on Hertz (`devkit nas`, template `hertz-service`).
  `hertzx.Config` is an alias of `kitexx.Config` on purpose (one set of conf files and docs, and
  `kitexx.ClientOptions(cfg)` for the RPC services behind the API). It reuses, not copies,
  `kitexx.Bootstrap`, `ListenAddr`, `Registration`, `DeregisterWait`, `DrainTimeout`,
  `RunShutdownHooks`, `NewTraceID`, `TraceIDKey`. `Run` replaces Hertz's `Spin` because Spin runs
  the shutdown hooks, the deregistration and the transport shutdown concurrently and its registry
  registers one second after start regardless of the listener (read in Hertz v0.10.6); Hertz also
  panics when the port is taken, so `run` probes the port first and recovers. The trace_id enters
  as `X-Trace-Id` (validated by `looksLikeID`) and is stored as the same metainfo persistent value
  kitexx reads, which is what makes it reach the RPC services. `hlog.go` is `kitexx/klog.go` for
  the other logging package. Verified on a real Nacos: HTTP -> API -> RPC with one trace_id, the
  stop order, registration with `protocol=http`.
- `kitexx`: the glue a service's `main` uses. `Options(cfg)` returns Kitex
  server options (basic info, listen address, logging middleware, meta
  handler, Nacos registry unless `registry_disabled`) and installs the logger.
  It refuses, before anything starts, a developer's machine that would
  register in a Nacos that is not on it (`nacosx.Config.CheckRegistration`:
  `config.IsLocal()` and a Nacos address that is not loopback; the owner's
  rule, since a laptop in the shared Nacos receives everybody's requests);
  `nacos.register: false` is the way to work against such a Nacos, and the
  planned "dev takeover" relies on it. `service.advertise` is registered
  instead of the listener's address (containers behind a mapped port).
  `Nacos(cfg)` returns the client of the process, `WatchConfig[T](cfg)` the
  configuration that refreshes itself (data id `config_data_id`, default
  `<service.name>.yaml`),
  `ClientOptions(cfg)` returns TTHeader transport, caller name and the Nacos
  resolver (no resolver with `registry_disabled`), `Run` starts the server and
  syncs the logger on exit. `kitexx.Config` is meant to be embedded
  inline in the service's own config struct; the `kitex-service` template in
  `../devkit-registry` depends on its field names and YAML keys.
- `otelx`: OpenTelemetry for the process, deliberately on the official SDK and
  not on kitex-contrib/hertz-contrib `obs-opentelemetry` (built against Kitex
  0.11 / Hertz 0.9, and they drag metrics along). `Init` installs the W3C
  propagator always and a real provider only when `otel.endpoint` is set
  (empty = off: no-op provider, nothing recorded, old trace_id behaviour);
  `kitexx.Bootstrap` calls it and registers the flush as an `OnShutdown` hook.
  The resource is built `NewSchemaless`: merging a schema'd resource with
  `resource.Default()` fails on a schema version mismatch, and the semconv
  package has to be the one of the SDK version (`v1.43.0` for otel 1.47; it
  no longer has `RPCSystemKey`/`RPCService`, hence the literal `rpc.system`
  and `rpc.service` keys in `kitexx/tracing.go`). `WithRemoteTraceID` is how
  a caller that sends only a 32-hex id (X-Trace-Id, TRACE_ID) ends up in the
  same trace: a remote parent with that trace id, a span id made from its
  second half, sampled.
- `kitexx/tracing.go`, `hertzx/tracing.go`: the spans. Server span first in
  the middleware chain (`Options` adds `ServerTracing` before
  `LoggingMiddleware`; `hertzx.New` uses `Tracing(), RequestLog(),
  Recovery()`), so that `ensureTraceID` / `RequestLog` log the span's trace id
  and the clients a handler calls make child spans. The propagator reads and
  writes *transient* metainfo values through `metainfoCarrier` (the TTHeader
  carries them one hop, the life of a traceparent; `Set` must replace the
  context, hence the pointer), and HTTP headers through `headerCarrier`. The
  trace_id precedence is: span's trace id, else TRACE_ID / X-Trace-Id, else
  new; and whatever wins is written back as TRACE_ID so older services still
  get it. `tracing_test.go` in both packages runs against an in-memory span
  recorder (`tracetest.SpanRecorder`); `recordSpans` restores the no-op
  provider, because the provider is process-global.
- `mysqlx` and `kitexx.Runtime` (`runtime.go`): the owner's rule is that the
  framework code of every service is the same and configuration decides what
  is used, and that a colleague never writes "get the database": `NewRuntime`
  (Bootstrap + whatever `mysql.enabled` asks for, before Nacos registration,
  so a service without its database never takes traffic) fills `Runtime{Config,
  DB}`, the generated `main.go` passes it to `app.Setup`, the generated
  `repo.New(rt)` stores `rt.DB`, and handlers call repo methods. `Options(cfg)`
  stays as `NewRuntime` + `rt.Options()`; `hertzx.New` takes the Runtime
  (breaking, hence v0.9.0). `mysql.source platform` reads the platform
  database's address from `infra.yaml` in Nacos (`mysql.platform.{addr, db,
  user, password_env}`), its password from that variable, then the tenant's row
  (`TENANT_CODE`) from `platform.datasource` (`mysqlx.Resolve`, plain
  database/sql); every password is an environment variable named by data, never
  in data. GORM: `SkipDefaultTransaction`, `NowFunc` UTC, the DSN pins
  parseTime/loc=UTC/time_zone/utf8mb4 and timeouts, `zlogger` bridges GORM's
  logger to `zlog.Ctx(ctx)` (debug / slow warn / error, `ErrRecordNotFound` is
  not an error), `gorm.io/plugin/opentelemetry` makes each statement a span
  (`WithoutMetrics`, `WithoutQueryVariables`). `gorm.Open` dials by itself, so a
  database that is down fails at "connect to", not at the ping; both carry the
  same `hint`. `TestAgainstMySQL` needs `MYSQL_TEST_DSN_ROOT`
  (`root:root@tcp(127.0.0.1:13306)/`; `docker run -d --name mysqlx-test -e
  MYSQL_ROOT_PASSWORD=root -p 13306:3306 mysql:8.4`) and runs in CI job
  `mysql`. `Runtime` must not grow service-specific fields; the next
  infrastructure (Redis, Kafka) gets the same shape: a config section with
  `enabled`, opened in `NewRuntime`, a field on `Runtime`.
- `redisx`: the `mysqlx` shape for Redis / Valkey (`Config` with enabled /
  source / role, `Target`, `Open` with `redisotel` tracing, `New` for programs
  that are not services). `source platform` reuses `mysqlx.ResolveKind` with
  kind `valkey` (`db_name` is the database index, `TargetFromRow`); the
  platform database lookup is `platformDatabase` in `kitexx/runtime.go`, shared
  with MySQL. Tests run on miniredis (`RunT`, `RequireAuth`).
- `kitexx/idempotency.go`: the owner's API rule "money and state-changing
  requests carry a request_id" made automatic. A method is idempotent when its
  Req has a `request_id` field (Kitex getter `GetRequestId`); the middleware
  unwraps the `*Args` (`GetFirstArgument`), claims `idem:<service>:<method>:<id>`
  with SETNX (value `pending`, 24h), keeps the JSON of the `*Result`'s
  `Success` after a successful call, and on a repeat decodes it into a new
  value of the Success field's type by reflection (`setSuccess`). Fail closed:
  no Redis or Redis down is 5002, never a second execution; a handler error
  deletes the key. Codes 1002 / 1003 / 5002 are in the idl repository's
  errors.md. It is installed by `rt.Options()` unconditionally (count it in
  `TestOptionsWithoutRegistry`); tests use the generated shapes as hand-written
  structs on miniredis.
- `starrocksx`: database/sql on the go-sql-driver connector (StarRocks is
  MySQL-protocol), `prepare` rewrites `:tenant` to `?` inserting the tenant at
  the right argument position (string literals skipped) and refuses non-SELECT
  or tenant-less statements; `scanRows` maps columns to struct fields by `db`
  tag / snake_case (error on an unmapped column) or scalars; MaxRows cap.
  Runtime.Report via `openReport` (infra.yaml `starrocks` block for source
  platform; static assumes tenant_a when TENANT_CODE is empty). Tests run
  against MySQL (MYSQL_TEST_DSN_ROOT).
- `kafkax` dedupe: `SetDedupe(rdb)` (runtime: when rt.Redis and
  kafka.dedupe.enabled != false; testx too). In `handle`, after the envelope
  decodes: SETNX `dedupe:<svc>:<topic>:<id>` EX ttl; dup -> result
  "duplicate", return (offset committed). On dlq park or ctx cancel the key is
  DEL'd so a redelivery / replay runs. Redis error -> handle anyway (warn).
- `kafkax.DLQ` (the `--dlq` tool, `kitexx.DLQFlag` / `RunDLQ`, run after
  app.Setup so `c.subs` names the dlq topics): kadm on the producer client
  for ListEndOffsets / FetchOffsets (GroupIDNotFound = never committed) /
  CommitOffsets of the tool group `<service>-dlq`; pending = committed..end,
  read with a group-less kgo client via ConsumePartitions; replay strips the
  park headers (error, source-topic, consumer) and adds replayed-from. Lag
  gauge `kafka_consumer_lag{topic}` is HighWatermark - last offset - 1 per
  poll (max over partitions). Test TestDLQTool on kfake.
- `kitexx.Limits` (first middleware in Options): in-flight counter (atomic)
  and a fixed 100ms-window QPS counter, refusing through `BizError(ctx,
  NewBizStatusError(CodeBusy 5003))`; `connectionLimit` adds Kitex's
  `WithLimit{MaxConnections}` + a reporter for the metric. Probed: Kitex's
  QPS limiter closes the connection ("EOF peer close" at the client, not an
  ErrOverlimit), which is why QPS is ours. Metric rpc_server_rejected_total.
- `idx`: 53-bit snowflake: 41 time bits from Epoch 2026-01-01 above 12 low
  bits split by `id.instance_bits` (default 5 + 7; `Instance(id, bits)` to
  decode). 53 so the id is exact as a JSON / JavaScript number: no string form
  at the gateway (the user asked for this over 64 + string after weighing the
  trade-offs; capacity is 32 replicas x 128/ms per service by default). `Next`
  under a mutex, waits out a backwards clock and a full sequence. Instance
  lease in Redis (`idx:<svc>:<n>`, SET NX scan from 0, TTL 90s, renew 30s with
  a token-matching script; a lost lease is re-taken or a new number leased).
  No Redis: INSTANCE_ID; else only `Options{Local}` (Log.Env local/empty)
  allows the fnv(hostname) fallback, a deployment gets a generator whose Next
  panics with ErrNoInstance. Runtime.ID is made in NewRuntime after Redis;
  testx makes one too.
- `testx`: builds `kitexx.Runtime` by hand (exported fields) on miniredis,
  kfake, an httptest Centrifugo API (`/api/<method>`, records publish), and
  for `WithMySQL(dir)` a `test_<svc>_<n>` database on MYSQL_TEST_DSN_ROOT
  (go-sql-driver ParseDSN; multiStatements for the `*.up.sql` files; dropped
  in Cleanup; t.Skip when the server does not answer) opened with
  mysqlx.Open, plus `delayx.New` (poll 50ms). `Start` = Kafka.Start +
  StartOutbox + Delay.Start (if handlers); `RunDue` = Delay.RunOnce
  (exported for this). Gateway helpers wrap ut.PerformRequest. Its MySQL
  test runs in CI's mysql job like the outbox's.
- `hertzx.Guard` (installed by New after the request log, before
  RequireLogin): `kitexx.GuardConfig` (`guard:`), `Rate` parses "N/s|m|h" /
  "off" (YAML unmarshal). Fixed-window counters: Redis INCR+PEXPIRE pipeline
  on `rl:<svc>:<scope>:<window>` when rt.Redis is set, else an in-memory map
  (purged when > 10000). Per-route limits come from x-rate-limit in the
  embedded spec (apidoc `@limit` -> `Limits`, generic `setExtension`), read
  lazily on the first request (Spec is set after New). Timeout: ctx with
  deadline into c.Next; afterwards DeadlineExceeded + empty body -> 504/1010.
  Body size is `server.WithMaxRequestBodySize` in New. Codes 1008 / 1010 in
  kitexx/guard.go. Tests use ut.PerformRequest with X-Forwarded-For.
- `redisx` lock and cache: `Open` takes an optional service name kept as the
  connection's ClientName; `Namespace(rdb)` reads it back and `keyOf` builds
  `lock:<svc>:<name>` / `cache:<svc>:<name>:<id>`. `WithLock`: SET NX PX with a
  random token, a keeper goroutine extends every ttl/3 with a compare-and-
  pexpire script and cancels fn's ctx (ErrLockLost) when the key is gone,
  release is compare-and-del; `Wait` retries every 50ms. `Cache[T]`: GET /
  JSON, per-process singleflight (sync.Map of calls), SET with ±10% jitter,
  `Negative` stores a tombstone ("\x00nf") for the not-found error, Redis
  errors fall back to the loader (warned once a minute), nil client = loader
  only. Tests on miniredis (it supports CLIENT SETNAME).
- `delayx`: run-once-at-a-time tasks in the service's MySQL (`delayed_task`,
  `delayx.DDL`), the same shape as the outbox: `Schedule` inside the business
  transaction, a poller per process (`Start` from the runtime's OnStart, only
  when `Handlers() > 0`, so a service that schedules nothing never polls a
  table it does not have) claims due rows with `FOR UPDATE SKIP LOCKED`, marks
  them `running`, runs the handler with `delay.timeout`, then `done` /
  `pending` with backoff (1m << n, cap 1h) / `failed` after `max_attempts`. A
  `running` row older than timeout + 1m (the process died) is claimed again:
  at least once, handlers must be idempotent. `Cancel` flips pending rows of
  kind + key. Gauges `delay_tasks_pending/failed/overdue_seconds`, counter
  `delay_tasks_run_total{kind,result}`. The post-deploy `delay` check uses
  `Overdue` and `FailedCount`. Integration test `TestScheduler` needs
  `MYSQL_TEST_DSN_ROOT` (CI's mysql job runs it).
- `kafkax`: franz-go (Redpanda's recommendation), tracing through
  `kotel` (`kgo.WithHooks`; the produce span's parent is `Record.Context`, the
  consumer side `tracer.WithProcessSpan(r)` extracts the parent from the
  headers, which is what keeps producer and consumer in one trace). One
  producer client, one consumer client per subscribed topic, group = service
  name, `DisableAutoCommit` + `CommitUncommittedOffsets` after each poll's
  records are handled, `BlockRebalanceOnPoll` and therefore `defer
  cl.AllowRebalance()` in the poll loop, or `Close` hangs (found the hard
  way). The envelope is `Event`; a handler error is retried `max_retries`
  times (backoff attempt*200ms) and the record then copied to `<topic>.dlq`
  with `error` / `source-topic` / `consumer` headers; a panic is an error. The
  datasource row for kind kafka: host (comma-separated hosts allowed), port,
  username, password_env; `mysqlx.ResolveKind` now treats an empty
  password_env as "no password" (dev Redpanda has no SASL). Tests run on
  `kfake` (in-process cluster, `SeedTopics`).
- `centrifugox`: Centrifugo's HTTP API (`POST /api/<method>`, `X-API-Key`),
  three methods used (`info` as the Open check, `publish`, `history`),
  errors in the `error` object of the response; `ConnectionToken` is an HS256
  JWT with `sub` = user id (golang-jwt v5). `source platform` reads
  `centrifugo.{api_addr, api_key_env, token_hmac_env}` from infra.yaml
  (`infraDocument` in `kitexx/runtime.go`, shared with the platform
  database). Tests use an httptest fake of the API.
- `Runtime` now: `Config, DB, Redis, Kafka, Centrifugo`, opened in that
  order in `NewRuntime`; `rt.Run(svr)` starts the Kafka consumers after the
  server is up and `Run(svr, cfg)` is the no-consumer form the old template
  used. The kitex-service template calls `rt.Run`.
- `jobx` and `kitexx/jobs.go`: scheduled work, the owner's rule applied
  again (one framework, colleagues write only the function; the schedule is
  code, not infra). `Job{Name, Schedule, Timeout, Description, Run}`;
  `Validate` (DNS-1123 name, unique, five-field cron via robfig/cron's
  parser, has Run) runs at every start (`rt.SetJobs`) and before the list.
  `Execute` makes a root span (`trace.WithNewRoot`: a CronJob has no caller),
  a logger with job/run_id/trace_id, takes the lock `job:<service>:<name>`
  with SETNX (TTL = timeout + 1 min; `Locker` is the two methods of
  `*redis.Client` so tests use miniredis and nil means "no Redis", a warn and
  run), `context.WithTimeout`, recovers a panic, returns the error = exit 1.
  `kitexx.JobFlags` reads `--list-jobs` / `--job=<name>` from os.Args before
  the configuration is loaded, so the CI can list the jobs of a freshly built
  binary with no Nacos; `ListJobs` prints `{"jobs":[...]}` (JSON is YAML, the
  deployment reads it as a values file); `rt.RunJob` runs one and then the
  shutdown hooks and `nacosx.CloseShared`, never `Options`/`Run` (no
  registration, no consumers). The templates' `app/jobs.go` is the once file
  with `Jobs(rt)`, called with an empty Runtime for the list, so it must only
  register, never connect.
- `metricsx`: Prometheus on client_golang, one process registry
  (`metricsx.Registry`, Go + process collectors; never the default
  registry), served by `Serve` on `metrics.addr` (default 9091, `Serve` opens
  the listener so a taken port is its error; `Runtime.serveMetrics` turns
  that into a WARN and goes on). Middlewares: `kitexx.ServerMetrics` /
  `ClientMetrics` (after the tracing middleware in `Options` /
  `ClientOptions`; `resultCode` is ok / biz code / error, never an error
  string, label cardinality), `hertzx.Metrics` (route = `c.FullPath()` or
  `unmatched`). `mysqlx` pool via `collectors.NewDBStatsCollector` in
  `openMySQL` (`metricsx.Register` tolerates a duplicate), `redisx`
  `metricsHook` (go-redis Hook, added in `Open`), `kafkax` `metricsHook`
  (own franz-go hook; kprom was tried and dropped: it registers its vectors
  in `OnNewClient`, so a second client in one process panics with a
  duplicate registration) plus event counters in `Publish` / `handle`.
  Tests use `prometheus/testutil`. The option counts in
  `TestOptionsWithoutRegistry` / `TestClientOptions` include the metrics
  middlewares.
- `httpx`: resty v2 (v3 was still a release candidate on 2026-10-07; the
  owner chose resty over imroc/req: standard transport, larger user base)
  with `otelhttp.NewTransport` for the client span and the traceparent
  (which needs the global propagator, `otelx.Init` installs it; the test
  sets `propagation.TraceContext{}` itself). Retries: our own
  `retryCondition` only (network error / 429 / 5xx, idempotent methods
  only; resty's defaults would retry a POST on a network error). JSON is
  decoded by us, not `SetResult`, because resty only decodes a JSON
  content type and vendors forget it. Log and metrics come from
  `OnAfterResponse` + `OnError` (the final attempt; `Attempt` tells), path
  without query (tokens live there), never headers or bodies. `Config.Providers
  map[string]httpx.Provider`, built in `Runtime.openProviders`, which also
  enforces the deployment rule: `POD_NAMESPACE` set and not ending in
  `-provider` with providers configured is a start failure (the chart sets
  POD_NAMESPACE; a laptop has none). `rt.Provider(name)` panics on an unknown
  name: it is called in `app.Setup`, so that is a start failure too. `guard.go` is a RoundTripper between resty and otelhttp (so retries and
  raw R() calls pass through it): a semaphore for `max_concurrent` (waits on
  the request ctx, which resty's timeout bounds) and sony/gobreaker v2
  (consecutive failures, MaxRequests 1 in half-open). A 5xx/429 response is
  made a failure for the breaker by wrapping it in `failedStatus` and
  unwrapped again on the way out, so the caller still sees the status;
  `ErrCircuitOpen` is excluded from retries. `newTransport` sets
  MaxIdleConnsPerHost 100 (Go default 2) and 5s dial/TLS timeouts. `debug`
  is honoured only when `config.IsLocal()`. `auth.go` is another RoundTripper, first in the chain (resty → auth →
  guard → otelhttp → pool) so every attempt is signed afresh: bearer, basic,
  oauth2 (x/oauth2 clientcredentials + ReuseTokenSource; the token endpoint
  uses the pooled transport through `oauth2.HTTPClient` in the ctx),
  hmac-sha256 over a payload template, or custom (`Signer`, set with
  `UseSigner`, stored in an atomic.Pointer; before it is set every call is
  `ErrNoSigner`). `readBody` reads the body for signing and restores Body
  and GetBody so the request can still be sent and retried. Owner's model
  (decided 2026-10-07): one tenant = one complete deployment, so vendor
  credentials are the deployment's environment (Secret `provider-secrets`),
  no per-tenant lookup; `TENANT_CODE` and the "platform" database were kept
  with their comments re-characterised as this deployment's registry.
- `webhookx` (inbound callbacks) and `httpx.Callback` (its config under
  the provider): `Handle(rt, h, provider, path, handler, opts...)` registers
  POST and PUT on the Hertz engine; order in `serve` is cidr → size →
  verify → event id → Kafka archive → Redis SETNX → handler, each step a
  distinct `result` label and status. Archive failure and Redis failure are
  500 on purpose (the provider retries; never handle what was not kept),
  duplicate is the route's reply without the handler, handler error deletes
  the SETNX key so the retry runs. hmac verify tolerates a `scheme=` prefix
  on the header and compares case-insensitively in constant time; `{timestamp}`
  in the payload enables the skew window. Panics in Handle (unknown
  provider, no callback entry, custom without WithVerifier) are start-time
  on purpose. Tests: hertz `ut`, miniredis, kfake (`archive_test.go`). `webhookx.Server(rt)` (server.go) is the listener for a Kitex service:
  one Hertz engine per Runtime (sync.Map keyed by *Runtime), the hertzx
  middlewares, started through `rt.OnStart` (new: `Runtime.starters`, run
  by `rt.Run` before Kafka and the RPC server; `RunStarters` for tests) and
  stopped by an OnShutdown hook; the port is probed first because Hertz
  panics on a taken port. `callbacks.enabled` false makes `Server` panic on
  purpose. The owner's decision (2026-10-07): one vendor-integration
  service, `ser-vendor`, Kitex for internal RPC plus this listener for
  callbacks, in the provider namespace; no separate proxy service (the
  namespace policy is the egress, the Ingress is the entry). The ingress of
  that service must not carry the office-only middleware.
- `s3x`: aws-sdk-go-v2 s3 with `otelaws.AppendMiddlewares`, BaseEndpoint +
  UsePathStyle (SeaweedFS), static credentials. `Open` does HeadBucket (the
  bucket must exist; the chart's createBuckets makes them). `Client.full`
  prefixes every key with the service's prefix (default service name); all
  methods go through `do` (timeout, metric `s3_requests_total{op,status}`
  with status ok / not_found / error, debug or error record). `Get` uses
  `context.WithoutCancel` for the body because the caller reads it after
  `do` returns. `mapErr` turns NotFound / NoSuchKey / HTTP 404 into
  `ErrNotFound`. `source platform` reuses `mysqlx.ResolveKind` with kind
  "s3": the row's password_env holds the ACCESS KEY's variable (so
  `row.Password` is the access key) and params carry
  `secret_env=...&path_style=...&insecure=...&region=...` (`TargetFromRow`);
  the seed row of dev is shaped that way. Tests use gofakes3 (in-memory
  S3, including presigned URLs).
- `kitexx/rpcclient.go`: the protections of `ClientOptions` (owner asked
  2026-10-07 why third-party calls had them and internal calls did not):
  `WithRPCTimeout` 3s, `WithConnectTimeout` 1s, `WithFailureRetry` with
  `retryOnlyUnsent` (ErrGetConnection / ErrNoConnection / ErrNoDestAddress /
  ErrNoDestService only: never a timeout or a handler error, the call may
  have run), one `circuitbreak.CBSuite` per process keyed by
  `RPCInfo2Key` (service + method), closed by an OnShutdown hook. Section
  `rpc_client:` (`RPCClientConfig`) changes them. `TestClientOptions` counts
  these options (8 without a registry, 9 with the caller's name).
- `cmd/apidoc` + `hertzx/docs.go`: OpenAPI from the IDL. The CloudWeGo plugin
  `thrift-gen-http-swagger` documents only annotated fields and emits
  `responses: {}` for a response struct without `api.body` on its fields, so
  `Annotate` adds `api.body="<name>"` to every bare field of a temp copy
  (`fieldLine` regex: "N: [optional|required] type name" with no "(" on the
  line) before running thriftgo; `Patch` edits info via yaml.Node to keep
  key order, then `Localize` (docs.go): tag = second path segment mapped
  through `// @docs tag <key>: <名>` directives (else the key; /ping → 系统),
  summary = first sentence of the plugin's description (`splitSummary` on
  newline / 。 / ". " / ：), "Successful response" → Chinese, top-level
  `tags` list in directive order. The owner wants the docs in Chinese like
  the Casual Casino page; the UI (Scalar) itself stays English. `ServeDocs(h, cfg, spec)`: /openapi.yaml, /docs → redirect,
  /docs is a page loading the embedded Scalar bundle (`hertzx/docs/scalar.js`,
  4.4 MB, version in docs/README.md; the owner wanted the same look as the
  Casual Casino docs, which use Scalar), /openapi.json is the yaml converted;
  only with `docs.enabled` (`kitexx.Config.Docs`). hertz-contrib/swagger
  was used first and dropped. The hertz-service template
  (0.15.0) installs the plugin in `make tools`, runs apidoc in `make gen`
  into cmd/<svc>/openapi.yaml (gitignored, embedded with go:embed by
  main.go) and calls ServeDocs after hertzx.New.
- `authx` (v0.25.0; owner's design decisions 2026-10-08: a dedicated
  ser-auth, authentication for every realm, authorization elsewhere; members
  single-session, no guest login; access 15m, refresh 30d): HS256 JWT with
  claims sub/typ(realm)/sid/jti; Redis keys `auth:sess:<realm>:<uid>` (current
  sid), `auth:sessions:<realm>:<uid>` (set), `auth:sid:<sid>` (alive marker,
  what Verify and Refresh check), `auth:rt:<sha256(token)>` (refresh record),
  `auth:rtused:<hash>` (retired, for replay detection), `auth:bl:<jti>`
  (logout before expiry). Issuer (ser-auth) and Verifier (gateways) share
  the package; nobody else touches the keys. `kitexx/identity.go` carries
  UID/REALM/SID as persistent metainfo (like TRACE_ID); `UID(ctx)` is how
  handlers get the member, `MustUID` panics to catch a wrongly @public
  endpoint. `hertzx.RequireLogin` reads the embedded OpenAPI: apidoc's
  `Secure` gives every operation `security: [{bearerAuth: []}]` except the
  methods whose comment has `// @public` (`security: []`), so the IDL is the
  single source of what is public; the docs page gets a bearer scheme and a
  script that remembers `data.access_token` from responses and attaches it.
- `etcdx` was removed in v0.20.0: nothing in the stack runs etcd.

- `kitexx.VersionEnv` (`APP_VERSION`, v0.24.0): `Bootstrap` fills `cfg.Log.Version`
  from it when the configuration leaves it empty; the chart sets it to the
  image tag. Found by the acceptance-test agent on 2026-10-07: the VCS
  revision in CI builds came out `-dirty`.

## Versioning rules

- Tags are plain semver (`v0.1.0`). Once a tag is pushed, proxy.golang.org
  caches that version forever: never move or re-push a tag, publish the next
  version instead. `v0.1.0` is already cached.
- While on 0.x, a breaking change bumps the minor version.
- The `kitex-service` template pins `CommonVersion` and `KitexVersion`. After
  tagging a release here, bump those defaults in
  `../devkit-registry/components/kitex-service/component.json` and publish a
  new template version, otherwise new services keep getting the old common.
- The Kitex version in `go.mod` and the template's `KitexVersion` (which also
  selects the `kitex` code generator version) must match.

## Gotchas

- `go mod tidy` needs `google.golang.org/genproto` at a recent version;
  nacos-sdk-go pulls an old monolithic genproto that collides with the split
  modules ("ambiguous import"). If that error comes back after a dependency
  bump, `go get google.golang.org/genproto@latest` fixes it.
- Docs come in pairs: `README.md` and `README.zh-CN.md`, same structure.
