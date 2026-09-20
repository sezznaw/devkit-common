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
| `config`  | 读取 `conf/<APP_ENV>.yaml`，支持 `${VAR}` 展开；`Dir`/`LoadDefault` 定位 conf 目录；`Duration` 支持 `3s` 这类写法 |
| `kitexx`  | Kitex 服务端/客户端选项：Nacos 注册与发现、优雅退出、统一日志、`OnShutdown`、`Run` |
| `nacosx`  | 由 YAML 配置构造 Nacos 命名/配置客户端 |
| `redisx`  | go-redis 客户端，创建时校验连通性 |
| `etcdx`   | etcd v3 客户端 |

最小的服务入口：

```go
var cfg struct{ kitexx.Config `yaml:",inline"` }
if err := config.LoadDefault(&cfg); err != nil { /* 打印错误并以 1 退出 */ }
opts, err := kitexx.Options(cfg.Config)
svr := orderservice.NewServer(handler.New(), opts...)
kitexx.OnShutdown("db", db.Close)      // 在最后一个请求处理完之后执行
kitexx.Run(svr, cfg.Config)
```

### `kitexx` 为服务提供了什么

- **优雅退出。** 收到 SIGINT/SIGTERM/SIGHUP 后，服务先从 Nacos 注销，继续服务 `shutdown.deregister_wait`（默认 3 秒）让调用方刷新实例列表，然后关闭监听，给在途请求最多 `shutdown.drain_timeout`（默认 15 秒），最后按注册的相反顺序执行 `OnShutdown` 钩子。这两个值之和要小于平台的强杀超时（Kubernetes 默认 30 秒）。
- **统一的日志格式。** Kitex 内部日志和你的日志走同一个 `slog` logger（带 `logger=kitex`），handler 的 panic 是一条记录，堆栈放在 `stack` 属性里，JSON 日志采集不会被破坏。
- **不依赖启动目录的配置加载。** `config.LoadDefault` 依次查找 `$CONF_DIR`、`./conf`、可执行文件旁边，文件缺失时给出明确的文字说明而不是 panic。
- **连不上 Nacos 时给出明确的提示。** 在创建 Nacos 客户端之前，服务会先检查配置的服务器，Nacos 2.x 客户端需要的两个端口都检查（主端口，以及用于 gRPC 的主端口加 1000）。连不上就直接停下，并说明是哪个地址、什么原因、该看哪个配置项，而不是 SDK 那句 `client not connected, current status:STARTING`。
- **每个进程只有一个 Nacos 连接**，注册和所有下游客户端共用（`nacosx.SharedNamingClient`）。
- 请求日志中间件，并把请求级 logger 放进 context；handler 的 panic 会被转成错误（这一点由 Kitex 自身提供）。

```yaml
shutdown:
  deregister_wait: 3s     # 0s 表示不等待
  drain_timeout: 15s
```

版本规则：语义化 tag `vX.Y.Z`。0.x 阶段破坏性变更提升次版本号；各服务在 `go.mod` 中锁定版本。

```sh
go test ./...
```
