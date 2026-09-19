# common

[English](README.md) | **简体中文**

所有后端服务共用的 Go 库，按普通 module 引用：

```sh
go env -w GOPRIVATE=github.com/sezznaw      # 仅私有仓库需要
go get github.com/sezznaw/devkit-common@latest
```

| 包        | 作用 |
|-----------|------|
| `log`     | 公司统一日志，基于 `log/slog`：`New`、`Named`、`WithContext` / `FromContext` |
| `config`  | 读取 `conf/<APP_ENV>.yaml`，支持 `${VAR}` 环境变量展开 |
| `kitexx`  | Kitex 服务端/客户端选项：Nacos 注册与发现、日志中间件、`Run` |
| `nacosx`  | 由 YAML 配置构造 Nacos 命名/配置客户端 |
| `redisx`  | go-redis 客户端，创建时校验连通性 |
| `etcdx`   | etcd v3 客户端 |

最小的服务入口：

```go
var cfg struct{ kitexx.Config `yaml:",inline"` }
config.MustLoad("conf", &cfg)
opts, _ := kitexx.Options(cfg.Config)
svr := orderservice.NewServer(new(handler.OrderServiceImpl), opts...)
kitexx.Run(svr, cfg.Config)
```

版本规则：语义化 tag `vX.Y.Z`。0.x 阶段破坏性变更提升次版本号；各服务在 `go.mod` 中锁定版本。

```sh
go test ./...
```
