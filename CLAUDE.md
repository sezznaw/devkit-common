# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The shared Go library imported by every service that `devkit ngs` generates.
Three names to keep straight:

- Go module path: `github.com/sezznaw/devkit-common`
- GitHub repository: `sezznaw/devkit-common` (public)
- Local directory: `common` (sibling of `../devkit` and `../devkit-registry`)

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
    `TestCallerOnEveryPath` guards it. The console prints the caller relative
    to the working directory for files inside it, absolute otherwise
    (`clickablePath`), which GoLand's run window turns into a link (confirmed
    by the user) and which is never ambiguous; JSON uses zap's short form.
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
  and expands `${VAR}` from the environment *before* YAML parsing.
- `kitexx` shutdown design, which is easy to get wrong: Kitex's `Stop()` runs
  its own shutdown hooks, then deregisters, then closes the listener and
  drains, and `svr.Run()` returns only after all that. Therefore (1)
  `delayedRegistry` wraps the Nacos registry and sleeps *after* the real
  `Deregister`, because callers still hold the instance in their cache;
  (2) `OnShutdown` hooks are ours and run after `svr.Run()` returns, never via
  `server.RegisterShutdownHook`, which fires while requests are still served;
  (3) `shutdown.deregister_wait` is a `*config.Duration` so an explicit `0s`
  differs from "unset" (default 3s).
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
- `kitexx`: the glue a service's `main` uses. `Options(cfg)` returns Kitex
  server options (basic info, listen address, logging middleware, meta
  handler, Nacos registry unless `registry_disabled`) and installs the logger,
  `Nacos(cfg)` returns the client of the process, `WatchConfig[T](cfg)` the
  configuration that refreshes itself (data id `config_data_id`, default
  `<service.name>.yaml`),
  `ClientOptions(cfg)` returns TTHeader transport, caller name and the Nacos
  resolver (no resolver with `registry_disabled`), `Run` starts the server and
  syncs the logger on exit. `kitexx.Config` is meant to be embedded
  inline in the service's own config struct; the `kitex-service` template in
  `../devkit-registry` depends on its field names and YAML keys.
- `redisx`, `etcdx`: thin constructors from YAML-loadable config structs;
  `redisx.New` pings before returning.

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
