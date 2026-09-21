# zlog 待办清单（2026-09-20 评审）

状态：`[ ]` 未做　`[x]` 已做并有测试　`[-]` 不做（附原因）

## 一、不做就用不起来

- [x] 1. kitexx 改用 zlog：配置结构、请求日志中间件、Kitex 内部日志（klog）桥接；删除旧的 `log` 包；更新服务模板
  - 结果：kitexx 的配置、中间件、klog 桥接、shutdown 日志全部改用 zlog；旧 `log` 包已删除；服务模板已在 devkit-registry 的 `feat/zlog` 分支更新（待 common 发布 v0.3.0 后才能发布）
- [x] 2. 请求上下文：`zlog.Ctx(ctx)`、往 ctx 里加字段、`trace_id` 的生成与跨服务传递
  - 结果：`zlog.Ctx` / `CtxWith` / `NewCtx`；kitexx 中间件生成或沿用 `trace_id`，经 TTHeader 跨服务传递（用两个真实 Kitex 服务端到端验证过）
- [x] 3. 接管第三方库的日志：标准库 `log` 和 `log/slog` 的输出统一走 zlog
  - 结果：`Init` 把 `log/slog`（连带标准库 `log`）接到 zlog，通过了标准库自带的 slogtest 一致性测试

## 二、实测确认的问题

- [x] 4. 业务字段之间重名（`With` 的 `uid` 与调用处的 `uid`）导致 JSON 出现重复 key
  - 结果：同名字段保留后一个（调用处覆盖 `With`），JSON 不再有重复 key
- [x] 5. `zlog.Sync()` 输出到 stdout 时返回无害的假错误
  - 结果：忽略终端/管道不支持 sync 的假错误，真实错误照常返回
- [x] 6. `Init` 之前的日志在生产环境格式不对（控制台彩色文本混进 JSON 流）
  - 结果：`Init` 之前的 logger 由 `ZLOG_FORMAT`、`ZLOG_LEVEL`、`ZLOG_SERVICE`、`ZLOG_ENV` 配置；模板的 Dockerfile 已设置；模板的 `fatal()` 改走 zlog
- [x] 7. 多行的字段值（带堆栈的 error、panic 堆栈）在控制台挤成一长行
  - 结果：多行的值显示在记录下方的缩进块里，堆栈每一行可点击

## 三、缺的能力

- [x] 8. 打日志前判断级别是否开启（避免白白计算昂贵的参数）
  - 结果：`zlog.Enabled(zlog.LevelDebug)`
- [x] 9. ERROR 级别自动附带调用堆栈（可配置，默认关）
  - 结果：`stacktrace: error`，默认关；JSON 里的键是 `stack`
- [x] 10. 标识进程的字段补全：`env`、`host`、`version`
  - 结果：`env`、`host`（主机名）、`version`（默认取构建时的 VCS 修订号）；都已加入保护名单
- [x] 11. 日志洪峰保护（采样）与写缓冲
  - 结果：`sampling: {first, thereafter}` 与 `buffer: {size, flush_interval}`，默认都关
- [x] 12. 字段值与消息的大小上限（可配置，默认关）
  - 结果：`max_field_bytes`，默认不限制；按字符边界截断并注明原长度；模板 prod.yaml 设为 8192
- [x] 13. 敏感信息：`zlog.Secret`
  - 结果：`zlog.Secret(key, v)` 输出 `***`；文档里提醒了 `Any(req)` 的风险
- [x] 14. 测试辅助：在单元测试里捕获 / 静音日志
  - 结果：新包 `zlog/zlogtest`：`Capture(t)`、`Discard(t)`；另有 `zlog.SetDefault`
- [x] 15. 运行时调级别接入 Nacos 配置中心
  - 结果：`log_level_data_id`：级别跟随 Nacos 配置；设置失败只告警、不影响启动（用假的配置客户端测试，未连真实 Nacos）

## 四、小瑕疵

- [x] 16. `Fatal` 没有自动化测试
  - 结果：用子进程测试：输出日志、刷出缓冲、退出码为 1
- [-] 17. 控制台里多行的消息占多行（只影响人看）
  - 不改：多行的**字段值**已在第 7 项处理；多行的**消息**保持原样。消息是给人看的正文，原样换行最好读；JSON 格式下始终是一行，采集不受影响
- [x] 18. `err` 字段在控制台默认显示为红色
  - 结果：`Err` 默认红色，可用 `.Yellow()` 等覆盖
- [-] 19. JSON 里 `level` 排在 `time` 前面
  - 不做：这个顺序是 zap 的 JSON 编码器写死的，要改只能自己重写整个 JSON 编码器；采集系统不关心字段顺序，不值得
