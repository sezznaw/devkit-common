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
| `config`  | Load `conf/<APP_ENV>.yaml` with `${VAR}` expansion |
| `kitexx`  | Kitex server/client options: Nacos registration and discovery, logging middleware, `Run` |
| `nacosx`  | Nacos naming / config clients from a YAML config block |
| `redisx`  | go-redis client with connection check |
| `etcdx`   | etcd v3 client |

A minimal service `main`:

```go
var cfg struct{ kitexx.Config `yaml:",inline"` }
config.MustLoad("conf", &cfg)
opts, _ := kitexx.Options(cfg.Config)
svr := orderservice.NewServer(new(handler.OrderServiceImpl), opts...)
kitexx.Run(svr, cfg.Config)
```

Versioning: semantic tags `vX.Y.Z`. Breaking changes bump the minor version
while we are on 0.x; services pin the version in their `go.mod`.

```sh
go test ./...
```
