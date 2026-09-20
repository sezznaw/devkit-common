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

- `zlog`: the logger on zap that replaces `log` (work in progress: `kitexx`
  still uses `log`). `Init(Options)` installs the default used by the
  package-level functions; `New` only builds. Fields are typed, one bracket
  per pair: `zlog.Info("login", zlog.Int("uid", uid), zlog.Err(err))`. The
  alternating `"key", value, ...` form was dropped on purpose: nothing checks
  it before run time, and keeping it next to the typed form would keep it in
  use. Eight constructors (`field.go`): `Str`, `Int`, `Float` are generic so
  that the caller need not know int32 from int64 (Thrift mixes them); `Err`
  fixes the key `err`. `Field` is our own struct around `zap.Field`, not an
  alias, because it has methods: `zlog.Int("age", 15).Blue()` colors that one
  field on the console (`Red Green Yellow Blue Purple Cyan Gray`). zap gives
  an encoder nothing but key and value, so a colored field travels as
  `zap.Inline(coloredField)`, which sets `consoleEncoder.fieldColor` around
  the field; `core.zap` does that only for a colored console, so JSON and
  `NO_COLOR` output is identical with or without colors. Arguments of `Infof`
  and friends take the same colors as `zlog.Blue(uid)` (`color.go`, a
  `fmt.Formatter` that applies the verb to the wrapped value); `core.logf`
  formats the message itself so that it can take the colors off first when the
  output is not a colored console. `logf` must not assign to `args`: go vet
  stops treating a function as a printf wrapper when it does, and `Infof`
  calls would no longer be checked. A field named like a
  key every record has (`level`, `time`, `msg`, `caller`, `service`) is written
  as `fields.level` etc. in both formats (`fieldKey`, logrus's convention): zap
  does not deduplicate, parsers keep the last duplicate so the record loses its
  level, and Elasticsearch rejects the record. `TestEveryRecordKeyIsProtected`
  fails when a new record key is not added to `fieldKey`. Fields passed to the
  raw `Zap()`/`L()` logger bypass this. `core.log` checks the
  level before converting the fields, so a call below the level allocates
  nothing (raw zap allocates the field slice); otherwise cost equals raw zap
  (benchmarks in the test file).
  Two kinds of `*Logger`: `zlog.With(...)` and `Default()` *follow the
  default*, resolving the logger installed by `Init` each time they log
  (`Logger.core`, cached per installed default), because a package-level
  `var log = zlog.With(zlog.Str("component", "repo"))` runs before `main` calls `Init`
  and would otherwise stay on the start-up defaults: info level, no `service`,
  console text inside a JSON stream. Loggers from `New`/`Init` and their
  children own their configuration and ignore later `Init` calls.
  The number of zlog frames between the caller and zap is fixed per path, and
  `wrap` sets the skip to match: two behind every logging function (the
  function plus `core.log` or `core.logf`). Keep that invariant, the
  reported file:line relies on it (`TestCallerOnEveryPath` guards it). The console
  prints the caller so that GoLand's run window makes it a link and it is never
  ambiguous (`clickablePath`): relative to the working directory for files
  inside it, as compiler errors are, absolute otherwise. Not
  `handler/handler.go` alone: two services of one project both have that file.
  JSON uses zap's short form, independent of build machine and flags. The
  console format is our own `consoleEncoder` (`console.go`), because zap's
  prints the fields as a JSON object at the end of the line; ours prints
  `key=value` in the order added, keys dimmed. The two formats carry the same
  content with one exception, kept in one place, `identityFields`: what
  identifies the process (`service`) is in the JSON records only. To see what a
  collector gets, `go run ./examples/zlog -format json`. Console colors
  are on unless `NO_COLOR` is set and deliberately do not test for a TTY: the
  GoLand run window is not one. To judge the console format by eye, Run
  `examples/zlog` (`go run ./examples/zlog`); GoLand's test runner window
  prints the color escapes as text (`[32mINFO[0m`) instead of rendering them.
- `log`: slog wrapper. `New(Options)` also installs the slog default;
  `WithContext`/`FromContext` carry a request-scoped logger; `Options` is
  YAML-loadable (its `Output` field is not).
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
- `kitexx/klog.go` routes Kitex's klog through slog; a multi-line message is
  split into message + `stack` attribute so it stays one record.
- `kitexx`: the glue a service's `main` uses. `Options(cfg)` returns Kitex
  server options (basic info, listen address, logging middleware, Nacos
  registry unless `registry_disabled`), `ClientOptions(cfg)` returns the Nacos
  resolver, `Run` starts the server. `kitexx.Config` is meant to be embedded
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
