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
    their buffer first.
  - `Sync` drops EINVAL/ENOTTY/EBADF: stdout as a terminal or pipe cannot be
    synced. `Options.JSON` is the deprecated key of the old `log` package,
    still read so that an upgraded service does not silently turn to console
    output. `zlog/zlogtest` captures or silences the default logger in tests.
  - `zlog/PLAN.zh-CN.md` is the review checklist of 2026-09-20 with the state
    of every item.
- `config`: `Load(dir, &cfg)` reads `<dir>/<APP_ENV>.yaml` (default `dev`)
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
- `nacosx.Probe` runs inside `SharedNamingClient` before the SDK client is
  created: nacos-sdk-go does not fail on an unreachable server, it retries in
  the background and the first call fails with "client not connected, current
  status:STARTING". Nacos 2.x needs the main port and main port + 1000 (gRPC);
  both are dialled, any one reachable server is enough. `kitexx.Options` adds
  the `registry_disabled: true` hint, because only it knows that setting.
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
  (`loglevel.go`); failing to set that up is a warning, not a start failure.
- `kitexx`: the glue a service's `main` uses. `Options(cfg)` returns Kitex
  server options (basic info, listen address, logging middleware, meta
  handler, Nacos registry unless `registry_disabled`) and installs the logger,
  `ClientOptions(cfg)` returns TTHeader transport, caller name and the Nacos
  resolver (no resolver with `registry_disabled`), `Run` starts the server and
  syncs the logger on exit. `kitexx.Config` is meant to be embedded
  inline in the service's own config struct; the `kitex-service` template in
  `../devkit-registry` depends on its field names and YAML keys.
- `nacosx`, `redisx`, `etcdx`: thin constructors from YAML-loadable config
  structs; `redisx.New` pings before returning.

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
