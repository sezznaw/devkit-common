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
| `config`  | Load `conf/<APP_ENV>.yaml` with `${VAR}` expansion (`${VAR:-default}` when unset); `Dir`/`LoadDefault` find the conf directory; `Duration` for `3s`-style values |
| `kitexx`  | Kitex server/client options: Nacos registration and discovery, graceful stop, unified logging with a `trace_id` across services, `OnShutdown`, `Run` |
| `hertzx`  | The same for API (HTTP) services on Hertz: one configuration with `kitexx`, request log with a `trace_id` that travels on to the RPC services, recovery, the same rules for Nacos and the same graceful stop; see below |
| `nacosx`  | Nacos: registration and discovery, configuration that refreshes itself while the program runs, everything logged through zlog; see below |
| `kafkax`  | the event bus (Redpanda): `Publish` wraps the envelope and carries the trace, `Subscribe` handlers are run by the framework once the server is up, failures retried then parked in `<topic>.dlq`; see below |
| `centrifugox` | push (Centrifugo): `Publish` to a channel, `ConnectionToken` signs the client's JWT; see below |
| `s3x`     | object storage (SeaweedFS on dev, any S3 later) from the `s3:` section: `rt.S3.Put/Get/Stat/List/Delete`, presigned download and upload URLs, every file under the service's own prefix; source static or the deployment's datasource row; every call a span and a metric; see below |
| `httpx`   | calls to external HTTP APIs (resty + otelhttp): providers listed under `providers:` with base URL, timeout, retries and headers from the environment; `rt.Provider("odds-feed").GetJSON(ctx, path, &out)`; trace carried on, one log record per call without secrets, metrics; a service with providers must run in the provider namespace; see below |
| `webhookx` | the callbacks a provider sends us (payment results, settlements), on a Hertz service in the provider namespace: `webhookx.Handle(rt, h, "pay", "/callbacks/pay/paid", onPaid)`; source-address check, signature check (hmac-sha256, basic, or the vendor's own Verifier), raw copy to Kafka for disputes, event-id dedupe through Redis, 500 on a handler error so the provider retries, metrics; see below |
| `metricsx` | Prometheus metrics on a port of their own (`metrics.enabled`, default port 9091): requests by method and result code with latency histograms for Kitex and Hertz, calls made to other services, Go runtime, MySQL pool, Redis commands, Kafka events; the deployment scrapes it; see below |
| `jobx`    | scheduled jobs: a service lists them in `app/jobs.go` (name, cron schedule, timeout, function), the framework runs one per `--job=<name>` with a root span, a run id in the log, a lock, a timeout and an exit code, and `--list-jobs` is what the deployment turns into CronJobs; see below |
| `redisx`  | Redis / Valkey from the `redis:` section, the rules of `mysqlx` (enabled, source static or platform), every command a span; the idempotency middleware uses it |
| `mysqlx`  | MySQL (GORM) from the `mysql:` section: pool, every query a span and a record with the trace_id, slow-query warnings; the address from the configuration or from the platform's datasource table; see below |

A minimal service `main`:

```go
var cfg struct{ kitexx.Config `yaml:",inline"` }
if err := config.LoadDefault(&cfg); err != nil { /* print and exit 1 */ }
rt, err := kitexx.NewRuntime(cfg.Config)   // logger, tracing, and what the configuration enables (MySQL, ...)
opts, err := rt.Options()
svr := orderservice.NewServer(handler.New(repo.New(rt)), opts...)   // rt.DB goes to the repo
kitexx.OnShutdown("cache", cache.Close)    // runs after the last request finished
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
- **Tracing (OpenTelemetry).** With `otel.endpoint` set (an OTLP gRPC
  collector, such as Tempo on port 4317) every RPC is a server span, every
  call this service makes is a client span, and the W3C `traceparent` travels
  in the TTHeader to the service called, so Grafana shows the whole chain as a
  waterfall. The `trace_id` in the log then *is* the trace id of the span: Loki
  and Tempo link to each other by it. A caller that sends only a `trace_id`
  and no `traceparent` still lands in the same trace. With `endpoint` empty
  (usually `"${OTEL_EXPORTER_OTLP_ENDPOINT}"` not set) nothing changes: no span
  is recorded and the `trace_id` is made and passed on as before. Spans not
  yet exported are flushed when the server stops.
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
otel:
  endpoint: "${OTEL_EXPORTER_OTLP_ENDPOINT}"   # OTLP gRPC collector, e.g. tempo.monitoring:4317; empty = off
  sample_ratio: 1.0       # share of new traces recorded; a caller's decision is kept
  insecure: true          # no TLS inside the cluster
```

### What `hertzx` gives an API service

An API service (`devkit nas`) is started like an RPC service, and
`hertzx.Config` *is* `kitexx.Config`: the same `conf/*.yaml`, the same rules.

```go
rt, err := kitexx.NewRuntime(cfg.Config)   // logger, tracing, and what the configuration enables
h, err := hertzx.New(rt)           // listener, request log, recovery, the checks on Nacos
app.Setup(&cfg, rt, h)             // the service: RPC clients, middleware
router.GeneratedRegister(h)        // the routes hz generated from the IDL
err = hertzx.Run(h, cfg.Config)    // serve, then stop gracefully
```

- **One record per request**, `http`, with `method`, `path`, `status`, `latency`,
  `client`; 5xx is an error, 4xx a warning. What a handler logs with
  `zlog.Ctx(ctx)` carries the same `trace_id`, `method` and `path`.
- **One `trace_id` from the HTTP request to the last RPC service.** With
  tracing on, every request is a server span (named like `GET /user/:id`,
  method and route); a W3C `traceparent` header makes it a child of the
  caller's span, and an `X-Trace-Id` alone that is 32 hex characters is the id
  of the trace. With tracing off, the `X-Trace-Id` is kept when it looks like
  an id (8 to 64 letters, digits, `-`, `_`; anything else never reaches the
  log), otherwise a new one is made. Either way the final `trace_id` is set on
  the response and is in the context the way `kitexx` puts it there, so a
  client made with `kitexx.ClientOptions` passes it on (with the parent-child
  relation of the spans when tracing is on).
- **A panic in a handler** is one error record with the stack, and a 500.
- **The same stop as an RPC service**: leave Nacos, keep serving for
  `shutdown.deregister_wait`, close the listener, give requests in progress
  `shutdown.drain_timeout`, run the `OnShutdown` hooks. Hertz's own `Spin` does
  these at the same time and registers a second after the start whether or not
  it listens, which is why `Run` replaces it and registers through `nacosx`
  (with `protocol=http` in the metadata) once the port really accepts.
- **A port that is taken is a sentence**, naming the port and `service.addr`;
  Hertz itself panics with a goroutine dump.
- Hertz's own records go through zlog, `logger=hertz`.

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
- **Every service says which services are alive.** At start one record,
  `services alive`, lists every service with its number of instances and their
  addresses. After that, `service online`, `service offline` and `service
  instances changed` say what came or went and, again, everything that is alive,
  so the last of these records always tells how things are:

  ```
  INFO service online name=ser-auth alive=1 services=2 instances=3
      overview:
        ┌──────────┬───┬──────────────────────────────────┐
        │ SERVICE  │ N │ INSTANCES                        │
        ├──────────┼───┼──────────────────────────────────┤
        │ ser-auth │ 1 │ + 127.0.0.1:8889                 │
        │ ser-user │ 2 │   127.0.0.1:8888  ← this process │
        │          │   │   127.0.0.1:8890                 │
        └──────────┴───┴──────────────────────────────────┘
  ```

  `+` came (green), `-` went (red; a service of which nothing is left keeps a row with N = 0),
  `~` is not what it was (yellow). In JSON the same is `overview: {service, changed, alive}`.

  Instances of a known service are pushed by Nacos (under a second); a service
  name that is new is found by asking for the list every 3 seconds. The watch
  has a Nacos client of its own, because the one that calls go through never
  hears that the LAST instance of a service has gone: the SDK keeps the last
  list when Nacos pushes an empty one, to protect callers from a Nacos that lost
  its data. That protection stays; only the records see through it. With the
  watch on, the calling path's own `service discovered` / `instances changed`
  go to debug. `nacos.watch_services: false` turns it off;
  `nc.WatchServices(every)` is the call for programs that are not Kitex services.
- **A laptop never ends up in a Nacos that other people use.** With
  `nacos.register: false` a service finds the others and reads its
  configuration, and is not announced: that is how a developer's machine works
  against the Nacos of the development server. Without it, the local
  environment (no `APP_ENV`) registers in a Nacos on the same machine only;
  anything else is refused at start, with the address and the way out, because
  everybody who uses that Nacos would have requests sent to the laptop.
- **What gets registered is an address callers can reach.** An empty host
  becomes the address this machine reaches Nacos from: the right one on a
  machine with a VPN or Docker networks, and 127.0.0.1 with a Nacos on the same
  machine, which survives a change of the Wi-Fi address. A container that is
  reached through the host and a mapped port sets `service.advertise`
  (`"host"` or `"host:port"`, usually `"${ADVERTISE_ADDR}"`).
- **Without Nacos** (`registry_disabled: true`, for tests and for working
  offline) configurations are files, `conf/nacos/<data id>`, and
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

### MySQL with `mysqlx`

**One framework for every service; whether MySQL is used is configuration.**
Every service's configuration has a `mysql:` section, `enabled: false` by
default. Enabled, `kitexx.NewRuntime` connects and pings *before* the service
registers in Nacos (a service whose database is not there does not start, and
says why) and hands the handle over as `rt.DB` (`*gorm.DB`). The service never
opens a connection; its repo uses the one it is given:

```go
// repo/member.go
func (r *Repo) GetMember(ctx context.Context, uid int64) (*Member, error) {
	var m Member
	err := r.db.WithContext(ctx).Where("uid = ?", uid).First(&m).Error   // WithContext: the query is a span of the request's trace
	return &m, err
}
```

Where the database is, `mysql.source` decides:

```yaml
# conf/dev.yaml, uat, prod: the deployment only says which tenant it serves
# (environment variable TENANT_CODE); the address comes from the platform
# database's datasource table, the platform database's own address from
# infra.yaml in Nacos, and every password from an environment variable
mysql:
  enabled: true
  source: platform
  role: tenant            # tenant: this tenant's database; platform: the platform database itself

# conf/local.yaml: the developer's own database
mysql:
  enabled: true
  source: static
  addr: "${MYSQL_ADDR}"   # empty: 127.0.0.1:3306
  db: tenant_a
  user: tenant_a
  password: "${MYSQL_PASSWORD}"
  max_open: 20            # pool, defaults 20 / 5 / 30m
  max_idle: 5
  conn_max_lifetime: 30m
  slow_query: 200ms       # slower than this is a warn record "mysql slow query"
```

- **Every query is one record**, with the request's `trace_id`: debug when
  fine, warn when slow, error when it failed (a row that is not there is not
  a failure); and one span of the request's trace (the statement, without
  the values).
- **A laptop on a database that is not on it gets a WARN** (source static,
  an address that is not loopback, `APP_ENV` not set), the same kind of
  reminder as "a laptop does not register in the shared Nacos".
- **GORM, with rules**: no `AutoMigrate` (the schema comes from the
  migrations only); no associations and no `Preload` (join explicitly);
  transactions passed explicitly (`repo.WithTx(tx)`), the writes of a money
  path in one explicit transaction; `SkipDefaultTransaction` is on, one
  statement is one statement.
- `rt` belongs to the framework: a service takes from it, never adds to it.

### Redis with `redisx`, and idempotent requests

The `redis:` section works like `mysql:`: `enabled`, `source: static`
(`addr`, `password`, `db`) or `source: platform` (the datasource row of kind
valkey; `db_name` is the database index), opened by the framework at start-up,
the client in `rt.Redis`.

**Idempotent requests**: the API conventions ask money and state-changing
requests to carry a `request_id`. Give the Req struct in the IDL a
`request_id` field and the method is idempotent, by the middleware
`rt.Options()` installs:

- the first call runs and its result is kept under the id for 24 hours
- the same id again gets the kept result back, the handler does not run
  (log record `replayed`)
- the same id while the first call is still running: business code 1002
- an empty `request_id`: 1003
- an error from the handler drops the id, so the caller can retry
- `redis.enabled` false: 5002, the request is refused; running a transfer
  twice is worse than refusing it

Methods without the field are untouched.

### Events with `kafkax`

The `kafka:` section follows the pattern (`enabled`; `source static` with
`brokers`, or `source platform`, the datasource row of kind kafka); the
client is `rt.Kafka`.

**Publish**: `rt.Kafka.Publish(ctx, "events.member", "42", "member.updated",
data)`: topic, key (the entity's id, so its events stay in order), event
type, data. The framework wraps the standard envelope `{id, type, tenant_id,
time, data}`, puts the trace of ctx into the headers and returns once the
cluster has acknowledged.

**Consume**: in `app.Setup`, `rt.Kafka.Subscribe("events.member", func(ctx,
ev *kafkax.Event) error { ... })`; the consumer group is the service's name.
The framework starts the consumers once the server is up (`rt.Run`) and
stops them first on the way out. Every message is a span of the producer's
trace and a record with its trace_id; a handler error is retried 3 times
(`kafka.max_retries`) and then the message goes to `<topic>.dlq` (the error
in a header) and consumption goes on; a panic is treated the same. At least
once: a handler is to be idempotent (deduplicate by `ev.ID`, or be
idempotent by nature).

Event and topic naming is in the idl repository's README.

### Push with `centrifugox`

The `centrifugo:` section: `enabled`; `source static` with `api_addr`,
`api_key`, `token_secret`, or `source platform`, the address from
`infra.yaml` in Nacos and the secrets from the environment variables it
names. The client is `rt.Centrifugo`.

- `rt.Centrifugo.Publish(ctx, "user:42", data)`: to the clients subscribed
  to that channel; a span of the trace
- `rt.Centrifugo.ConnectionToken("42")`: the JWT a logged-in client connects
  with (HS256, `token_ttl` default 24h), returned by the gateway with the
  login; Centrifugo then knows who the connection is, and a service can push
  to `user:<id>`
- `History(ctx, channel, n)`: the last messages of a channel (when the
  namespace keeps history), for tests and reconnecting clients

### Files with `s3x`

```yaml
s3:
  enabled: true
  source: platform      # dev / uat / prod: the datasource row of kind s3 (endpoint, bucket, key variables)
  # source: static      # a laptop: docker run -d -p 8333:8333 chrislusf/seaweedfs server -s3
  # endpoint: "http://127.0.0.1:8333"
  # bucket: sportsbook-dev-assets
  # prefix: ser-user    # default: the service name; every key lives under it
```

```go
err  := rt.S3.Put(ctx, "avatars/42.png", file, "image/png")
data, err := rt.S3.GetBytes(ctx, "kyc/42/front.jpg")
url, err  := rt.S3.PresignGet(ctx, "exports/2026-10.xlsx", 10*time.Minute)   // the client downloads directly
url, err  := rt.S3.PresignPut(ctx, "avatars/42.png", "image/png", 5*time.Minute) // the client uploads directly
objs, err := rt.S3.List(ctx, "avatars", 100)
err  = rt.S3.Delete(ctx, "avatars/42.png")
```

One bucket per deployment, one prefix per service (default the service
name), so services never touch each other's files; keys are paths like
"avatars/42.png". A missing object is `s3x.ErrNotFound`. Large files go
through presigned URLs, not through the service. `rt.S3.Raw()` is the AWS
SDK client for the rest. Every call is a span (otelaws), a debug record and
`s3_requests_total{op, status}` / `s3_request_duration_seconds`. The dev
credentials are the SeaweedFS admin identity in `app-datasources`
(S3_ACCESS_KEY_ID / S3_SECRET_ACCESS_KEY); production gets an identity of
its own with access to that bucket only.

### External APIs with `httpx`

```yaml
providers:
  odds-feed:
    base_url: "https://api.vendor.com"
    timeout: 3s                       # one attempt; default 5s
    retries: 2                        # default 2; GET/HEAD/OPTIONS/PUT/DELETE only, on network errors, 429 and 5xx
    headers:
      X-Api-Key: "${ODDS_FEED_API_KEY}"   # the secret is in the environment, never in the file
```

```go
odds := rt.Provider("odds-feed")        // in app.Setup; a name that is not configured panics there
var out OddsResp
err := odds.GetJSON(ctx, "/v1/odds?match=123", &out)
err  = odds.PostJSON(ctx, "/v1/payout", req, &resp)   // a POST is never retried by the framework
odds.R().SetContext(ctx).SetQueryParam(...)           // anything else: it is a *resty.Client
```

Authentication is configuration too, `auth:` under the provider:

```yaml
    auth: {type: bearer, token: "${ODDS_FEED_TOKEN}"}
    auth: {type: basic, username: "${PAY_USER}", password: "${PAY_PASS}"}
    auth: {type: oauth2, token_url: "https://id.vendor.com/token", client_id: "${PAY_CLIENT_ID}", client_secret: "${PAY_CLIENT_SECRET}"}
    auth: {type: hmac-sha256, secret: "${PAY_HMAC_SECRET}", header: X-Signature, payload: "{method}\n{path}\n{timestamp}\n{body}"}
    auth: {type: custom}        # the vendor's own scheme: a Signer the integration sets
```

oauth2 is the client-credentials flow with the token cached and refreshed.
hmac-sha256 signs a payload template ({method} {path} {query} {timestamp}
{body}) and sends the timestamp in `timestamp_header` (default
X-Timestamp); `encoding` hex or base64. For a scheme none of these fit,
`type: custom` and in app.Setup `rt.Provider("pay").UseSigner(paysig.Sign)`,
where `Sign(req *http.Request, body []byte) error` is a plain function in
the integration's package that sets the vendor's headers; it runs on every
attempt, with the body already read for it. Secrets come from the
environment in every case: the deployment injects the `provider-secrets`
Secret into every service, one variable per secret, named
`<PROVIDER>_<FIELD>` (ODDS_FEED_API_KEY); the integration says which names
it needs, the operator fills them in.

Two guards sit under every call, raw `R()` calls included. `max_concurrent`
(default 64) caps the calls in flight to one provider; the rest wait within
their timeout. A circuit breaker opens after `breaker_failures` consecutive
failed attempts (network error, 429, 5xx; default 5) and for
`breaker_open_for` (default 30s) every call fails at once with
`httpx.ErrCircuitOpen` instead of waiting out its timeouts; then one probe
is let through and the circuit closes on success. State changes are logged
and `http_client_breaker_state` / `http_client_rejected_total` count them.
The connection pool keeps 100 idle connections per host (Go's default of 2
would mean a TLS handshake for most calls to a vendor polled often).

The worst case of one call is (retries + 1) attempts of up to `timeout`
each plus the waits between them, about 16s with the defaults; a handler
that cannot wait that long gives its ctx a deadline. `debug: true` logs
every request and response with headers and bodies, on a developer's
machine only (APP_ENV local); a deployment ignores it with a warning.

Every call carries the trace (a client span `GET odds-feed /v1/odds`, the
W3C traceparent in the request), writes one record with the trace_id
(provider, method, path without the query string, status, latency,
attempt; never headers or bodies), and counts
`http_client_requests_total{provider, http_method, status}` /
`http_client_duration_seconds`. A non-2xx answer is a `*httpx.StatusError`
with the status and the first bytes of the body. Only the business
namespaces are cut off from the internet, so a service with `providers:`
must be deployed to the `-provider` namespace: `NewRuntime` refuses to start
otherwise (it reads `POD_NAMESPACE`) and says where to move it.

### Callbacks with `webhookx`

The other direction: the provider calls us. The rules go under the
provider's `callback:` entry, the handler is a function, and the route is
registered in `app.Setup` of a Hertz service in the provider namespace
(callback paths are the vendor's, so they are not in the IDL):

```yaml
providers:
  payment-x:
    base_url: "https://pay.vendor.com"
    callback:
      verify: {type: hmac-sha256, secret: "${PAYMENT_X_CALLBACK_SECRET}", header: X-Signature, payload: "{timestamp}.{body}", timestamp_header: X-Timestamp, max_skew: 5m}
      allow_cidrs: ["203.0.113.0/24"]          # optional, the vendor's published addresses
      event_id: {json: "data.id"}               # or {header: X-Event-Id}; what makes a redelivery a no-op
      dedupe_ttl: 168h                          # default 7 days
      archive_topic: callbacks.payment-x        # default; "-" switches the raw copy off
```

```go
webhookx.Handle(rt, h, "payment-x", "/callbacks/payment-x/paid", func(ctx context.Context, ev *webhookx.Event) error {
	var n PaidNotice
	if err := ev.Decode(&n); err != nil { return err }
	return wallet.Credit(ctx, n.OrderID, n.Amount)   // idempotent by order id
}, webhookx.WithReply(200, "application/json", `{"code":"SUCCESS"}`))
```

On a Kitex service (the one that integrates vendors), the HTTP listener for
callbacks is the framework's: `callbacks: {enabled: true, addr: 8081}` in
the configuration, `webhookx.Server(rt)` in `app.Setup` returns the engine
(tracing, metrics, request log, recovery already on it), `rt.Run` starts it
right before the RPC server and stops it with it. The deployment exposes
only that port to the internet, under `/callbacks/`.

```go
cb := webhookx.Server(rt)
webhookx.Handle(rt, cb, "payment-x", "/callbacks/payment-x/paid", callbacks.OnPaid)
```

What happens to every callback, in order: source address against
`allow_cidrs` (403), body limit (413, default 1 MiB), signature (401;
`verify.type` hmac-sha256 over a payload template with a timestamp window,
basic, or `custom` with `webhookx.WithVerifier(fn)` for the vendor's own
scheme), the raw request copied to the Kafka archive topic (a failure there
is a 500: the provider retries, we never handle what we could not keep),
the event id checked in Redis (a redelivery is acknowledged without running
the handler), then the handler: nil answers the route's reply (200 "ok"
unless `WithReply`), an error or a panic answers 500 and forgets the event
id, so the provider's retry runs the handler again. One log record per
callback with provider and event_id, and
`callbacks_received_total{provider, http_route, result}`. Handlers stay
idempotent anyway: a provider may send the same notification under two ids.

### Calls between services: timeout, retry, breaker

`ClientOptions` protects every call this service makes to another service,
with defaults a cluster wants and an `rpc_client:` section only to change
them: a 3s timeout per call (`timeout`), 1s to connect (`connect_timeout`),
one retry (`retries`) only when the request was never sent (no instance, no
connection; a timeout or an error from the handler is not retried, because
the call may have run), and a circuit breaker per service and method
(`breaker: false` to switch off): over 50% errors in at least 200 calls and
the calls fail at once with `kerrors.ErrCircuitBreak` until the downstream
recovers. A caller that must retry a write does it itself with the same
request_id; the idempotency middleware replays the result.

### API documentation (`cmd/apidoc`, `hertzx.ServeDocs`)

An API service documents itself from its IDL. `make gen` runs
`go run github.com/sezznaw/devkit-common/cmd/apidoc` on the service's Thrift
file: it adds `api.body` to every bare struct field (the convention is POST +
JSON), runs thriftgo with CloudWeGo's `thrift-gen-http-swagger` plugin, and
writes `cmd/<service>/openapi.yaml`, which `main.go` embeds. With
`docs.enabled` the service serves `/openapi.yaml` and `/docs` (Scalar API Reference,
"Send request" against the service itself) and `/openapi.json`. The response structs carry the
envelope `{code, msg, data}`, so the page shows exactly what a client gets.
Comments on methods and fields in the IDL become the descriptions: the first
sentence of a method's comment is its title, the rest its description. Three
`// @docs` lines at the top of the IDL name the page and its sections:
`// @docs title: Sportsbook 玩家网关`, `// @docs description: ...`,
`// @docs tag member: 会员` (operations are grouped by the second segment of
their path, `/v1/<domain>/...`; `/ping` goes under 系统). Nothing else to
write. On for local and dev, off in production.

### Version

Every record and span carries `version`: `log.version` from the configuration, else `APP_VERSION` (the deployment sets it to the image tag), else the VCS revision compiled into the binary (`-dirty` when the tree was modified at build time, which CI's `go mod tidy` can cause; the environment variable avoids that).

### Metrics with `metricsx`

```yaml
metrics:
  enabled: true     # dev / uat / prod; off on a laptop (two services would fight over the port)
  addr: 9091        # the default; the service chart scrapes this port
```

Nothing to write: with `metrics.enabled` the framework serves `/metrics`
(and `/healthz`) on that port, apart from the service's own port so that it
is never reachable through the ingress, and records

- `rpc_server_requests_total{rpc_service, rpc_method, code}` and
  `rpc_server_duration_seconds`: every RPC handled; `code` is `ok`, the
  business code of a `BizStatusError` (bounded by the table in the idl
  repository), or `error`
- `rpc_client_requests_total` / `rpc_client_duration_seconds`: the calls
  this service makes, by the service called
- `http_server_requests_total{http_method, http_route, status}` /
  `http_server_duration_seconds` (Hertz): the route is the IDL pattern,
  never the raw path; a request that matched no route is `unmatched`
- `go_sql_*` (the MySQL pool, by database), `redis_commands_total{cmd,
  status}` / `redis_command_duration_seconds`, `kafka_events_published_total`,
  `kafka_events_handled_total{topic, result}` (`ok`, `retried`, `dlq`),
  `kafka_records_produced_total`, `kafka_records_fetched_total`,
  `kafka_broker_connects_total`, and the Go runtime and process collectors

A metric of the service's own: `promauto.With(metricsx.Registry).NewCounter(...)`
at package level; the default Prometheus registry is not served. A port that
is taken is a warning at start, not a failure.

### Scheduled jobs with `jobx`

A job is a function with a name and a cron schedule, listed in the service's
`app/jobs.go` (the only file to touch; the schedule lives next to the code):

```go
func Jobs(rt *kitexx.Runtime) []jobx.Job {
	r := repo.New(rt)
	return []jobx.Job{{
		Name:     "reconcile-bets",
		Schedule: "*/5 * * * *",          // five fields, UTC
		Timeout:  10 * time.Minute,
		Run:      func(ctx context.Context) error { return reconcile.Run(ctx, r) },
	}}
}
```

The generated `main.go` does the rest: `<binary> --list-jobs` prints the list
as JSON (`{"jobs":[{name, schedule, timeout_seconds}]}`) without reading any
configuration, which is what the CI stores next to the image tag and the
deployment turns into one Kubernetes CronJob per job; `<binary> --job=<name>`
is what that CronJob runs: the framework opens what the configuration enables
(the database, Redis, ...), runs the job and exits, without starting the
server, registering in Nacos or starting consumers. The exit code is 1 on
failure, which is how Kubernetes counts a failed run.

What a run gets: a root span `job <name>` (so a slow job is a waterfall in
Grafana, and the queries and calls inside are its children), a logger with
`job`, `run_id` and `trace_id` on every record (`zlog.Ctx(ctx)`), one record
at the end with the result and the duration, a lock `job:<service>:<name>` in
Redis so two runs of one job never overlap (without Redis the run proceeds with
a warning; the CronJob's `concurrencyPolicy: Forbid` still covers the scheduled
runs), a context cancelled at the timeout, and a panic turned into a failure.
Every start validates the list (name as a Kubernetes name, unique, schedule
parseable), so a typo is a start failure, not a surprise at 3 am.

Rules for the function: idempotent and resumable (a run can be retried), work
in chunks and commit each, check `ctx.Err()` between chunks, and never trust
"last run time": read the watermark from the data.

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
  or `env: local (APP_ENV is not set)`; in JSON these remarks are in
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
