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
| `nacosx`  | Nacos: registration and discovery, configuration that refreshes itself while the program runs, everything logged through zlog; see below |
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
- **One Nacos client per process**, shared by registration, every downstream
  client and the configuration (`kitexx.Nacos(cfg)`, `nacosx.Shared`).
- **Configuration that refreshes itself**: `kitexx.WatchConfig[T](cfg)`, see
  `nacosx` below.
- Request logging middleware (method, caller, latency, error), and handler
  panics turned into errors (the latter by Kitex itself).

```yaml
shutdown:
  deregister_wait: 3s     # 0s disables the wait
  drain_timeout: 15s
```

### Nacos with `nacosx`

```go
type Dynamic struct {                       // only what may change while the service runs
    Battle struct {
        MaxLevel int `yaml:"max_level"`
    } `yaml:"battle"`
}

dyn, err := kitexx.WatchConfig[Dynamic](cfg.Config)  // data id: config_data_id, default "<service.name>.yaml"
...
if level > dyn.Get().Battle.MaxLevel {               // always the version in effect; one atomic load
```

Outside Kitex the same is `nc, err := nacosx.New(cfg.Nacos)` and
`nacosx.Watch[Dynamic](nc, "gateway.yaml")`.

- **A change in the Nacos console is in effect a moment later**, without a
  restart. Every change is a new `T`; what `Get` returned before is never
  written to, so there is nothing to lock. `dyn.OnChange(func(old, cur *Dynamic))`
  is for what has to be rebuilt rather than read (a rate limiter, a pool).
- **A bad change cannot take the service down.** Content that does not parse,
  or that `Validate() error` (optional, on `*T`) refuses, is rejected with an
  ERROR record and the version before it stays in effect; so does the last
  version when the configuration is deleted. At start the same problems stop
  the service instead: it should not start on half a configuration, nor on a
  snapshot of unknown age when Nacos does not answer.
- **The log says what changed**, key by key, with the values of keys that look
  like secrets (`password`, `secret`, `token`, `*_key`, ...) hidden, and points
  out keys the program has no field for, which is what a typo looks like:

  ```
  INFO  config changed data_id=order.yaml group=DEFAULT_GROUP md5=c50a4f9a→542f35bb
      changes:
        + battle.double_exp  true
        ~ battle.max_level   60 → 80
        - legacy_flag        false
        ~ pay.secret         *** → ***
  WARN  config has keys the program does not know; a typo? data_id=order.yaml keys="max_levle (line 2)"
  ERROR config change rejected; the previous one stays in effect data_id=order.yaml err="not valid: battle.max_level must be positive"
  ```

  In JSON `changes` is an array of `{op, key, from, to}`.
- **Registration and discovery** for programs that are not Kitex services:
  `deregister, err := nc.Register(nacosx.Registration{Service: "gateway", Addr: ":8080"})`,
  `nc.Instances("order")`, `nc.Pick("order")` (weighted random),
  `nc.Subscribe("order", fn)`. Kitex services get both from `kitexx.Options`
  and `kitexx.ClientOptions`. Either way the log shows `registered in Nacos`,
  `service discovered` and `instances changed` with a line per instance
  (`+ 10.0.0.7:8888  weight=10`, `- 10.0.0.6:8888`).
- **An outage of Nacos is three records, not three hundred.** `nacos
  connection lost` with the SDK's first complaint as `cause`, `nacos is still
  unreachable` every 30 s, and `nacos connection restored down_for=48.7s
  sdk_records_hidden=194`. Meanwhile the service keeps the instance lists and
  configurations it has, and afterwards the SDK registers and subscribes again
  by itself.
- **The Nacos SDK logs through zlog** (`logger=nacos-sdk`) instead of into
  `nacos-sdk.log`, from `nacos.sdk_log_level` up (default `warn`; at `info`
  the SDK prints the content of every configuration, passwords included).
- **Without Nacos** (`registry_disabled: true`, the usual setting on a
  developer's machine) configurations are files, `conf/nacos/<data id>`, and
  saving the file is a change like one in the console. Nothing is registered
  and no service is looked up: `client.WithHostPorts` says where one is.
- Lower level: `nc.Get(dataID)`, `nc.OnChange(dataID, func(content string))`,
  `nacosx.Group("other")` for a configuration in another group,
  `nacosx.Static(&Dynamic{...})` for tests, `nacosx.NewFrom` to put fake SDK
  clients underneath. `go run ./examples/nacosx` shows all of the above.

```yaml
nacos:
  addrs: ["nacos:8848"]
  namespace: ""            # namespace id; one per environment
  group: DEFAULT_GROUP     # of services and of configurations
  sdk_log_level: warn      # debug | info | warn | error
config_data_id: order.yaml # kitexx.WatchConfig; default "<service.name>.yaml"
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
  settings are an object, `"config": {...}`. What `kitexx` fills in says where
  it is from: `service: order (from service.name)`, `env: prod (from APP_ENV)`,
  or `env: dev (APP_ENV is not set)`; in JSON these remarks are in
  `"config_notes": {...}`.
- **Before `Init`** (a service that cannot read its configuration) the logger
  is configured by `ZLOG_FORMAT`, `ZLOG_LEVEL`, `ZLOG_SERVICE`, `ZLOG_ENV`;
  deployments set `ZLOG_FORMAT=json`.
- **Sampling says what it left out.** With `sampling` on, every second in
  which records were dropped ends with one record per level and message,
  `zlog: records dropped by sampling dropped_msg="redis down" dropped=4213`, at
  the level of what was dropped, so the size of a flood can be read from the
  log. `zlog.Sync()` writes the count of the current second at once.
- `zlog.Enabled(zlog.LevelDebug)` guards records that are expensive to build;
  `zlog.SetLevel("debug")` changes the level at run time; `zlog.Sync()` before
  exit; package `zlog/zlogtest` captures or silences logs in tests.
- A value that implements `zlog.Blocker` (`LogBlock(colored bool) string`) and
  is passed to `zlog.Any` is lines below the record on the console and the
  value itself in JSON; `nacosx` uses it for `changes` and `instances`.

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
