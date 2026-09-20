# common

**English** | [简体中文](README.zh-CN.md)

Shared Go library for all backend services. Import it as a normal module:

```sh
go env -w GOPRIVATE=github.com/sezznaw      # only if the repository is private
go get github.com/sezznaw/devkit-common@latest
```

| Package   | Purpose |
|-----------|---------|
| `log`     | Company logger on `log/slog`: `New`, `Named`, `WithContext` / `FromContext` |
| `config`  | Load `conf/<APP_ENV>.yaml` with `${VAR}` expansion; `Dir`/`LoadDefault` find the conf directory; `Duration` for `3s`-style values |
| `kitexx`  | Kitex server/client options: Nacos registration and discovery, graceful stop, unified logging, `OnShutdown`, `Run` |
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
- **One log format.** Kitex's internal logs go through the same `slog` logger
  as yours (`logger=kitex`), and a handler panic is one record with the stack
  in a `stack` attribute, so JSON log collection keeps working.
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
- Request logging middleware with a request-scoped logger in the context, and
  handler panics turned into errors (the latter by Kitex itself).

```yaml
shutdown:
  deregister_wait: 3s     # 0s disables the wait
  drain_timeout: 15s
```

Versioning: semantic tags `vX.Y.Z`. Breaking changes bump the minor version
while we are on 0.x; services pin the version in their `go.mod`.

```sh
go test ./...
```
