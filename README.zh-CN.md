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
| `config`  | 读取 `conf/<APP_ENV>.yaml`，支持 `${VAR}` 展开（`${VAR:-默认值}` 未设时取默认）；`Dir`/`LoadDefault` 定位 conf 目录；`Duration` 支持 `3s` 这类写法 |
| `kitexx`  | 服务的 Runtime：配置里开了什么就在注册前打开（`rt.DB`、`rt.Redis`、`rt.Kafka`、`rt.Centrifugo`、`rt.S3`、`rt.Delay`、`rt.ID`、`rt.Report`、`rt.Provider(名字)`），Kitex 服务端/客户端选项（Nacos 注册发现、trace、指标、请求日志、参数校验、幂等、`limits:`），优雅停机，`--job`、`--verify`、`--dlq`；见下文 |
| `hertzx`  | API（HTTP）服务的同一套：和 `kitexx` 共用配置，带 trace_id 的请求日志一路传到 RPC 服务，recovery、CORS、网关防护（`guard:` 请求体、时限、限流）、客户端设备与 IP（`client:`、`X-Device-Type`）、按 IDL 注解的 `RequireLogin` / `RequirePermission` / `Audit`、`Bind` 校验、加 `{code, msg, data}` 壳的 `OK` / `Fail` / `FailCode`、`/docs`；见下文 |
| `devroute` | 从开发者本机接管 dev 的一个服务：`X-Dev-Tag` 作为 metainfo 持久值 `DEV_TAG` 跨每一跳，负载均衡器把带 tag 的请求送到同 tag 的接管实例（Nacos 分组 `DEV_TAKEOVER`），带 tag 的请求用调试超时；只在 `APP_ENV=dev` 开。工具是 `devkit takeover`；见下文 |
| `pagex`   | 分页：`Normalize(req.Page)` 返回 page、size、offset，默认值（第 1 页、20 条、最多 100）只在这一处；见下文 |
| `cmd/apidoc`、`cmd/errgen` | `make gen` 跑的两个生成器：网关从 IDL 目录生成 OpenAPI 文档（壳、required、x-error-codes）；每个服务从 `idl/errors/*.thrift` 的注释生成 `Message` / `Err` / `Errf`；见下文 |
| `otelx`   | trace（OpenTelemetry）：`kitexx`/`hertzx` 按 `otel:` 段装好，每个 RPC、HTTP 调用、SQL、Redis 命令、Kafka 消息都是一个 span；不用直接调 |
| `nacosx`  | Nacos：服务注册与发现、运行中自动刷新的配置，所有输出都走 zlog；详见下文 |
| `kafkax`  | 事件总线（Redpanda）：`Publish` 自动填信封和 trace 头，`Subscribe` 的消费者由框架在服务起来后拉起，失败重试后进 `<主题>.dlq`；详见下文 |
| `centrifugox` | 推送（Centrifugo）：`Publish` 到频道，`ConnectionToken` 给客户端签连接 JWT；详见下文 |
| `authx`   | 登录态：一个认证服务按领域（member、admin、agent，项目可自加如 finance_admin）签发 access（JWT，15 分钟）和 refresh（30 天，轮换、检测重放）令牌，会话在 Redis；每个网关本地校验自己领域的令牌加一次 Redis 读，把身份放进上下文（`kitexx.UID(ctx)`，随 RPC 透传）；`hertzx.RequireLogin` 保护 IDL 里没标 `// @public` 的所有接口；详见下文 |
| `s3x`     | 对象存储（dev 是 SeaweedFS，以后可换任何 S3），按 `s3:` 配置：`rt.S3.Put/Get/Stat/List/Delete`、预签名下载和上传链接、公开桶 `rt.S3.Public()`（每个对象一个固定 `URL(key)`），所有文件在服务自己的前缀下；库里只存 key 不存 URL；source static 或本部署的数据源表；每次调用一个 span 和一条指标；详见下文 |
| `httpx`   | 调外部 HTTP 接口（resty + otelhttp）：`providers:` 里登记 base URL、超时、重试、来自环境变量的头；`rt.Provider("odds-feed").GetJSON(ctx, path, &out)`；trace 带过去，每次调用一条不含密钥的日志，指标；有 providers 的服务必须部署在 provider namespace；详见下文 |
| `webhookx` | 厂商回调我们（支付结果、赛果结算），在 provider namespace 的 Hertz 服务上：`webhookx.Handle(rt, h, "pay", "/callbacks/pay/paid", onPaid)`；来源 IP、验签（hmac-sha256、basic 或厂商私有 Verifier）、原始报文留档到 Kafka、按事件 id 用 Redis 去重、handler 出错回 500 让厂商重试、指标；详见下文 |
| `metricsx` | Prometheus 指标，单独端口（`metrics.enabled`，默认 9091）：Kitex/Hertz 每个方法的请求数、结果码、耗时直方图，对外 RPC 调用，Go 运行时，MySQL 连接池，Redis 命令，Kafka 事件；由部署抓取；详见下文 |
| `jobx`    | 定时任务：服务在 `app/jobs.go` 里列出任务（名字、cron 时间表、超时、函数），框架按 `--job=<名>` 跑一个任务——根 span、日志带 run_id、锁、超时、退出码；`--list-jobs` 的输出由部署变成 CronJob；详见下文 |
| `redisx`  | 按 `redis:` 段连 Redis / Valkey，规则同 `mysqlx`（enabled、source static 或 platform），每条命令一个 span；`WithLock`（跨副本锁）、`Cache[T]`（读穿缓存）；幂等中间件用它；见下文 |
| `mysqlx`  | 按 `mysql:` 配置打开 MySQL（GORM）：连接池、每条 SQL 一个 span 和一条带 trace_id 的日志、慢查询告警；地址来自配置或平台的数据源表；详见下文 |
| `starrocksx` | 报表库（StarRocks，业务表的 CDC 副本）：`rt.Report.Query` / `QueryRow`，必须带 `:tenant`，只读，只给报表服务；见下文 |
| `delayx`  | 到某一刻执行一次（取消未支付订单、开赛封盘）：业务事务里 `rt.Delay.Schedule`，app.Setup 里 `rt.Delay.Handle`；至少一次、退避重试、failed 状态、指标；见下文 |
| `idx`     | 业务唯一号（`rt.ID.Next()`）：53 位、按时间有序、跨副本唯一（实例号在 Redis 租）、JSON 数字装得下；见下文 |
| `moneyx`  | 金额 = 小数 amount + 货币码，底层 shopspring/decimal，舍入模式、分账、`common.Money` 传输形式；见下文 |
| `verifyx` | 部署后验证：框架的检查加 `app/verify.go`，`--verify` 作为 PostSync Job 跑；见下文 |
| `testx`   | 测试用 Runtime（进程内 Redis 和 Kafka、记录推送的 Centrifugo、按迁移建的库）、身份 ctx、网关 JSON 助手；见下文 |

最小的服务入口：

```go
var cfg struct{ kitexx.Config `yaml:",inline"` }
if err := config.LoadDefault(&cfg); err != nil { /* 打印错误并以 1 退出 */ }
rt, err := kitexx.NewRuntime(cfg.Config)   // 日志、链路追踪，以及配置里启用的连接（MySQL …）
opts, err := rt.Options()
svr := orderservice.NewServer(handler.New(repo.New(rt)), opts...)   // rt.DB 交给 repo
kitexx.OnShutdown("cache", cache.Close)    // 在最后一个请求处理完之后执行
kitexx.Run(svr, cfg.Config)
```

### `kitexx` 为服务提供了什么

- **优雅退出。** 收到 SIGINT/SIGTERM/SIGHUP 后，服务先从 Nacos 注销，继续服务 `shutdown.deregister_wait`（默认 3 秒）让调用方刷新实例列表，然后关闭监听，给在途请求最多 `shutdown.drain_timeout`（默认 15 秒），最后按注册的相反顺序执行 `OnShutdown` 钩子。这两个值之和要小于平台的强杀超时（Kubernetes 默认 30 秒）。
- **统一的日志格式。** Kitex 内部日志和你的日志一样走 zlog（带 `logger=kitex`，`caller` 是 Kitex 里打日志的那一行），handler 的 panic 是一条记录，堆栈放在 `stack` 字段里，JSON 日志采集不会被破坏。
- **跟随请求的 `trace_id`。** 服务端中间件取调用方传来的 `trace_id`，没有就生成一个，写进这次请求的每一条日志（`zlog.Ctx(ctx)`）；`ClientOptions`（TTHeader 传输）会把它继续传给本服务调用的其他服务，采集系统里用一个 id 就能看到整条调用链。请求日志里还会记录调用方是谁（`from`）。
- **链路追踪（OpenTelemetry）。** 配置里写上 `otel.endpoint`（OTLP gRPC 采集端，比如 Tempo 的 4317 端口），每个 RPC 就自动有一个服务端 span，本服务调用别人时有一个客户端 span，W3C `traceparent` 随 TTHeader 传给被调方，所以 Grafana 里能看到整条调用链的瀑布图。这时日志里的 `trace_id` **就是** span 的 trace id，Loki 和 Tempo 用同一个 id 互跳。只带 `trace_id` 不带 `traceparent` 的老调用方也能续上同一条 trace。`endpoint` 留空（通常是 `"${OTEL_EXPORTER_OTLP_ENDPOINT}"` 没有设置）时一切照旧：不记录 span，`trace_id` 还是原来的生成和传递方式。服务退出时会把没发完的 span 发出去。
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
otel:
  endpoint: "${OTEL_EXPORTER_OTLP_ENDPOINT}"   # OTLP gRPC 采集端，如 tempo.monitoring:4317；空 = 关闭
  sample_ratio: 1.0       # 新 trace 的采样比例；调用方已决定的沿用其决定
  insecure: true          # 集群内不走 TLS
```

### `hertzx` 为 API 服务提供了什么

API 服务（`devkit nas`）的启动方式和 RPC 服务一样，`hertzx.Config` **就是** `kitexx.Config`：同样的 `conf/*.yaml`，同样的规则。

```go
rt, err := kitexx.NewRuntime(cfg.Config)   // 日志、链路追踪，以及配置里启用的连接
h, err := hertzx.New(rt)           // 监听、请求日志、panic 恢复、对 Nacos 的检查
app.Setup(&cfg, rt, h)             // 服务自己的部分：RPC 客户端、中间件
router.GeneratedRegister(h)        // hz 根据 IDL 生成的路由
err = hertzx.Run(h, cfg.Config)    // 提供服务，然后优雅退出
```

- **每个请求一条记录** `http`，带 `method`、`path`、`status`、`latency`、`client`；5xx 是错误，4xx 是警告。handler 里用 `zlog.Ctx(ctx)` 打的日志带着同一个 `trace_id`、`method` 和 `path`。
- **从 HTTP 请求到最后一个 RPC 服务是同一个 `trace_id`。** 开着链路追踪时，每个请求是一个服务端 span（名字是 `GET /user/:id` 这样的方法加路由），请求头里的 W3C `traceparent` 让它接在调用方的 span 下面；只有 `X-Trace-Id` 且是 32 位十六进制时，它就是这条 trace 的 id。关着时，`X-Trace-Id` 看起来像一个 id（8 到 64 位的字母、数字、`-`、`_`；其他内容不会进入日志）就沿用它，否则新生成一个。无论哪种情况，最终的 `trace_id` 都会写进响应头，并按 `kitexx` 的方式放进 context，所以用 `kitexx.ClientOptions` 创建的客户端会把它继续传下去（开着时还带着 span 的父子关系）。
- **handler 里的 panic** 是一条带堆栈的错误记录，并返回 500。
- **和 RPC 服务一样的退出顺序**：从 Nacos 注销，继续服务 `shutdown.deregister_wait`，关闭监听，给处理中的请求 `shutdown.drain_timeout`，执行 `OnShutdown` 钩子。Hertz 自带的 `Spin` 会把这些事同时做，而且不管有没有开始监听都在启动 1 秒后注册，所以由 `Run` 取代它，并在端口真正可以连接之后通过 `nacosx` 注册（元数据里带 `protocol=http`）。
- **端口被占用时是一句话**，说明是哪个端口、该改 `service.addr`；Hertz 自己遇到这种情况会 panic 并打出一大段 goroutine 堆栈。
- Hertz 自身的日志走 zlog，带 `logger=hertz`。

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
- **每个服务都会说明当前有哪些服务存活。** 启动时打一条 `services alive`，列出每个服务的实例数和地址。之后每当有服务上线（`service online`）、下线（`service offline`）或实例数变化（`service instances changed`），都会打一条记录，既说明谁来了、谁走了，也再次列出当前存活的全部服务，所以最后一条这类记录永远反映当前的状况：

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

  `+` 表示新上线（绿色），`-` 表示已下线（红色；整个服务都没了时它仍保留一行，N 为 0），`~` 表示状态变了（黄色）。JSON 格式下同样的内容是 `overview: {service, changed, alive}`。

  已知服务的实例变化由 Nacos 推送（一秒以内）；全新的服务名靠每 3 秒查询一次服务列表来发现。这个功能使用一个单独的 Nacos 客户端，因为调用路径上的那个客户端永远收不到“某个服务的最后一个实例下线了”：Nacos 推来空列表时，SDK 会保留上一份列表，用来保护调用方不受 Nacos 丢数据的影响。这层保护保持不变，只有这些记录能看穿它。开启后，调用路径自己的 `service discovered` / `instances changed` 降为 debug 级别。`nacos.watch_services: false` 可以关闭；不是 Kitex 服务的程序调用 `nc.WatchServices(every)`。
- **笔记本不会出现在别人也在用的 Nacos 里。** 设置 `nacos.register: false` 后，服务照常查找其他服务、读取配置，只是不注册自己：开发者的电脑连开发服务器的 Nacos 时就这样用。不设置它时，本机环境（没有 `APP_ENV`）只允许注册到本机上的 Nacos；其余情况启动时直接拒绝，并说明地址和解决办法，因为否则所有使用那个 Nacos 的人都会有一部分请求被发到这台笔记本上。
- **注册进去的一定是调用方连得上的地址。** 主机留空时，注册的是“本机连向 Nacos 时使用的地址”：电脑上有 VPN 或 Docker 网络时它就是对的那一个；Nacos 在本机时它是 127.0.0.1，Wi-Fi 地址变了也不受影响。通过宿主机地址和映射端口访问的容器要设置 `service.advertise`（`"主机"` 或 `"主机:端口"`，通常写 `"${ADVERTISE_ADDR}"`）。
- **没有 Nacos 时**（`registry_disabled: true`，用于测试和断网时工作）配置就是文件：`conf/nacos/<data id>`，保存文件就等同于在控制台里改配置。此时不注册、也不查找服务：用 `client.WithHostPorts` 指明服务地址。
- 更底层的接口：`nc.Get(dataID)`、`nc.OnChange(dataID, func(content string))`、`nacosx.Group("other")`（配置在别的分组）、`nacosx.Static(&Dynamic{...})`（测试用）、`nacosx.NewFrom`（在底下放假的 SDK 客户端）。`go run ./examples/nacosx` 可以看到以上全部效果。

```yaml
nacos:
  addrs: ["nacos:8848"]
  namespace: ""            # 命名空间 ID；每个环境一个
  group: DEFAULT_GROUP     # 服务和配置共用的分组
  sdk_log_level: warn      # debug | info | warn | error
config_data_id: order.yaml # kitexx.WatchConfig 使用；默认 "<service.name>.yaml"
```

### 用 `mysqlx` 连 MySQL

**框架代码只有一套，用不用 MySQL 由配置决定。** 每个服务的配置都有 `mysql:` 一段，默认 `enabled: false`；
启用后 `kitexx.NewRuntime` 在服务注册到 Nacos **之前**连库、ping，连不上就不启动并说明原因，
连上的句柄在 `rt.DB`（`*gorm.DB`）。服务代码不打开连接，只在 repo 里用它：

```go
// repo/member.go
func (r *Repo) GetMember(ctx context.Context, uid int64) (*Member, error) {
	var m Member
	err := r.db.WithContext(ctx).Where("uid = ?", uid).First(&m).Error   // WithContext：这条 SQL 是请求 trace 里的一个 span
	return &m, err
}
```

数据库在哪，`mysql.source` 说了算：

```yaml
# conf/dev.yaml、uat、prod：部署只说自己服务哪个租户（环境变量 TENANT_CODE），
# 地址从平台库的 datasource 表取，平台库的地址在 Nacos 的 infra.yaml，密码都来自环境变量
mysql:
  enabled: true
  source: platform
  role: tenant            # tenant：本租户的库；platform：平台库本身

# conf/local.yaml：开发者自己的库
mysql:
  enabled: true
  source: static
  addr: "${MYSQL_ADDR}"   # 不设就是 127.0.0.1:3306
  db: tenant_a
  user: tenant_a
  password: "${MYSQL_PASSWORD}"
  max_open: 20            # 连接池，默认 20 / 5 / 30m
  max_idle: 5
  conn_max_lifetime: 30m
  slow_query: 200ms       # 超过就记一条 warn "mysql slow query"
```

- **每条 SQL 一条日志**，带请求的 `trace_id`：正常 debug，慢的 warn，失败的 error（查不到行不算失败）；
  每条 SQL 也是请求 trace 里的一个 span（语句不带参数值）。
- **本机连了别人也在用的库会有一条 WARN**（source static、地址不是本机、`APP_ENV` 未设置），
  和 Nacos 的"笔记本不许注册到共享 Nacos"是同一类提醒。
- **GORM 的约束**：不用 `AutoMigrate`（表结构只来自迁移文件）；不定义关联、不用 `Preload`（要连表就写明确的 Join）；
  事务显式传递（`repo.WithTx(tx)`），资金路径的多表写在一个明确的事务里；
  `SkipDefaultTransaction` 已开，一条语句就是一条语句。
- `rt` 属于框架：服务从里面取，不往里加。

### 表结构在模型里：`rt.AutoMigrate`

**表结构写在项目 `lib/model/mysql` 里，一张表一个 Go 结构体；拥有这张表的服务在启动时把表补到和结构体一致。**
业务表不写 SQL 迁移文件，repo 里不写 `CREATE TABLE`：

```go
// app/app.go
func Setup(cfg *Config, rt *kitexx.Runtime) (*handler.Impl, error) {
	if err := rt.AutoMigrate(mysql.Bet{}, mysql.ExportTask{}); err != nil {   // 本服务拥有的表
		return nil, err
	}
	...
}
```

```go
// lib/model/mysql/bet.go：每个字段写明 column 和 type
type Bet struct {
	ID          int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement"`
	BetNo       int64  `gorm:"column:bet_no;type:bigint;not null;uniqueIndex:uk_bet_no"`
	StakeAmount string `gorm:"column:stake_amount;type:decimal(24,3);not null"`
	SettleTime  int64  `gorm:"column:settle_time;type:bigint;not null;default:0;index:idx_bet_settle_time"`
}
func (Bet) TableName() string { return "bet" }
```

`rt.AutoMigrate` 做的事：缺表建表，缺列、缺索引补上，列的类型、可空、默认值和模型不一致就改过来（放宽 varchar、int 变 bigint、
换默认值），每处差异打一条日志（`auto migrate: bet: columns added [settle_time]; columns altered [remark: varchar(64) -> varchar(255)]`），
没差异也说一句。它用两条 `information_schema` 查询一次读完这批表的列和索引（和表数无关），在内存里比对，没差异立刻返回；
有差异才拿 Redis 锁（`lock:<服务>:auto-migrate`，多副本、多服务同时启动也只有一个在改表），锁内再规划一次，**只把有差异的表**交给
GORM，GORM 改之前还会逐列再比一遍。字段没有显式 `type:` 直接拒绝、不碰库：GORM 的默认类型（string 变 longtext、float 变 double）不是金额列该有的样子。

它**故意不做**、必须由人来做的改动：删字段或改字段名（GORM 只加不删，改名会变成旧列旁边多一列）；收窄类型（varchar 变短、
整数变小或有符号变无符号、decimal 位数变少、text 变 varchar：服务拒绝启动并指出是哪一列，因为 ALTER 可能截断或拒绝已有数据）；
需要回填数据的变更。这些走人工迁移，和管数据库的人商量。

在哪开着：`local` 和 `dev` 开，表结构跟着代码走；`uat` 和 `prod` 关，只打印差异，由发布流程把迁移当独立一步跑。
`mysql.auto_migrate: true|false` 可覆盖。`--verify` 下什么都不做。接管模式（`devkit takeover`）下不管怎么配都不改表：本机实例连的是共享的 dev 库，本地模型里可能有没推送的改动，所以只打印差异（推送模型、发布服务后 dev 的表自然跟上）。一张表只由一个服务迁移（它所属的领域），别的服务只读。

框架自己的表（PublishTx 的 `outbox`、延时任务的 `delayed_task`）也由框架在开了对应功能时按同一规则建（`EnsureFrameworkTables`，
`NewRuntime` 调），项目里不声明它们。

测试：`testx.New(t, "ser-bet", testx.WithMySQL(""), testx.WithModels(mysql.Bet{}))`，一个干净的新库，框架表和本服务的表都在，
哪里都没有 SQL 迁移。

### 用 `redisx` 连 Redis，以及幂等请求

`redis:` 段和 `mysql:` 一模一样：`enabled`、`source: static`（`addr`、`password`、`db`）或 `source: platform`
（datasource 表里 kind 为 valkey 的那一行，`db_name` 是库序号），框架在启动时打开，句柄在 `rt.Redis`。

**幂等请求**：接口规范要求资金和状态变更的请求带 `request_id`。IDL 里给 Req 加一个 `request_id` 字段，
这个方法就自动是幂等的——`rt.Options()` 装的中间件会：

- 第一次调用正常执行，结果按 `request_id` 保存 24 小时
- 同一个 `request_id` 再来，直接返回保存的结果，handler 不再执行（日志 `replayed`）
- 第一次还在执行中又来一次：业务码 1002
- `request_id` 留空：业务码 1003
- handler 返回 error：丢掉这个 id，调用方可以重试
- 没有开 `redis.enabled`：服务启动时就拒绝启动并列出这些方法（v0.52.3 起）；万一跑起来了，请求一律 5002——转账执行两次比拒绝更糟

没有 `request_id` 字段的方法不受影响。

**往下游传 `request_id`**（v0.53.0 起）：handler 处理一个带 `request_id` 的请求时又调了别的服务的写方法，下游的 id
必须是这个 id 或由它派生，上游重试时下游才认得出是同一次。框架把收到的 id 放进 ctx（RPC 由幂等中间件放，
网关由 `hertzx.New` 装的 `RequestID()` 从请求体取），提供：

- `kitexx.RequestID(ctx)`：当前请求的 id，照原样传下去
- `kitexx.SubRequestID(ctx, "fee")`：同一请求里对同一个方法调两次时各用各的（`<id>:fee`、`<id>:stake`），part 是业务含义，不是重试计数
- `kitexx.WithRequestID(ctx, id)`：job、事件消费者、延时任务没有上游 id，自己定一个每次重跑都相同的（`"settle:"+matchID`、`"event:"+ev.ID`）

`ClientOptions` 装的客户端中间件 `ClientRequestID` 在发出前检查，不对就不发、返回错误并记 Error 日志：

- id 为空
- 在一个带 id 的请求里，传了不是由它派生的 id（新生成的 UUID、时间戳）——重试时下游会再执行一次
- 同一请求里用同一个 id 调同一个方法第二次——下游只会回放第一次的结果
- 调用失败后用同一个 id 重试是允许的，这正是幂等的用途

`devkit lint` 的 `request-id-source` 规则在写代码时拦同样的错：`RequestId` 字段只能取自请求（`req.RequestId`、
`req.GetRequestId()`）或这几个函数。

### 锁与缓存 `redisx`

**跨副本的锁。** `redisx.WithLock(ctx, rt.Redis, "settle:"+matchID, 30*time.Second, func(ctx context.Context) error {...})` 在服务所有副本范围内持锁执行函数（key `lock:<服务>:<名字>`）：结算一场比赛、改一个会员的余额、同步一个厂商。第二个调用方立刻得到 `redisx.ErrLocked`，或 `redisx.Wait(2*time.Second)` 等一会再放弃。函数运行期间锁在后台自动续期，所以 TTL 是"进程死掉多久后别人能接手"，不是工作时限；中途锁丢了（Redis 不可用、key 被删）函数的 ctx 被取消，WithLock 返回 `redisx.ErrLockLost`。handler 里的 `sync.Mutex` 只锁本副本，所以 `devkit lint` 的 `mutex` 规则在 handler、repo 里拦它，自己写 `SetNX` 也拦（规则 `lock`）。指标 `locks_total{name,result}`、`lock_wait_seconds`。

**读穿缓存。** `members := redisx.NewCache[repo.Member](rt.Redis, "member", 5*time.Minute).Negative(repo.ErrMemberNotFound, 30*time.Second)`，然后 `m, err := members.Get(ctx, id, func(ctx) (repo.Member, error) { return r.load(ctx, id) })`：有缓存给缓存（key `cache:<服务>:member:<id>`，JSON），没有就跑 loader——同一个 key 同一进程同时只跑一次，热 key 不会打穿数据库——结果按 TTL ±10% 存起来。`Negative` 把"查无此物"也记一小段时间。写方在事务**提交之后**调 `members.Del(ctx, id)`（提交前删，并发的读会把旧值填回去）。Redis 不可用时 Get 就是直接调 loader，每分钟告警一条：缓存是优化，不是真相来源；`rt.Redis` 为 nil（没开 redis）同理。类型结构变了就换个缓存名。指标 `cache_requests_total{name,result}`（hit、miss、negative、error）。

### 用 `kafkax` 发事件和消费事件

`kafka:` 段和前面一样（`enabled`、`source static` 的 `brokers` 或 `source platform` 的 datasource 表 kind 为 kafka 的行），
句柄在 `rt.Kafka`。

**发**：`rt.Kafka.Publish(ctx, "events.member", "42", "member.updated", data)`——主题、分区键（实体 id，同一实体有序）、
事件名、数据。框架包上标准信封 `{id, type, tenant_id, time, data}`，把 ctx 的 trace 放进消息头，等集群确认后返回。

**收**：`app.Setup` 里 `rt.Kafka.Subscribe("events.member", func(ctx, ev *kafkax.Event) error { ... })`，
消费组是服务名。框架在服务启动后拉起消费者（`rt.Run`），退出时先停消费者。每条消息是生产者那条 trace 里的一个 span、
一条带 trace_id 的日志；handler 返回 error 重试 3 次（`kafka.max_retries`）后发到 `<主题>.dlq`（消息头带 error）
并继续，不堵分区；handler panic 同样处理。至少一次语义，handler 要幂等（按 `ev.ID` 去重或操作本身幂等）。

事件命名、主题命名在 idl 仓库的 README。

#### 消费者只看到一次

Kafka 和 outbox 都是至少一次投递，同一条事件可能到消费者两次（重平衡、回放、转发器重试）。有 Redis 时框架替你去重：handler 跑之前先占 `dedupe:<服务>:<主题>:<事件 id>`（`kafka.dedupe.ttl`，24h），第二次投递记日志、计数（`kafka_events_handled_total{result="duplicate"}`）、提交位移但不跑 handler。handler 失败进死信时会忘掉这个 id，所以从死信回放还会执行。`kafka.dedupe.enabled: false` 关掉；没有 Redis 就不去重并告警。handler 仍要幂等，覆盖去重管不到的情形（两个生产者用不同 id 发同一件事）。

#### 事务里的事件：outbox

和数据库变更同属一件事的事件走 outbox，不走 `Publish`：在事务里 `rt.Kafka.PublishTx(ctx, tx, "events.wallet", id, "wallet.debited", data)` 把事件写进本服务的 `outbox` 表（项目迁移建表，`kafkax.OutboxDDL` 是表结构）。事务提交则变更和事件都落地，回滚则都没有；同一进程里的转发器（服务同时有 Kafka 和 MySQL 时自动开，`kafka.outbox.enabled: false` 关）随后把行发出去，信封和 `Publish` 一样，trace 是发起请求的那条。至少一次投递、event id 不变，消费者照旧按 `ev.ID` 去重。发不出去的行按退避重试（1s、2s…5 分钟；表里有 `attempts`、`last_error`、`next_attempt_at`），一个坏主题不会挡住别的；多副本分摊（`FOR UPDATE SKIP LOCKED`）。已发的行保留 `kafka.outbox.retention`（7 天）后删除。指标 `kafka_outbox_pending`、`kafka_outbox_oldest_age_seconds`（按它告警）、`kafka_outbox_published_total{topic,status}`。`devkit lint` 的 `outbox` 规则拒绝在跑事务的代码里直接 `Publish`。

#### 死信与积压

handler 一直失败的消息在 `<主题>.dlq` 里，错误在 header 里。`<二进制> --dlq list` 列出本服务订阅的每个主题的死信队列里待处理的消息（offset、时间、key、事件类型和 id、错误）；`--dlq show <主题> <offset>` 看一条的全文；`--dlq replay <主题>` 把待处理的全部重发回原主题——原始记录加一个 `replayed-from` 头，修好的 handler 像第一次一样处理——然后标记完成；`--dlq replay <主题> <offset>` 只重发一条；`--dlq drop <主题>` 不重发直接标记完成。"待处理"是工具自己的消费组（`<服务>-dlq`）还没提交过去的部分，队列本身只追加。线上在服务自己的 Pod 里跑：`kubectl -n <ns> exec deploy/<服务> -- /app/<服务> --dlq list`；本机 `make dlq ARGS="list"`。先把修复发上去再回放。消费者还会上报 `kafka_consumer_lag{topic}`（每次拉取后离主题末尾还差多少条），`kafka_events_handled_total{result="dlq"}` 计被放进死信的数量；部署按这两个告警。

### 到某一刻执行一次：`delayx`

必须在某个时刻执行的函数（15 分钟后取消未支付订单、开赛封盘、一小时后重试派彩）是延时任务，不是 `time.AfterFunc`（进程重启就丢），也不是定时扫表（一分钟延迟、全表扫）。在需要它的那次业务变更的事务里 `rt.Delay.Schedule(ctx, tx, "order.cancel-unpaid", orderNo, time.Now().Add(15*time.Minute), payload)` 往本服务的 `delayed_task` 表写一行（项目迁移建表，`delayx.DDL` 是表结构）：订单和它的取消任务要么一起存在，要么都不存在。`app.Setup` 里每种 kind 注册一次函数 `rt.Delay.Handle("order.cancel-unpaid", func(ctx, t *delayx.Task) error {...})`；同一进程里的调度器（服务有 MySQL 且注册了 handler 时自动开，`delay.enabled: false` 关）认领到点的行（`FOR UPDATE SKIP LOCKED`，多副本分摊）并执行，延迟不超过一个轮询间隔（1s）。`rt.Delay.Cancel(ctx, tx, kind, key)` 撤掉还没执行的任务（订单付了）。至少一次（执行中崩溃会重跑），所以 handler 要幂等：先重读状态（"订单还没付吗？"），没事可做就返回 nil。返回错误的 handler 按退避重试（1m、2m、4m…1h）直到 `delay.max_attempts`（5 次），然后任务标 `failed`、记 `last_error`、计入 `delay_tasks_failed`（按它告警）；`delay_tasks_overdue_seconds` 是最早到点未执行的任务晚了多久，`delay_tasks_run_total{kind,result}` 是结果计数。已完成的行保留 `delay.retention`（7 天）。部署后检查 `delay`：有任务超时两分钟未执行或已彻底失败就不通过。`devkit lint` 的 `delay` 规则拒绝服务代码里的 `time.AfterFunc` / `time.NewTimer`。周期性的工作（每晚报表）是 job（`jobx`），不是延时任务。

一次要跑几分钟的工作（导出一张几十万行的表、一轮对账）也是延时任务，注册时加选项：`rt.Delay.Handle("export.run", h.OnExportRun, delayx.Long(), delayx.Timeout(30*time.Minute))`。`Long()` 的 kind 在长任务池里跑（每副本同时 `delay.long_workers` 个，默认 1），不占轮询协程，短任务不会排在它后面；池满时多出来的任务留在 pending 排队，由有空位的副本认领。`Timeout(d)` 代替 `delay.timeout`（60s）给这个 kind 单独的时限，到时 ctx 结束，重试与否仍看 handler 返回什么（导出超时想直接算失败，就在自己的任务行上记失败后返回 nil）。所有运行中的任务每 `delay.heartbeat`（30s）刷新一次行上的 `locked_at`，三个心跳没刷新的行（进程死了）才会被重新认领，长任务不会因为跑得久被别的副本重跑。`rt.Delay.Abort(ctx, tx, kind, key)` 终止一个任务：pending 的直接取消，running 的在本进程里立刻结束 ctx、在别的副本上在下一个心跳结束，handler 用 `context.Cause(ctx) == delayx.ErrAborted` 区分终止和超时。服务停机时正在跑的任务 ctx 结束（cause 是 `delayx.ErrShutdown`），任务放回 pending、`run_at` 改为现在、不计一次尝试，新副本起来立刻接着跑；handler 分块处理、每块看 `ctx.Err()`，被打断时就不会丢太多。测试里 `rt.RunDue()` 会等长任务跑完再返回。

### 唯一号 `idx`

注单号、订单号、流水号用 `rt.ID.Next()`：53 位整数，跨服务所有副本唯一、按毫秒有序、看不出数量。选 53 位是为了它到哪都精确：JSON 数字、JavaScript 的 Number（整数到 2^53 为止）、Go 的 int64、BIGINT 列；64 位雪花过了网关必须转字串，哪里忘了就丢精度，而我们的规模用不着那几位。高 41 位是 2026-01-01 起的毫秒（够到 2095），低 12 位按 `id.instance_bits` 分给副本数和每毫秒的号数（默认 5 + 7：每服务 32 副本，每副本每毫秒 128 个、每秒 12.8 万；副本少单量大的项目设 3 + 9；以后改分配也不会和旧号冲突，高位的时间把它们分开）。实例号在 Redis 里租（`idx:<服务>:<n>`，后台续租、退出释放），副本之间不会重；没有 Redis 用 `INSTANCE_ID`；两者都没有只有本机运行会退到主机名哈希（并告警），线上 `Next` 会报 `idx.ErrNoInstance`（panic，由 Recovery 变成一次失败请求），宁可失败也不发重号。网关 IDL 和 RPC IDL 一样写 `i64 bet_no`。`idx.Time(id)` 是生成时刻。号不是秘密：读注单的 handler 要校验它属于登录的 uid。没人引用的行（会员、角色）继续自增；生成器给会离开系统的号用。

### 报表 `starrocksx`

`report.enabled` 把 StarRocks 的 `report` 库打开为 `rt.Report`：业务表的 CDC 副本（MySQL binlog → Canal → Redpanda → Routine Load），延迟几秒，给报表、统计、大范围的后台列表和导出用。只有报表服务开它（`devkit lint` 的 `report-only` 规则，devkit.yaml 里 `report_service`）：它是副本，任何要据此做判断的读都不走它，那些读经拥有该表的服务查 MySQL。句柄只能查：`rt.Report.Query(ctx, &rows, "SELECT status, COUNT(*) AS n FROM member WHERE tenant_id = :tenant GROUP BY status")` 扫进结构体切片（列按 `db` 标签或字段名的 snake_case 对上）或标量切片，`QueryRow` 一行（没有是 `sql.ErrNoRows`）。每条查询必须用 `:tenant` 写租户，句柄填本部署的租户，没写的拒绝执行；有 `report.timeout`（30s）和 `report.max_rows`（10000，超了分页或聚合）。source platform 读 Nacos 里 infra.yaml 的 `starrocks` 段（只读账号 `report` 的密码来自那里指明的环境变量）。StarRocks 走 MySQL 协议，所以用 MySQL 驱动，测试也用 MySQL 顶替。指标 `report_queries_total{status}`、`report_query_duration_seconds`。

### 用 `centrifugox` 推送

`centrifugo:` 段：`enabled`，`source static` 的 `api_addr`、`api_key`、`token_secret`，或 `source platform` 从 Nacos 的
`infra.yaml` 取地址、从它指名的环境变量取密钥。句柄在 `rt.Centrifugo`。

- `rt.Centrifugo.UserChannel(uid)`：一个会员的频道名（`user:<uid>`），用它、不要自己拼字串
- `rt.Centrifugo.Publish(ctx, "user:42", data)`：推给订阅了这个频道的客户端，是 trace 里的一个 span
- `rt.Centrifugo.ConnectionToken("42")`：给登录后的客户端签连接 JWT（HS256，`token_ttl` 默认 24h），
  网关在登录响应里返回；Centrifugo 据此知道连接是谁，服务就能往 `user:<id>` 推
- `History(ctx, channel, n)`：取频道最近的消息（命名空间开了 history 时），测试和断线重连用

### 用 `s3x` 存文件

```yaml
s3:
  enabled: true
  source: platform      # dev / uat / prod：数据源表 kind=s3 那行（地址、两个桶、对外地址、密钥变量名）
  # source: static      # 本机：项目的一键环境 local/local.sh up --full（SeaweedFS 8333，两个桶都建好）
  # endpoint: "http://127.0.0.1:8333"
  # bucket: sportsbook-local-assets          # 私密桶：证件、凭证、导出；用 PresignGet 临时链接给出去
  # public_bucket: sportsbook-local-public   # 公开桶（谁都能读）：头像、banner、logo；直接给 URL
  # public_endpoint: "http://127.0.0.1:8333" # 浏览器能访问到的 S3 地址（默认同 endpoint）
  # public_url: ""                           # 公开桶前面有 CDN 时填它，替代 <public_endpoint>/<public_bucket>
  # prefix: ser-user    # 默认是服务名；两个桶里的 key 都在它下面
```

```go
// 私密文件（rt.S3）：只有服务和本人能看，客户端拿到的是带有效期的链接。
err  := rt.S3.Put(ctx, "kyc/42/front.jpg", file, "image/jpeg")
data, err := rt.S3.GetBytes(ctx, "kyc/42/front.jpg")                    // Get 是流式，Stat 看大小和类型，PutBytes 收 []byte
url, err  := rt.S3.PresignGet(ctx, "exports/2026-10.xlsx", 10*time.Minute)   // 客户端直接下载
url, err  := rt.S3.PresignPut(ctx, "kyc/42/front.jpg", "image/jpeg", 5*time.Minute) // 客户端直接上传，再把 key 提交上来
objs, err := rt.S3.List(ctx, "kyc/42", 100)
err  = rt.S3.Delete(ctx, "kyc/42/front.jpg")
// 公开文件（rt.S3.Public()，没配公开桶时是 nil）：浏览器直接显示的东西。
pub := rt.S3.Public()
url, err  := pub.PresignPut(ctx, "avatars/42.png", "image/png", 5*time.Minute)   // 上传链接；客户端 PUT 完把 key 提交上来
resp.AvatarUrl = pub.URL(row.AvatarKey)                                  // "http://s3.example.com/sportsbook-dev-public/ser-member/avatars/42.png"
```

一套部署两个桶，一个服务一个前缀（默认服务名，两个桶一样），服务之间碰不到彼此的文件；key 是 "avatars/42.png" 这样的路径。
**库里只存 key，不存 URL**（列名 `avatar_key`，不是 `avatar_url`）：URL 在写响应时按配置拼出来，换域名、换桶、加 CDN 不用动任何一行数据。
响应里给 `<名>_url`（公开文件用 `URL`，私密文件用 `PresignGet`），写请求里传 `<名>_key`。上传不经过网关（请求体限 1 MiB）：`PresignPut` 给客户端一个直接 PUT 的链接。
不存在返回 `s3x.ErrNotFound`。其他需求用 `rt.S3.Raw()` 拿 AWS SDK 客户端。
每次调用是一个 span（otelaws）、一条 debug 日志和 `s3_requests_total{op, status}` / `s3_request_duration_seconds`。
dev 的凭证是 SeaweedFS 的 admin 身份，放在 `app-datasources`（S3_ACCESS_KEY_ID / S3_SECRET_ACCESS_KEY）；正式环境给业务
单独建一个只能访问这个桶的身份。

### 用 `httpx` 调外部接口

```yaml
providers:
  odds-feed:
    base_url: "https://api.vendor.com"
    timeout: 3s                       # 单次尝试；默认 5s
    retries: 2                        # 默认 2；只对 GET/HEAD/OPTIONS/PUT/DELETE，且只在网络错误、429、5xx 时
    headers:
      X-Api-Key: "${ODDS_FEED_API_KEY}"   # 密钥在环境变量里，不在文件里
```

```go
odds := rt.Provider("odds-feed")        // 在 app.Setup 里取；配置里没有这个名字会在这里 panic
var out OddsResp
err := odds.GetJSON(ctx, "/v1/odds?match=123", &out)
err  = odds.PostJSON(ctx, "/v1/payout", req, &resp)   // POST 框架绝不重试
odds.R().SetContext(ctx).SetQueryParam(...)           // 其他需求：它就是一个 *resty.Client
```

认证也是配置，写在 provider 的 `auth:` 下：

```yaml
    auth: {type: bearer, token: "${ODDS_FEED_TOKEN}"}
    auth: {type: basic, username: "${PAY_USER}", password: "${PAY_PASS}"}
    auth: {type: oauth2, token_url: "https://id.vendor.com/token", client_id: "${PAY_CLIENT_ID}", client_secret: "${PAY_CLIENT_SECRET}"}
    auth: {type: hmac-sha256, secret: "${PAY_HMAC_SECRET}", header: X-Signature, payload: "{method}\n{path}\n{timestamp}\n{body}"}
    auth: {type: custom}        # 厂商私有算法：对接代码设一个 Signer
```

oauth2 是 client credentials 流程，token 缓存并在过期前刷新。hmac-sha256 对一个模板（{method} {path} {query} {timestamp} {body}）
签名，时间戳放在 `timestamp_header`（默认 X-Timestamp），`encoding` 为 hex 或 base64。都不合适的厂商用 `type: custom`，在 app.Setup 里
`rt.Provider("pay").UseSigner(paysig.Sign)`，`Sign(req *http.Request, body []byte) error` 是对接代码包里的一个普通函数，
只负责按厂商算法设请求头；每次尝试都会调用，body 已经替它读好。所有密钥都来自环境变量：部署把 `provider-secrets` 这个 Secret
注入每个服务，一个密钥一个变量，命名 `<厂商>_<字段>`（ODDS_FEED_API_KEY）；对接同事说需要哪些名字，运维填值。

每次调用下面有两道保护，裸用 `R()` 也一样。`max_concurrent`（默认 64）限制对一个 provider 同时在途的调用数，多出来的在自己的
超时内排队。熔断器在连续 `breaker_failures` 次失败（网络错误、429、5xx；默认 5）后打开，`breaker_open_for`（默认 30s）内
所有调用立刻以 `httpx.ErrCircuitOpen` 失败而不是等满超时；然后放一个探测请求过去，成功就恢复。状态变化有日志，
`http_client_breaker_state` / `http_client_rejected_total` 可以看。连接池每个 host 保留 100 条空闲连接
（Go 默认 2 条，高频拉取的供应商几乎每次都要重新 TLS 握手）。

一次调用最坏耗时是 (retries + 1) 次尝试、每次最多 `timeout`、加上中间的等待，默认值下约 16s；等不起的 handler 给 ctx 加
deadline。`debug: true` 把每次请求响应连头和 body 打到日志，只在本机（APP_ENV local）生效；部署环境忽略并告警。

每次调用都带 trace（client span `GET odds-feed /v1/odds`，请求头里有 W3C traceparent），写一条带 trace_id 的日志
（provider、方法、不含查询串的路径、状态码、耗时、第几次尝试；绝不记头和 body；错误文本里 URL 查询参数的值一律打成 `***`，因为厂商的 key 常放在那里，返回给调用方的错误也已打码；厂商要求放在 URL 其他位置的密钥用 `c.Secret(token)` 登记一次，日志、错误、span 里的 URL 一并打码），并计数
`http_client_requests_total{provider, http_method, status}` / `http_client_duration_seconds`。
非 2xx 的响应是 `*httpx.StatusError`，带状态码和 body 的前几百字节。业务 namespace 不能出网，所以有 `providers:` 的服务
必须部署到 `-provider` namespace：`NewRuntime` 读 `POD_NAMESPACE`，放错了拒绝启动并告诉你搬到哪。

### 用 `webhookx` 收回调

另一个方向：厂商调我们。规则写在 provider 的 `callback:` 下，handler 是一个函数，路由在 provider namespace 里某个 Hertz 服务的
`app.Setup` 注册（回调路径是厂商定的，不进 IDL）：

```yaml
providers:
  payment-x:
    base_url: "https://pay.vendor.com"
    callback:
      verify: {type: hmac-sha256, secret: "${PAYMENT_X_CALLBACK_SECRET}", header: X-Signature, payload: "{timestamp}.{body}", timestamp_header: X-Timestamp, max_skew: 5m}
      allow_cidrs: ["203.0.113.0/24"]          # 可选，厂商公布的来源地址
      event_id: {json: "data.id"}               # 或 {header: X-Event-Id}；重复投递靠它变成空操作
      dedupe_ttl: 168h                          # 默认 7 天
      archive_topic: callbacks.payment-x        # 默认值；"-" 关闭留档
```

```go
webhookx.Handle(rt, h, "payment-x", "/callbacks/payment-x/paid", func(ctx context.Context, ev *webhookx.Event) error {
	var n PaidNotice
	if err := ev.Decode(&n); err != nil { return err }
	return wallet.Credit(ctx, n.OrderID, n.Amount)   // 按订单号幂等
}, webhookx.WithReply(200, "application/json", `{"code":"SUCCESS"}`))
```

在 Kitex 服务（对接厂商的那个服务）上，收回调的 HTTP 监听由框架提供：配置 `callbacks: {enabled: true, addr: 8081}`，
`app.Setup` 里 `webhookx.Server(rt)` 返回引擎（trace、指标、请求日志、panic 恢复都已装好），`rt.Run` 在起 RPC 服务器之前
把它起来、退出时一起停。部署只把这个端口的 `/callbacks/` 开给公网。

```go
cb := webhookx.Server(rt)
webhookx.Handle(rt, cb, "payment-x", "/callbacks/payment-x/paid", callbacks.OnPaid)
```

每个回调依次经过：来源地址对照 `allow_cidrs`（403）、body 上限（413，默认 1 MiB）、验签（401；`verify.type` 为 hmac-sha256
对带时间戳窗口的模板签名、basic，或 `custom` 配 `webhookx.WithVerifier(fn)` 走厂商算法）、原始请求复制到 Kafka 留档主题
（留档失败回 500 让厂商重试，没留住的绝不处理）、事件 id 查 Redis（重复投递直接应答、不跑 handler）、然后是 handler：
返回 nil 就按路由的应答回（默认 200 "ok"，可 `WithReply`），返回 error 或 panic 回 500 并忘掉这个事件 id，厂商重试时会再跑。
每个回调一条带 provider 和 event_id 的日志，指标 `callbacks_received_total{provider, http_route, result}`。
handler 仍要幂等：厂商可能用两个 id 发同一件事。

### 服务之间的调用：超时、重试、熔断

`ClientOptions` 给本服务对其他服务的每次调用加上保护，默认值就是集群里该有的，`rpc_client:` 段只用来改它们：
单次调用 3s 超时（`timeout`）、建连 1s（`connect_timeout`）、重试 1 次（`retries`）但只在请求根本没发出去时
（没有实例、没有连接；超时或 handler 的错误不重试，因为那次可能已经执行了）、按被调服务和方法熔断（`breaker: false` 关闭）：
至少 200 次调用里错误超过一半就立刻以 `kerrors.ErrCircuitBreak` 失败，直到下游恢复。写操作要重试的话由调用方自己带同一个
request_id 再调，幂等中间件会回放结果。

### 按调用方放行（`callers:`）

动钱、持密钥的服务只让该调它的服务调它：`callers.rules` 每条列出一组方法和允许的调用方服务名（Kitex 客户端在 TTHeader 里带的服务名，就是请求日志里的 `from`）。写进规则的方法只接受列出的服务和本服务自己（部署后检查用自己的客户端），没带名字的调用在这些方法上被拒；没写进任何规则的方法谁都能调。被拒回业务码 1007（`kitexx.CodeForbidden`），日志一条 WARN，指标 `rpc_server_rejected_total{rpc_method,reason="caller"}`。中间件排在请求日志之后、参数校验之前。

```yaml
callers:
  rules:
    - methods: [DebitBet, CreditSettle, ReverseBet]
      allow: [ser-bet]
    - methods: [ManualAdjust, ReviewApproval, ClearVenueBonus]
      allow: [ser-admin-api]
```

### RPC 服务过载保护（`limits:`）

Kitex 服务的一个实例拒绝超出能力的请求，让过载变成快速的"忙"而不是一堆慢请求：同时处理不超过 `limits.max_in_flight`（默认 1000），每秒不超过 `limits.max_qps`（默认关，服务知道自己容量后再开；按 100ms 窗口计）, 连接不超过 `limits.max_connections`（10000）。被拒的请求不执行，回业务码 5003（`kitexx.CodeBusy`），网关像别的码一样透传，调用方稍后重试；指标 `rpc_server_rejected_total{rpc_method,reason}`，日志每分钟一条 WARN。Kitex 自带的限流器只用来限连接：限 QPS 时它直接断连接，客户端分不出是过载还是崩了（`devkit lint` 拦 `server.WithLimit`）。中间件排在链的最前面，拒绝一次不花别的代价。

### 网关防护（`guard:`）

API 服务在 handler 之前就拒绝一些请求，不配置也有默认值：请求体超过 `guard.max_body_bytes`（1 MiB）读之前就 413；每个请求通过 ctx 带 `guard.timeout`（10s）的截止时间，它发起的 RPC 调用到时放弃，到时还没产生响应的回 504、code 1010；一个客户端 IP 对本服务每个窗口最多 `guard.rate.per_ip` 个请求（默认 "600/m"，也可 "20/s"、"5000/h"、"off"），IDL 里标了 `// @limit 10/m` 的接口（apidoc 写成 x-rate-limit）再按 IP 单独限一次——登录、注册、改密码、验证码这些接口 `devkit lint` 的 `limit` 规则要求必须标。超限回 429、code 1008、带 Retry-After。有 Redis 时按固定窗口在 Redis 里计数（副本共用），没有就在内存里。限流故意放在登录校验之前，猜密码也受限；`/ping`、`/docs` 不限。指标 `http_requests_rejected_total{reason}`（rate_limit、timeout）。在代理后面客户端 IP 只采信可信代理转发的（见下节 `client.trusted_proxies`）。

### 客户端设备与 IP（`client:`）

请求的客户端是什么，是调用方的属性而不是某个操作的参数，所以走请求头和 context，不进请求体：`hertzx.Client`（`New` 装上，排在限流之后）从 `c.ClientIP()` 取 IP：连接方不是可信代理就用连接地址；是可信代理就从 `X-Forwarded-For` 右往左取第一个不是可信代理的地址，客户端自己带的转发头只会出现在左边，伪造无效；不读 `X-Real-IP`。可信代理 = 内网（10/8、172.16/12、192.168/16、fc00::/7）与本机，加 `client.trusted_proxies` 列的网段：服务前面有 CDN（如 Cloudflare）时把它的网段列上，否则取到的是 CDN 节点地址。hertz 默认信任一切、取最左边的值，经 Cloudflare 填办公室 IP 就能冒充（2026-10-21 实测，v0.61.0 改）。webhookx 的回调监听用同一个规则，从 `X-Device-Type` 取设备类型、`X-Device-Id` 取设备号，写成 metainfo 持久值（`CLIENT_IP`、`DEVICE`、`DEVICE_ID`），和 uid 一样跨每一跳 RPC。网关和 RPC 服务的任何 handler 用 `kitexx.ClientIP(ctx)`、`kitexx.Device(ctx)`、`kitexx.DeviceID(ctx)`（或 `kitexx.ClientOf(ctx)`）读；请求体里不许再有 IP 或设备字段。

```yaml
client:
  device:
    required: true                               # 没带合法 X-Device-Type 的请求回 code 1001
    types: [PC, H5, Android, iOS, Telegram]      # 允许的取值，匹配不分大小写，存的是这里的写法
```

没配 `types` 就完全不读这个头（后台网关）；配了，值不在列表里的不管是否 required 都回 1001（`X-Device-Type: 不合要求`），`required` 时没带也回 1001（`X-Device-Type: 必填`）。`/ping`、`/docs`、`/openapi.*` 永远不拒。`X-Device-Id` 可选，1 到 128 位 `[A-Za-z0-9._:-]`，别的拒绝。请求日志和这次请求的每条 RPC 记录都带 `device`。`ServeDocs` 把这两个头写进对外提供的 OpenAPI 文档（一个参数组件，每个接口引用，取值是 enum），前端生成器看得到；`/docs` 页默认带第一个取值。测试：网关测试用 `testx.Device("iOS")` / `testx.DeviceID(id)` 两个请求头，RPC handler 测试用 `kitexx.WithClient(ctx, kitexx.Client{Device: "iOS", IP: ...})` 扮演某个设备的请求；部署后检查把 `X-Device-Type` 放进 `verifyx.Gateway.Headers`。

### 接口文档（`cmd/apidoc`、`hertzx.ServeDocs`）

API 服务的文档从 IDL 生成。`make gen` 对服务的 IDL 目录跑 `go run github.com/sezznaw/devkit-common/cmd/apidoc -dir <idl>/<服务>/`：
目录里每个声明了 `service` 的文件（一个领域一个）先给没写注解的结构体字段补上 `api.body`（我们的约定是 POST + JSON），再用 thriftgo 加
CloudWeGo 的 `thrift-gen-http-swagger` 插件生成，然后合并（schema 名加 `<namespace>.` 前缀，两个领域都有 `ListReq` 不会撞；同一条路由出现在两个文件里报错），
写到 `cmd/<服务>/openapi.yaml`，`main.go` 把它嵌进二进制。`-idl <文件>` 仍可只生成一个文件。开了 `docs.enabled`，服务就提供 `/openapi.yaml`、`/openapi.json` 和
`/docs`（Scalar 文档页，"Send request" 直接打本服务）。

方法的 IDL 返回类型就是数据结构体；apidoc 给每个响应包上网关要加的 `{code, msg, data}` 壳（见下一节），返回 `common.Empty` 的方法合成一个 200，
没写 `optional` 的字段标成 `required`，`// @limit` 写成 `x-rate-limit`，`idl/errors/*.thrift` 的码表写成 `x-error-codes`（code、name、message、domain）给前端生成器。
IDL 里方法和字段的注释就是文档说明：方法注释的第一句是接口标题，其余是说明。`// @docs` 行（`// @docs title: ...`、`// @docs description: ...`、`// @docs tag member: 会员`）
放在一个没有 `service` 的文件里，约定叫 `docs.thrift`；`@public`、`@perm`、`@audit`、`@limit` 从全部文件读。接口按路径第二段 `/v1/<领域>/...` 分组，`/ping` 归到"系统"。
local 和 dev 开，生产关。

### 响应壳与分页（`hertzx.OK`、`pagex`）

网关方法的 IDL 返回类型就是数据结构体（没有数据用 `common.Empty`）；handler 以 `hertzx.OK(c, &data)`（`hertzx.OK(c, &common.Empty{})` 回没有 `data` 的壳）
或 `hertzx.Fail(ctx, c, err)` 结束：`kerrors.BizStatusError` 变成它的 code 和 msg，其他错误连 trace 记日志并回 5001。网关自己定的码用 `hertzx.FailCode(c, code, msg)`；
`hertzx.CodeOf(ctx, err) (int32, string)` 是 `Fail` 用的那套映射，给只想拿码、不想在这里应答的 handler。`{code, msg, data}` 的壳只在这里加一次，IDL 里不写，
`devkit lint` 的 `no-envelope`、`envelope-write` 守着。

列表请求带 `common.PageReq {page, page_size}`，响应在 `items` 旁边带 `common.PageInfo {page, page_size, total}`（`idl/common/common.thrift`）；
`page, size, offset := pagex.Normalize(req.Page)` 统一默认值（第 1 页、每页 20、最多 100），默认值只在这一处。lint 规则 `paging` 拦裸的 `page` / `page_size` / `total` 字段。

### 错误码与消息（`cmd/errgen`）

码只在 IDL 仓库声明一次：`idl/errors/<领域>.thrift`（`namespace go errcode`，每个 `const i32 名字 = 码` 上一行一句说明）。`make gen` 先用 thriftgo 把它们编成
`kitex_gen/errcode`，再跑 `go run github.com/sezznaw/devkit-common/cmd/errgen -dir <idl>/errors -out kitex_gen/errcode`，生成 `zz_messages.go`：
`Message(code) string`、`Err(code) error`、`Errf(code, detail string) error`（消息是那句说明，Errf 把 detail 用冒号接在后面的 `BizStatusError`；不是 printf 风格，细节自己拼好）。
服务写 `return nil, errcode.Err(errcode.MemberNotFound)`，不写消息字符串（lint 规则 `error-msg`）；网关的 `Fail` 把它变成壳，前端从 `x-error-codes` 显示同一句话。

### 金额 `moneyx`

金额是货币主单位的十进制小数加 ISO 4217 货币码：`moneyx.MustParse("12.34", "USD")`。不是"分"的整数——钱在系统每个边界上都是小数（厂商回调、支付、玩家、流水、报表），整数要在每个边界换算一次、每次都得知道货币小数位；也不是浮点——浮点存不下 0.1。运算交给 shopspring/decimal（唯一依赖），`moneyx` 补货币、货币小数位（USD 2、JPY 0、KWD 3、BTC 8；`RegisterCurrency` 可加）、舍入策略和 IDL / JSON / GORM 的形状。完整理由见包注释。

IDL 侧是 `common.Money {string amount, string currency}`（项目的 `idl/common/common.thrift`）：`m, err := moneyx.Of(req.Stake)` 转进来并拒绝超出货币小数位的输入；`a, c := m.Parts()` 回去。JSON 是 `{"amount":"12.34","currency":"USD"}`，字符串，JavaScript 不会碰到浮点。比例（赔率、费率、汇率）是 `common.Decimal`，字符串，Go 里是 `decimal.Decimal`。

`Add`、`Sub`、`Cmp`、`Equal`（别用 `==`，decimal 的 `==` 比的是内部结构）精确且要求同货币。小数只在一处出现：`m.Mul(rate, mode)` 按指定 `Rounding` 舍入一次到货币小数位（`RoundHalfUp`、派彩用 `RoundDown`、手续费用 `RoundUp`、`RoundHalfEven`）；多步公式中间用 `m.Amount` 走小数，最后 `FromDecimal(d, 货币, mode)` 回来一次。`Split(n)`、`Allocate(70, 20, 10)` 分出加起来正好的几份。GORM 存两列：嵌入并加前缀 `Balance moneyx.Money \`gorm:"embedded;embeddedPrefix:balance_"\``，迁移里 `balance_amount DECIMAL(24,8)`、`balance_currency CHAR(3)`。`devkit lint`（`money-type`、`rate-type`、`no-float-money`）拒绝金额和比例字段的其他类型。

### 版本号

每条日志和 span 带的 `version`：配置里的 `log.version`，否则环境变量 `APP_VERSION`（部署把镜像 tag 注进来），否则编译进二进制的 git 提交号（构建时工作区有改动会带 `-dirty`，CI 的 `go mod tidy` 可能导致；用环境变量就不会）。

### 用 `authx` 管登录态

```yaml
auth:
  enabled: true
  secret: "${AUTH_JWT_SECRET}"   # 认证服务和每个网关同一个值；至少 32 字符
  realm: member                  # 网关填：服务哪类主体（member | admin | agent，或项目自己的领域名，小写字母数字下划线）
  access_ttl: 15m
  refresh_ttl: 720h
  single_session: true           # 新登录顶掉旧会话
```

一个服务负责所有人的认证（`ser-auth`）：通过账号所属的服务校验密码（会员是 ser-member），用
`authx.NewIssuer(rt.Config.Auth, rt.Redis)` 做 `Login`、`Refresh`（旧 refresh 作废；再用它就是泄露，吊销该主体全部会话）、
`Logout`（结束会话、拉黑 access）和 `RevokeAll(ctx, realm, uid, reason)`（改密码、封号、后台踢人；reason 是项目自己定的一个词，如
`disabled`、`password_reset`，说明为什么吊销）。会话数据在它的 Redis 键里（`auth:*`）。

网关只校验、不按请求调 ser-auth：`authx.NewVerifier(rt.Config.Auth, rt.Redis)` 本地验签名和声明，再读一次 Redis
（会话是否还活着、令牌是否被拉黑）；`hertzx.RequireLogin(h, verifier, openAPI, publicPaths...)` 把它装到所有接口上，除了 IDL 里标了
`// @public` 的（apidoc 不给它们加安全要求）和框架自己的路径（/ping、/docs、/openapi.*）。没带令牌回 HTTP 401
`{code: 1004}`，会话已失效回 `{code: 1005}`，并在 `data.reason` 里说明为什么：`replaced`（被同一账号的新登录顶掉）、`logout`、
`refresh_replayed`（库自己写的三种），或 `RevokeAll` 传入的词；前端按它给出「此账号已在其他位置登录」「账号已停用」这类准确提示，而不是一句笼统的「请重新登录」。身份随上下文走：网关 handler 和它调用的每个 RPC 服务里 `kitexx.UID(ctx)`（登录校验已经跑过的地方用 `kitexx.MustUID(ctx)`；`kitexx.Realm(ctx)` 取领域）
都能拿到，`uid` 永远不再是请求字段。授权（能做什么）不在这里。

### 后台操作审计 `hertzx.Audit`

后台网关记录谁做了什么：`hertzx.Audit(h, openAPI, sink)`（app.Setup 里装在 RequireLogin 之后）对每个被审计的接口每次调用记一条 `AuditEntry`，不论成功失败：时间、realm 与 uid、IDL 里的接口标题、方法、路径、权限点、请求体和响应体（JSON 任意层级含数组、表单都打码：password、token、key、card、证件号、银行账号等键的值换成 ***；其他格式不记内容只记大小；各最多 4 KiB，在字符边界上截并以 `hertzx.AuditTruncated` 结尾）、HTTP 状态与业务码、trace id、来源 IP、User-Agent 与客户端送来的设备号（`X-Device-Id`，经 Client 中间件）、耗时。哪些接口记：IDL 方法注释 `// @audit` 一定记（登录），`// @noaudit` 不记，其余看权限点——有且不以 `.view` 结尾就记。handler 不写任何东西。sink 决定记到哪；`hertzx.NewAsyncSink(store)` 把"存一批"的函数（调账号服务的 RPC、写表）变成不阻塞请求的 sink：队列 1000、每秒或满 100 条刷一次、失败整批重试一次后逐条存（一条被拒不连累同批）、丢弃记日志并计入指标 `audit_entries_dropped_total{reason}`；用 kitexx.OnShutdown 注册 Close 以便退出前刷完。

### 用 `metricsx` 出指标

```yaml
metrics:
  enabled: true     # dev / uat / prod 打开；本机关掉（两个服务会抢端口）
  addr: 9091        # 默认值，服务 chart 抓的就是这个端口
```

不用写代码：开了 `metrics.enabled`，框架在这个端口提供 `/metrics`（和 `/healthz`），与服务自己的端口分开，
所以永远不会从 ingress 暴露出去。记录的指标：

- `rpc_server_requests_total{rpc_service, rpc_method, code}` 和 `rpc_server_duration_seconds`：处理的每个 RPC；
  `code` 是 `ok`、`BizStatusError` 的业务码（码表有限，在 idl 仓库）或 `error`
- `rpc_client_requests_total` / `rpc_client_duration_seconds`：本服务发出的调用，按被调服务分
- `http_server_requests_total{http_method, http_route, status}` / `http_server_duration_seconds`（Hertz）：
  route 是 IDL 里的模式，不是原始路径；没匹配到路由的请求记为 `unmatched`
- `go_sql_*`（MySQL 连接池，按库）、`redis_commands_total{cmd, status}` / `redis_command_duration_seconds`、
  `kafka_events_published_total`、`kafka_events_handled_total{topic, result}`（`ok`、`retried`、`dlq`）、
  `kafka_records_produced_total`、`kafka_records_fetched_total`、`kafka_broker_connects_total`，以及 Go 运行时和进程指标

服务自己的指标：包顶层 `promauto.With(metricsx.Registry).NewCounter(...)`；Prometheus 默认注册表不会被提供。
端口被占用是启动时的一条告警，不是启动失败。

### 从本机接管 dev 的服务（`devroute`）[takeover]

`devkit takeover <服务>` 让开发者在 IDE 里（带断点）跑的实例，替 dev 环境处理带 `X-Dev-Tag: <tag>` 的请求，其余请求照常走 dev 自己的实例。框架这边是 `devroute`，只在 `APP_ENV=dev`（或下面的接管模式）打开；uat、prod 里它不在请求路径上，测试锁住了这一点：网关忽略请求头、没有第二个 Nacos 客户端、不换负载均衡器、超时不动。dev 里：`hertzx` 把 `X-Dev-Tag`（校验 `^[a-z0-9-]{1,32}$`，逗号分隔多个）写成 metainfo 持久值 `DEV_TAG`，和 uid 一样跨每一跳；Kitex resolver 合并 `DEFAULT_GROUP` 和 `DEV_TAKEOVER`（第二个 nacosx 客户端，相信空列表，Diff 能看见 tag 变化）；`devroute.NewBalancer` 把带 tag 的请求送到 `dev-tag` 元数据相同的实例（按 tag 顺序第一个匹配，多个同 tag 轮询），其余交给 Kitex 默认的加权均衡器、只在基线实例里选，绝不选接管实例；带 tag 请求的 RPC 超时和网关的 `guard.timeout` 换成 `devroute.debug_timeout`（30 分钟）。请求日志带 `dev_tag`。

接管模式是本地实例连 dev 的方式：`devkit takeover` 运行期间维持 `<服务>/.devkit/takeover.json`（5 秒心跳，服务认 15 秒内的），并用一条 ssh 把集群里的地址全部打通。不设 `APP_ENV` 启动的服务发现属于自己的新鲜标记（`config.Takeover`），就读 `conf/<标记里的 env>.yaml`、拿到部署的环境变量、把 `platform` 来源解析出的所有地址（MySQL、Redis、Kafka 的 broker 及其公布的地址、Centrifugo、S3、StarRocks、OTLP）改写到隧道口（`config.RewriteAddr`），下游服务解析到其 ClusterIP 的隧道口，**不注册**，也不起任何后台工作者：不消费（否则和 dev 实例争分区）、不转发 outbox（本地写进共享表的行由 dev 实例转发）、不轮询延时任务、不开 metrics 端口，也不改表结构（`rt.AutoMigrate` 只打印差异，框架表不建）。启动日志第一行是 `TAKEOVER MODE`。`testx` 关掉标记检测，测试永远不会碰到正在进行的接管。可选的 `devroute:`（`enabled`、`debug_timeout`）覆盖 APP_ENV 规则，不进模板。

### 部署后验证 `verifyx`

每次发布后，部署用新镜像再起一次 `<二进制> --verify`（ArgoCD 的 PostSync Job）：框架的检查和服务自己的检查对刚上线的部署跑一遍，失败就是 Job 失败，几分钟内触发告警。框架按配置检查：MySQL 能应答且迁移不是 dirty、Redis 能应答、outbox 没有超过两分钟还没发出的事件、Nacos 里至少有一个本服务实例、网关的 `VERIFY_BASE_URL/ping` 能应答。服务在 `app/verify.go`（`Checks(cfg, rt) []verifyx.Check`）加自己的几条：用测试账号登录读一条、调一个 RPC。检查必须对线上无害：只读，或对测试账号自己数据的幂等写。`verifyx.NewGateway(rt.GatewayURL())` 是网关调自己接口的客户端（`Get`、带响应壳的 `Post`、登录后带 Bearer）。`make verify` 在本机对本地环境跑同一套。失败的检查每 10 秒重试一次、持续两分钟才算失败（Job 在滚动刚健康时就开跑，外面的世界可能慢几秒）；每次重试记一条 `verify retrying`。

### 发布不用排顺序：依赖闸门（`idl.deps`）

一个改动横跨几个服务（新 RPC 方法加调用它的网关）时，结果曾取决于各条流水线谁先跑完：网关先于 RPC 服务上线，就回 5001、验证失败。框架把顺序去掉了。`make gen` 在 `idl.lock` 旁写 `idl.deps`：本服务和它调用的每个 RPC 服务的 **IDL 代数**——IDL 仓库里改过该服务目录的提交数（截至生成代码的那个提交，沿主干单调递增）。`main.go` 把它嵌进二进制（`kitexx.SetIDLDeps`）。每个实例把自己的代数注册进 Nacos 元数据 `idl.gen`（另有 `idl.deps`：它调用的服务名单）；服务监听之前，`kitexx.WaitIDLDeps` 等到它调用的每个服务都至少有一个可用实例、且全部实例的代数不低于生成客户端时的代数。部署的 startupProbe 让等待中的 Pod 既不进 Service 也不被杀，旧 Pod 继续服务；依赖的版本一到，闸门几秒内打开，验证在之后才跑（并再查一次，检查名 `idl-deps`）。依赖一直不来，滚动就停在那里，部署在 progress deadline 后报出来；日志 `deps gate waiting` 写明等哪个服务、哪个实例、两边的代数。本机、没有 Nacos、`DEPS_GATE=off` 时闸门关闭；没有 `idl.gen` 的实例（在 `idl.deps` 之前构建的）不等，把本服务列为自己依赖的服务也不等（互等会等到天荒地老）。`devkit lint`（规则 `idl-deps`）在 `idl.deps` 缺失或没随 `idl.lock` 重新生成时报错。

### 用 `testx` 写测试

handler 测试先建一个和框架交给 `app.Setup` 一样的 Runtime，底下是只活一个测试的设施：`rt := testx.New(t, "order", testx.WithMySQL(testx.MigrationsDir()), testx.WithTopics("events.order"))` 给出进程内 Redis（miniredis，`rt.MiniRedis` 可直接看）、进程内 Kafka（kfake）、记录推送内容的假 Centrifugo（`rt.Pushes()`、`rt.WaitPush(channel, timeout)`），加了 `WithMySQL` 还有一个按项目 `*.up.sql` 迁移建出来、测完就删的库。库是唯一不在进程内的：来自 `MYSQL_TEST_DSN_ROOT`（模板 Makefile 指向本机一键环境，CI 指向 MySQL 服务容器），连不上就跳过而不是失败。然后 `h, _ := app.Setup(&app.Config{Config: rt.Config}, rt.Runtime)`，用 `testx.Ctx(uid)`（已登录会员；其他 realm 用 `CtxAs`；绕过真实登录的路由测试用 `kitexx.WithIdentity` 塞身份）调 handler。`rt.Start()` 跑服务器在 Setup 之后会跑的东西（订阅的消费者、outbox 转发器、延时任务轮询）；`rt.RunDue()` 不等时间直接执行到点的延时任务（先把 `run_at` 改到过去）；`rt.WaitEvent(topic, type, timeout)`、`rt.Events(topic, timeout)` 读主题里到了什么。网关用 `testx.Post(t, h, "/v1/x", body, testx.Bearer(token))`、`testx.Get` 进程内调路由，拿到解析好的响应壳（`MustCode(t, 200, 0)`、`Decode(t, &out)`）；`testx.From(ip)` 设 guard 计数用的客户端 IP。`devkit lint` 的 `tests` 规则提醒 handler 包一个测试都没有的服务。

### 用 `jobx` 写定时任务

一个任务就是一个带名字和 cron 时间表的函数，列在服务的 `app/jobs.go` 里（同事只碰这一个文件，时间表和代码放在一起）：

```go
func Jobs(rt *kitexx.Runtime) []jobx.Job {
	r := repo.New(rt)
	return []jobx.Job{{
		Name:     "reconcile-bets",
		Schedule: "*/5 * * * *",          // 五段式，UTC
		Timeout:  10 * time.Minute,
		Run:      func(ctx context.Context) error { return reconcile.Run(ctx, r) },
	}}
}
```

剩下的由生成的 `main.go` 负责：`<二进制> --list-jobs` 不读任何配置，直接把清单打成 JSON
（`{"jobs":[{name, schedule, timeout_seconds}]}`），CI 把它和镜像 tag 放在一起，部署为每个任务生成一个
Kubernetes CronJob；`<二进制> --job=<名>` 就是 CronJob 跑的命令：框架打开配置里启用的东西（数据库、Redis……），
跑完这个任务就退出，不起服务器、不注册 Nacos、不拉消费者。失败退出码为 1，Kubernetes 据此记一次失败。

一次运行得到：根 span `job <名>`（慢任务在 Grafana 里是一张瀑布图，里面的 SQL 和调用都是它的子 span）、每条日志都带
`job`、`run_id`、`trace_id` 的 logger（`zlog.Ctx(ctx)`）、结束时一条带结果和耗时的记录、Redis 里的锁 `job:<服务>:<名>`
保证同一任务不重叠（没开 Redis 则告警后照跑，CronJob 的 `concurrencyPolicy: Forbid` 仍挡住按时间表的重叠）、到超时即取消的
context、panic 变成一次失败。每次启动都校验清单（名字符合 Kubernetes 命名、不重复、时间表能解析），写错是启动失败，
不是凌晨三点的惊吓。

函数的规矩：幂等且可续跑（一次运行可能被重试），分块处理、每块提交，块之间看 `ctx.Err()`，不要相信"上次运行时间"，
水位从数据里读。

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
- **服务一启动就能看到日志是怎么配置的。** `Init` 的第一条记录 `logger configured` 会列出实际生效的全部设置，名字与配置文件里的一致：没写的项显示默认值（控制台里标注 `(default)`），写错的项显示回落后的值。它不受日志级别影响，配置写错时的告警也一样；JSON 格式下这些设置放在一个对象里：`"config": {...}`。由 `kitexx` 代填的值会注明来源：`service: order (from service.name)`、`env: prod (from APP_ENV)`，或 `env: local (APP_ENV is not set)`；JSON 格式下这些说明放在 `"config_notes": {...}` 里。
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
