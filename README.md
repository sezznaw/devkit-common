# common

**English** | [简体中文](README.zh-CN.md)

Shared Go library for all backend services. Import it as a normal module:

```sh
go env -w GOPRIVATE=github.com/sezznaw      # only if the repository is private
go get github.com/sezznaw/devkit-common@latest
```

| Package   | Purpose |
|-----------|---------|
| `zlog`    | Company logger on zap: typed fields checked by the compiler, a console format for people (colors, clickable `file:line`) and JSON for log collectors, request-scoped loggers with a `trace_id`; see below |
| `config`  | Load `conf/<APP_ENV>.yaml` with `${VAR}` expansion; `Dir`/`LoadDefault` find the conf directory; `Duration` for `3s`-style values |
| `kitexx`  | Kitex server/client options: Nacos registration and discovery, graceful stop, unified logging with a `trace_id` across services, `OnShutdown`, `Run` |
| `nacosx`  | Nacos naming / config clients from a YAML config block |
| `redisx`  | go-redis client with connection check |
| `etcdx`   | etcd v3 client |

A minimal service `main`:

```go
var cfg struct{ kitexx.Config `yaml:",inline"` }
if err := config.LoadDefault(&cfg); err != nil { /* print and exit 1 */ }
opts, err := kitexx.Options(cfg.Config)
svr := orderservice.NewServer(handler.New(), opts...)
kitexx.OnShutdown("db", db.Close)      // runs after the last request finished
kitexx.Run(svr, cfg.Config)
```

### What `kitexx` gives a service

- **Graceful stop.** On SIGINT/SIGTERM/SIGHUP the service leaves Nacos, keeps
  serving for `shutdown.deregister_wait` (default 3s) so callers can refresh
  their instance lists, closes the listener, gives in-flight requests up to
  `shutdown.drain_timeout` (default 15s), then runs the `OnShutdown` hooks in
  reverse order. Keep the two values together below the platform's kill
  timeout (Kubernetes: 30s by default).
- **One log format.** Kitex's internal logs go through zlog like yours
  (`logger=kitex`, with the line of Kitex that logged as `caller`), and a
  handler panic is one record with the stack in a `stack` field, so JSON log
  collection keeps working.
- **A `trace_id` that follows the request.** The server middleware takes the
  `trace_id` the caller sent or makes one, logs it on every record of the
  request (`zlog.Ctx(ctx)`), and `ClientOptions` (TTHeader transport) passes it
  on to the services this one calls, so a collector shows the whole chain
  under one id. The request log also says who called (`from`).
- **Log level at run time.** With `log_level_data_id` set, the level follows a
  Nacos configuration whose content is `debug`, `info`, `warn` or `error`.
- **Config that does not depend on the start directory.** `config.LoadDefault`
  looks at `$CONF_DIR`, `./conf`, then next to the executable, and reports
  missing files in plain words instead of panicking.
- **A clear message when Nacos cannot be reached.** Before creating the Nacos
  client the service checks the configured servers on both ports a Nacos 2.x
  client needs (the main port and main port + 1000 for gRPC) and stops with
  the address, the reason and the setting to look at, instead of the SDK's
  `client not connected, current status:STARTING`.
- **One Nacos connection per process**, shared by registration and every
  downstream client (`nacosx.SharedNamingClient`).
- Request logging middleware (method, caller, latency, error), and handler
  panics turned into errors (the latter by Kitex itself).

```yaml
shutdown:
  deregister_wait: 3s     # 0s disables the wait
  drain_timeout: 15s
```

### Logging with `zlog`

```go
zlog.Info("player login", zlog.Int("uid", uid), zlog.Str("ip", ip))
zlog.Error("save failed", zlog.Int("uid", uid), zlog.Err(err))
zlog.Infof("player %d login from %s", uid, zlog.Blue(ip))   // printf style
zlog.Ctx(ctx).Info("saved")                                  // carries trace_id and method of the request

var log = zlog.With(zlog.Str("component", "repo"))           // fine at package level: follows Init
```

- **Fields are typed and checked by the compiler**: `Str`, `Int`, `Float`
  (generic: any integer or float type), `Bool`, `Dur`, `Time`, `Err` (always
  under `err`), `Any`, `Secret` (written as `***`). A missing value or a key
  that is not a string does not compile; `go vet` checks `Infof` calls.
- **Console format for people** (`format: console`): a color per level,
  `file:line` that GoLand's Run window and terminals turn into a link (relative
  to the working directory, absolute for code outside it), `key=value` fields,
  multi-line values and stacks below the record with every frame a link. A
  field or a printf argument can be given a color: `zlog.Int("age", 15).Blue()`,
  `zlog.Blue(uid)` (`Red Green Yellow Blue Purple Cyan Gray`; `err` is red).
  GoLand's *test* window prints the color codes as text; use Run, or
  `go run ./examples/zlog`.
- **JSON format for collectors** (`format: json`): one object per line, RFC 3339
  time with zone offset, short `caller`, and `service`, `env`, `host`, `version`
  on every record (these identity fields are the only content the console
  leaves out). A record never has a key twice: a field named like a key of the
  record (`level`, `time`, `msg`, ...) becomes `fields.level`, and of two
  fields with the same key the later one is kept.
- **One stream.** `Init` also routes the standard library's `log` and
  `log/slog` through zlog, and `kitexx` does the same for Kitex's klog, so what
  dependencies print is in the same format.
- **The start of a service shows how it logs.** The first record of `Init`,
  `logger configured`, lists the settings in effect under the names they have
  in the configuration file: a default where nothing was written (marked
  `(default)` on the console), the fallback where it was a typo. It is written
  whatever the level, and so are the warnings about such typos; in JSON the
  settings are an object, `"config": {...}`.
- **Before `Init`** (a service that cannot read its configuration) the logger
  is configured by `ZLOG_FORMAT`, `ZLOG_LEVEL`, `ZLOG_SERVICE`, `ZLOG_ENV`;
  deployments set `ZLOG_FORMAT=json`.
- `zlog.Enabled(zlog.LevelDebug)` guards records that are expensive to build;
  `zlog.SetLevel("debug")` changes the level at run time; `zlog.Sync()` before
  exit; package `zlog/zlogtest` captures or silences logs in tests.

```yaml
log:
  level: info            # debug | info | warn | error
  format: json           # console | json
  stacktrace: error      # attach the call stack from this level up; omit for none
  max_field_bytes: 8192  # cut longer messages and values (containers split lines > 16 KB)
  sampling: {first: 100, thereafter: 100}       # flood protection, per second and message
  buffer: {size: 262144, flush_interval: 1s}    # buffered output, optional
log_level_data_id: order.log-level              # kitexx: the level follows this Nacos configuration
```

Versioning: semantic tags `vX.Y.Z`. Breaking changes bump the minor version
while we are on 0.x; services pin the version in their `go.mod`.

```sh
go test ./...
```
