# Functions 客户输出

0.55.0 / migration 044 本地基础已完成真实 Guest/UI 验收，见[精确交付](DELIVERY-2026-10-18-FUNCTION-OUTPUT.md)。性能、压力、可靠性和 HA/DR 专项按用户要求后置。

Functions 页面提供独立的“客户输出日志”。中央采集默认关闭；项目编辑者勾选敏感输出确认后显式开启。授权包括项目内可读成员共享访问、三天保留和最近 10,000 行容量限制。设置需要独立的精确策略版本、CSRF 和持久 Idempotency-Key；版本首次为 0。响应不确定时 UI 保留原键、版本和输入，明确重放原设置。禁用不唤醒或调用函数，停止后续持久化，既有输出仍按保留期可读。重新开启从新的确认时间开始，停采期间的 Guest 缓冲不回填。

运营开关 `functions.outputLogs.enabled` 默认 false，需要支持 output_logs capability 的已验证 Guest。Guest 的 root supervisor 独立接收 stdout/stderr，以固定实例、部署、项目/分支、启动 boot、代次、序号和时间归属；客户提供的 JSON、级别或文本不能声明为受信任平台记录。console.debug/log/info/warn/error 映射 DEBUG/INFO/INFO/WARN/ERROR；原始 stdout 为 INFO，stderr 为 ERROR。普通日志 scope 是 neon.function.app，root 在真实 HTTP/WS 派发前后生成 neon.function.request 的 invoke begin/end，不记录 URL、查询、请求头或请求正文。

root 在内存中仅保留最近 512 行；单行最多 4,096 字节，超长行整行替换并标记，不保留可能截断的秘密片段。已知环境变量值、完整注入的数据库 URL、数据库密码及其常见 URI 编码在 Guest 内替换为 [REDACTED]，然后才经固定 TLS/HMAC 管理通道发送。替换是已知值匹配，不能识别用户自行打印的任意、派生或分段秘密，因此必须明确确认且不应打印敏感信息。客户输出始终是应用内容，不用于安全、计量、资源归还或生命周期判断。

只有已授权采集的已发布 active 实例被后台读取；读取日志本身不执行客户代码，不增加活动调用计数，不唤醒 Functions/SQL。身份校验复用原 CA、Service/VM UID 和 boot/generation。每页最多 16 条且序列化有界，掉出 Guest 缓冲的行明确计入 dropped_lines。中央游标绑定原实例/boot，事务去重和当前策略锁防止并发重复及停采后的写入。捕获的正文用域分离 AES-GCM 加密并绑定 Function/instance/boot/sequence，当前项目权限校验后才解密返回。

中央保留三天并有最近 10,000 行硬容量，后台仅清理这些已确认可过期的客户日志密文，不选择对象字节、代码 artifact、请求/操作历史。正常停止先做有界采集，再由同一次认证停止响应返回最终尾日志（包括刷新后的未换行输出）；并发采集已保存的前缀会被去重，只持久化余下的新记录。原实例退休后的已保存记录仍可查询。采集为有界尽力交付；缓冲溢出或未采集的突然实例退出不能被表述为完整日志。失败启动尚未发布的实例、完整外部日志平台、跨服务统一检索/计费和 HA 外部持久尾流不由本片代替。

UI 支持级别过滤、稳定分页、明确的最新跟随、对已加载文本筛选，以及导出已加载 JSON。后两者明确限于当前已加载数据。实例、部署和 scope 可逐行检查；React 以文本渲染客户内容。API 为 GET/PATCH `.../functions/{slug}/output-policy` 和 GET `.../functions/{slug}/output-logs`；既有 request-logs 保持独立。删除整个函数定义后不再通过该旧 slug 读取，日志按既定窗口过期，避免将旧定义的输出交给后来复用的 slug。

精确实现见 `api/internal/functions/output.go`、`api/internal/control/function_output*.go` 和 `web/src/features/functions/FunctionOutput.tsx`。原生验收例子 `examples/functions/output/index.mjs` 只使用人工测试值证明替换；Linux UI 套件 `functions-output.spec.ts` 不替代后置的性能和可靠性门槛。
