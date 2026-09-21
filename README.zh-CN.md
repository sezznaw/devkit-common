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
| `nacosx`  | Nacos：服务注册与发现、运行中自动刷新的配置，所有输出都走 zlog；详见下文 |
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
- **每个进程只有一个 Nacos 客户端**，注册、所有下游客户端和配置共用（`kitexx.Nacos(cfg)`、`nacosx.Shared`）。
- **自动刷新的配置**：`kitexx.WatchConfig[T](cfg)`，见下文 `nacosx`。
- 请求日志中间件（方法、调用方、耗时、错误）；handler 的 panic 会被转成错误（这一点由 Kitex 自身提供）。

```yaml
shutdown:
  deregister_wait: 3s     # 0s 表示不等待
  drain_timeout: 15s
```

### 用 `nacosx` 对接 Nacos

```go
type Dynamic struct {                       // 只放服务运行期间可以改的东西
    Battle struct {
        MaxLevel int `yaml:"max_level"`
    } `yaml:"battle"`
}

dyn, err := kitexx.WatchConfig[Dynamic](cfg.Config)  // data id 取 config_data_id，默认 "<service.name>.yaml"
...
if level > dyn.Get().Battle.MaxLevel {               // 永远是当前生效的版本；只是一次原子读取
```

不用 Kitex 的程序写法相同：`nc, err := nacosx.New(cfg.Nacos)`，然后 `nacosx.Watch[Dynamic](nc, "gateway.yaml")`。

- **在 Nacos 控制台改完配置，片刻之后就生效**，不需要重启。每次变更都是一个新的 `T`，之前 `Get` 返回的那一份永远不会再被写入，所以不需要加锁。`dyn.OnChange(func(old, cur *Dynamic))` 用于需要“重建”而不是“读取”的东西（限流器、连接池）。
- **改错配置不会把服务搞挂。** 解析失败，或被 `Validate() error`（可选，定义在 `*T` 上）拒绝的内容不会生效：打一条 ERROR，上一个版本继续生效；配置被删除时同样保留最后一个版本。而在启动阶段，同样的问题会直接阻止启动：服务不应该带着半份配置启动，也不应该在 Nacos 不应答时用一份不知道多旧的本地快照启动。
- **日志会说清楚改了什么**，逐个 key 列出；看起来像密钥的 key（`password`、`secret`、`token`、`*_key` 等）的值会被隐藏；程序里没有对应字段的 key 会被指出来，手误写错的 key 就是这个样子：

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

  JSON 格式下 `changes` 是 `{op, key, from, to}` 的数组。
- **服务注册与发现**，给不是 Kitex 服务的程序用：`deregister, err := nc.Register(nacosx.Registration{Service: "gateway", Addr: ":8080"})`、`nc.Instances("order")`、`nc.Pick("order")`（按权重随机）、`nc.Subscribe("order", fn)`。Kitex 服务由 `kitexx.Options` 和 `kitexx.ClientOptions` 代劳。两种方式日志里都能看到 `registered in Nacos`、`service discovered`、`instances changed`（每个实例一行：`+ 10.0.0.7:8888  weight=10`、`- 10.0.0.6:8888`）。
- **Nacos 故障期间只有三种记录，而不是几百条。** `nacos connection lost`（`cause` 是 SDK 的第一条报错）、每 30 秒一条 `nacos is still unreachable`、恢复时一条 `nacos connection restored down_for=48.7s sdk_records_hidden=194`。期间服务继续使用手上已有的实例列表和配置，恢复之后 SDK 会自己重新注册和订阅。
- **Nacos SDK 自己的日志也走 zlog**（带 `logger=nacos-sdk`），不再写 `nacos-sdk.log` 文件；由 `nacos.sdk_log_level` 控制从哪个级别起输出（默认 `warn`；设为 `info` 时 SDK 会打印每一份配置的内容，包括密码）。
- **没有 Nacos 时**（`registry_disabled: true`，本机开发的常用设置）配置就是文件：`conf/nacos/<data id>`，保存文件就等同于在控制台里改配置。此时不注册、也不查找服务：用 `client.WithHostPorts` 指明服务地址。
- 更底层的接口：`nc.Get(dataID)`、`nc.OnChange(dataID, func(content string))`、`nacosx.Group("other")`（配置在别的分组）、`nacosx.Static(&Dynamic{...})`（测试用）、`nacosx.NewFrom`（在底下放假的 SDK 客户端）。`go run ./examples/nacosx` 可以看到以上全部效果。

```yaml
nacos:
  addrs: ["nacos:8848"]
  namespace: ""            # 命名空间 ID；每个环境一个
  group: DEFAULT_GROUP     # 服务和配置共用的分组
  sdk_log_level: warn      # debug | info | warn | error
config_data_id: order.yaml # kitexx.WatchConfig 使用；默认 "<service.name>.yaml"
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
- **服务一启动就能看到日志是怎么配置的。** `Init` 的第一条记录 `logger configured` 会列出实际生效的全部设置，名字与配置文件里的一致：没写的项显示默认值（控制台里标注 `(default)`），写错的项显示回落后的值。它不受日志级别影响，配置写错时的告警也一样；JSON 格式下这些设置放在一个对象里：`"config": {...}`。由 `kitexx` 代填的值会注明来源：`service: order (from service.name)`、`env: prod (from APP_ENV)`，或 `env: dev (APP_ENV is not set)`；JSON 格式下这些说明放在 `"config_notes": {...}` 里。
- **`Init` 之前**（比如服务读不到配置）的日志由环境变量 `ZLOG_FORMAT`、`ZLOG_LEVEL`、`ZLOG_SERVICE`、`ZLOG_ENV` 决定格式；部署环境应设置 `ZLOG_FORMAT=json`。
- **采样会说明它丢了多少。** 开启 `sampling` 后，凡是有日志被丢弃的那一秒，结束时都会按“级别 + 消息”各补一条记录：`zlog: records dropped by sampling dropped_msg="redis down" dropped=4213`，级别与被丢弃的日志相同，这样从日志里就能看出洪峰的规模。`zlog.Sync()` 会立即写出当前这一秒的计数。
- `zlog.Enabled(zlog.LevelDebug)` 用于保护构造代价高的日志；`zlog.SetLevel("debug")` 运行时调整级别；退出前调用 `zlog.Sync()`；`zlog/zlogtest` 包用于在测试里捕获或静音日志。
- 实现了 `zlog.Blocker`（`LogBlock(colored bool) string`）的值传给 `zlog.Any` 时，控制台里显示为记录下方的若干行，JSON 里仍是值本身；`nacosx` 的 `changes` 和 `instances` 就是这样输出的。

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
