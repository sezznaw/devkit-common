# common

[English](README.md) | **简体中文**

所有后端服务共用的 Go 库，按普通 module 引用：

```sh
go env -w GOPRIVATE=github.com/sezznaw      # 仅私有仓库需要
go get github.com/sezznaw/devkit-common@latest
```

| 包        | 作用 |
|-----------|------|
| `zlog`    | 公司统一日志，基于 zap：由编译器检查的强类型字段；给人看的控制台格式（颜色、可点击的 `文件:行号`）和给采集系统的 JSON 格式；带 `trace_id` 的请求级 logger；详见下文 |
| `config`  | 读取 `conf/<APP_ENV>.yaml`，支持 `${VAR}` 展开；`Dir`/`LoadDefault` 定位 conf 目录；`Duration` 支持 `3s` 这类写法 |
| `kitexx`  | Kitex 服务端/客户端选项：Nacos 注册与发现、优雅退出、统一日志与跨服务的 `trace_id`、`OnShutdown`、`Run` |
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
- **统一的日志格式。** Kitex 内部日志和你的日志一样走 zlog（带 `logger=kitex`，`caller` 是 Kitex 里打日志的那一行），handler 的 panic 是一条记录，堆栈放在 `stack` 字段里，JSON 日志采集不会被破坏。
- **跟随请求的 `trace_id`。** 服务端中间件取调用方传来的 `trace_id`，没有就生成一个，写进这次请求的每一条日志（`zlog.Ctx(ctx)`）；`ClientOptions`（TTHeader 传输）会把它继续传给本服务调用的其他服务，采集系统里用一个 id 就能看到整条调用链。请求日志里还会记录调用方是谁（`from`）。
- **运行时调整日志级别。** 设置 `log_level_data_id` 后，日志级别跟随一个 Nacos 配置，内容为 `debug`、`info`、`warn` 或 `error`。
- **不依赖启动目录的配置加载。** `config.LoadDefault` 依次查找 `$CONF_DIR`、`./conf`、可执行文件旁边，文件缺失时给出明确的文字说明而不是 panic。
- **连不上 Nacos 时给出明确的提示。** 在创建 Nacos 客户端之前，服务会先检查配置的服务器，Nacos 2.x 客户端需要的两个端口都检查（主端口，以及用于 gRPC 的主端口加 1000）。连不上就直接停下，并说明是哪个地址、什么原因、该看哪个配置项，而不是 SDK 那句 `client not connected, current status:STARTING`。
- **每个进程只有一个 Nacos 连接**，注册和所有下游客户端共用（`nacosx.SharedNamingClient`）。
- 请求日志中间件（方法、调用方、耗时、错误）；handler 的 panic 会被转成错误（这一点由 Kitex 自身提供）。

```yaml
shutdown:
  deregister_wait: 3s     # 0s 表示不等待
  drain_timeout: 15s
```

### 用 `zlog` 打日志

```go
zlog.Info("player login", zlog.Int("uid", uid), zlog.Str("ip", ip))
zlog.Error("save failed", zlog.Int("uid", uid), zlog.Err(err))
zlog.Infof("player %d login from %s", uid, zlog.Blue(ip))   // printf 风格
zlog.Ctx(ctx).Info("saved")                                  // 自动带上本次请求的 trace_id 和 method

var log = zlog.With(zlog.Str("component", "repo"))           // 写在包顶层也安全：会跟随 Init
```

- **字段是强类型的，由编译器检查**：`Str`、`Int`、`Float`（泛型：任何整数或浮点类型）、`Bool`、`Dur`、`Time`、`Err`（字段名固定为 `err`）、`Any`、`Secret`（输出为 `***`）。少写一个值、key 不是字符串都无法通过编译；`go vet` 会检查 `Infof` 的调用。
- **控制台格式给人看**（`format: console`）：每个级别一种颜色；`文件:行号` 在 GoLand 的 Run 窗口和终端里可以点击跳转（工作目录内为相对路径，之外为绝对路径）；字段显示为 `key=value`；多行的值和堆栈显示在记录下方，堆栈的每一行都可以点击。字段和 printf 参数都可以上色：`zlog.Int("age", 15).Blue()`、`zlog.Blue(uid)`（`Red Green Yellow Blue Purple Cyan Gray`；`err` 默认红色）。GoLand 的**测试**窗口会把颜色码显示成文字，请用 Run，或 `go run ./examples/zlog`。
- **JSON 格式给采集系统**（`format: json`）：每行一个对象，RFC 3339 带时区偏移的时间，短格式的 `caller`，每条记录都带 `service`、`env`、`host`、`version`（这几个标识字段是控制台唯一省略的内容）。一条记录里不会出现重复的 key：与记录自带字段同名的字段（`level`、`time`、`msg` 等）会变成 `fields.level`；两个同名字段保留后一个。
- **只有一种输出格式。** `Init` 会把标准库的 `log` 和 `log/slog` 也接到 zlog，`kitexx` 对 Kitex 的 klog 做同样的事，所以依赖库打的日志也是同一种格式。
- **服务一启动就能看到日志是怎么配置的。** `Init` 的第一条记录 `logger configured` 会列出实际生效的全部设置，名字与配置文件里的一致：没写的项显示默认值（控制台里标注 `(default)`），写错的项显示回落后的值。它不受日志级别影响，配置写错时的告警也一样；JSON 格式下这些设置放在一个对象里：`"config": {...}`。
- **`Init` 之前**（比如服务读不到配置）的日志由环境变量 `ZLOG_FORMAT`、`ZLOG_LEVEL`、`ZLOG_SERVICE`、`ZLOG_ENV` 决定格式；部署环境应设置 `ZLOG_FORMAT=json`。
- `zlog.Enabled(zlog.LevelDebug)` 用于保护构造代价高的日志；`zlog.SetLevel("debug")` 运行时调整级别；退出前调用 `zlog.Sync()`；`zlog/zlogtest` 包用于在测试里捕获或静音日志。

```yaml
log:
  level: info            # debug | info | warn | error
  format: json           # console | json
  stacktrace: error      # 从这个级别起附带调用堆栈；不写则不带
  max_field_bytes: 8192  # 截断过长的消息和字段值（容器会把超过 16KB 的行拆开）
  sampling: {first: 100, thereafter: 100}       # 洪峰保护，按每秒、每条消息计
  buffer: {size: 262144, flush_interval: 1s}    # 写缓冲，可选
log_level_data_id: order.log-level              # kitexx：日志级别跟随这个 Nacos 配置
```

版本规则：语义化 tag `vX.Y.Z`。0.x 阶段破坏性变更提升次版本号；各服务在 `go.mod` 中锁定版本。

```sh
go test ./...
```
