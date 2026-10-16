# 剩余需求：源码核对与实施账本

## 1. 核对基线

审计基线为控制面 `b4ad4f4`，此前已交付 `63456a7`，当前增量 API/Web 0.46.0；并核对 Autoscaling `1720924` 和统一 Helm `648488a`，以及本地
website 文档快照 `c0d49cbb6979b2ce79ea502d62dbc40780a923b0`（提交时间
2026-09-24T19:22:11+01:00）。这是本地可追溯的官方文档基线，不声称它包含该提交后
所有在线变更。公开 Git 源码已推送，不等于所有最新本地镜像已公开发行。
本表的“已有”是可定位的实现；现场通过以各版本 delivery 为准。
功能组没有固定的“全部功能数量”，本账本按可独立实施、可验收的需求划分。

## 2. 需求列表

核对路径：官方仓库 `content/docs/compute/functions/`（尤其 websockets、triggers、logs）、
`content/docs/storage/`（S3 矩阵、objects 与 5 GiB 限制）、`content/docs/auth/` 和
`content/docs/ai-gateway/`。本仓库对应 `api/internal/control/functions_public.go`、
`object_storage.go`、`sql_history*.go`、`services/functions/runtime/server.mjs`、
`services/auth/`、`api/internal/control/ai*.go`、授权/生命周期/调谐代码与 Chart。
例如 Functions 网关明确返回 426，Object Storage 已实现有界 multipart，响应仍明确列出 large_objects/physical_gc 等 missing；
没有把文档目标推断为现有功能。历史通过记录以对应 delivery 的源码与镜像为界。

| ID | 需求 | 基线状态 / 缺口 | 下一验收闭环 |
|---|---|---|---|
| F01 | Functions WebSocket | 未开发；Go public 和 Node runtime 明确返回 426 | 标准握手、子协议、二进制/文本、跨层关闭、存活阻止休眠、重连冷醒和 UI |
| F02 | Functions cron 触发器 | 未开发 | 持久计划、时区、租约、去重、重试、暂停、执行记录和 UI |
| F03 | 对象事件触发 Functions | 未开发 | 与对象发布同事务 outbox、分支绑定、幂等事件、重试/DLQ 和 UI |
| F04 | Functions stdout/stderr | 只有结构化请求日志；没有客户输出采集 | 有界流、脱敏策略/明确授权、实例/版本归属、分页/保留和 UI |
| F05 | Functions 分支复制及恢复 | 未形成完整实现 | 代码共享、环境重封装、受限 SQL 身份重建、隔离与失败恢复 |
| F06 | Functions 完整计量与独立 edge | 未完成；当前 public listener 同 API 进程 | 最小权限进程、实际资源/调用计量、长流与部署切换 |
| S01 | S3 multipart | 有界基础已实现并验收：七类 S3 操作、身份隔离、原子发布/回执、UI 续传、中止、SDK 字节、目录分组分页、已撤销身份管理员中止/审计恢复；仍限 8 MiB | [当前交付](DELIVERY-2026-10-16-CURRENT.md)与[运维合同](MULTIPART-ADMINISTRATION.md)；完整 checksum/大对象、GC/自动回收和写入权转让仍有独立扩展门槛 |
| S02 | 大对象与完整 checksum | 当前单对象 8 MiB；官网快照描述 5 GiB | 有界内存流式 I/O、Range/checksum、断线/配额和大文件 UI |
| S03 | 引用安全物理 GC | 未开发；删除只移除分支清单 | 分支/PITR/保留窗口引用证明、隔离清扫、失败恢复；不得删除历史业务字节 |
| S04 | S3 兼容扩展 | anonymous public read、trailers、STS 等未完成 | 按官网支持矩阵分别实现；每项签名/负例/真实 SDK 验证 |
| S05 | 独立 Storage gateway / 可信计量 | 当前 S3 同 API 进程；last_used_at 不等于计费 | 最小权限独立部署、配额/物理容量、租户并发与用量 |
| A01 | Console MFA | 基础自助已完成：标准 TOTP、恢复码、密码/重放、启用/关闭的其他会话撤销及 UI/PG 已验收 | [当前交付](DELIVERY-2026-10-16-CURRENT.md)；组织强制/WebAuthn/正式恢复流程仍是扩展门槛 |
| A02 | Console OIDC/SSO | 未开发；外部 IdP 可后置，本地协议可先做 | state/nonce/PKCE、绑定、组织策略、本地 IdP 与 UI |
| A03 | 应用 Auth OAuth/Admin/组织/手机号/事件 | 邮件密码/OTP/Magic/TOTP 已有；其余非完整官网能力 | 标准插件、配置隔离、用户管理、webhooks、真实本地服务和 UI |
| A04 | Auth 分支 factor 重绑定 | 未完成；克隆账户不能等同复制可信第二因素 | secret 重封装/重绑、会话清除、恢复与用户可见状态 |
| A05 | Auth 完整删除/恢复/隔离 | 独立门槛 | 原账号/会话/JWKS/密钥与分支生命周期一致性矩阵 |
| C01 | 数据库/角色 rename | 未开发；自定义数据库 Owner 已验收 | PG DDL、依赖目录/连接配置一致、幂等与失败恢复、UI |
| C02 | 默认/系统身份与外部 DDL | 未完成 | 默认 Owner/角色迁移、外部修改识别、受控对账与修复 |
| L01 | 失败创建清理与 TTL | 未开发完整路径 | 依赖图、到期关闭入口、持久 Operation、重试与安全回收 |
| L02 | 原分支 reset / 原地 PITR | 未开发；恢复到新分支已有 | 备份分支、连接切换、GC/连接竞争、失败回退与 UI |
| L03 | Time Travel Assist / Backend 一致恢复 | 未开发 | 查询时间点辅助、Auth/对象/函数分别描述恢复语义并校验 |
| Q01 | 个人 SQL 历史保留/删除 | 采集开关、0/7/30/90 天保留、到期清理和单条手工删除的隔离/重试/审计/UI/PG 基础闭环已完成 | [当前交付](DELIVERY-2026-10-16-CURRENT.md)；未知结果仍保护，人工定性/合规流程另验 |
| Q02 | 项目共享 SQL 历史 | 基础已实现并验收：逐条确认、密文引用、撤回、当前作者/读者授权、删除/到期失效、分页与多账号 UI | [共享合同](SHARED-SQL-HISTORY.md)与[当前交付](DELIVERY-2026-10-16-CURRENT.md)；不含外部公开链接/已复制内容召回 |
| Q03 | SQL AI、计划比较/建议 | 计划比较、受控 Schema/SQL AI 草稿、真实本地模型与人工执行、计划建议通路已验收；AI 修错及个人加密名称/显式 AI 命名也已验收；默认自动命名/长期授权和建议准确性仍有扩展门槛 | [比较合同](SQL-PLAN-COMPARISON.md)、[助手合同](SQL-ASSISTANT.md)；模型建议准确性独立，[修错合同](SQL-AI-REPAIR.md)、[命名合同](SQL-HISTORY-NAMES.md)；默认自动授权/全自动命名另验 |
| I01 | AI 动态供应商配置 UI | 未开发；本地 Ollama Chat/SSE 已验收 | 加密凭据、SSRF/运营权限、连接测试、模型更新、轮换和 UI |
| I02 | AI 更多协议/预算 | 仅文本 Chat Completions；Responses/Messages/Gemini 等不全 | 按协议实现真实流式、用量/预留/预算、取消结算；商业凭据可后置 |
| R01 | 严格 Guest 小数 CPU | QEMU 实际 250/500m 已验收；四倍预算保留 | Guest 隔离、公平性/计量；公开修复 runner 及统一锁待发行 |
| R02 | 完整物理内存归还 | VM status 缩回已有；物理 RAM/OOM 未通过 | Guest/宿主联合采样、压力与 SQL/coldboot；硬件压力后置单列 |
| X01 | 跨实例缩零栅栏 | 缺完整短连接 ledger / 外部 epoch fence | 多 API/Proxy、旧 Worker、分区、新连接/删除竞争；不能用 DB 租约代替 |
| H01 | 软件 HA | API/Worker 已分进程但单副本；元数据/数据面单域 | 多副本、领导切换、旧实例拒绝、业务连续性；物理独立另验 |
| H02 | 全栈 DR | 未完成演练 | 元数据/Secrets/对象/WAL 独立恢复、真实 SQL、实测 RPO/RTO |
| T01 | 全链路可信 TLS | Auth→Proxy 有正反例；浏览器/元数据/Storage 等仍有例外 | 本地 CA/IP SAN、verify-full、轮换/拒绝与网络策略；公网输入后置 |
| O01 | 长期监控/告警/审计/预算 | UI 瞬时/基础指标已有；长期运营不全 | TSDB 保留、SLO/告警闭环、容量/错误预算和审计导出 |
| O02 | 供应链消费策略与统一发行 | digest/source 锁已有；最新本地镜像尚未统一公开 | 同源发布、签名/SBOM/provenance 验证、版本锁/回滚和独立安装 |
| O03 | 官网运营集成 | 计费/充值、服务计划、CLI/MCP 全覆盖非当前完整实现 | 先固定自托管 API/配额合同；外部支付/正式运营输入另列 |

## 3. 实施和记录原则

优先缺代码且不依赖人工输入的闭环。本批已完成 A01 基础闭环及 Q01 的个人策略/到期清理/单条删除闭环；Q02 基础共享与 Q03 的计划比较、受控 SQL AI、修错和显式命名均已有验收；下一项 F02 定时触发器，再继续 F/S/生命周期
和分布式基础设施工作。清单不是完成证明，不以“已有页面”替代实际服务。
同集群串行验证，只重跑新增功能和具体受影响风险。Linux 构建与运行；保留原始
fixture、失败、日志、密码保护输入和所有业务字节。测试完成只退休确认为自有的
临时 Job/VM/Runner，记录 UID；Compute 回到 0。代码、API、模型、UI、部署和复测
文档同步，只有实际通过才关闭对应条目。

新增实现与验收追加到交付记录并更新本表，生产门槛保持独立。不把公网域名、商业
模型或物理故障域不足作为本地软件工程未完成的理由。

## 4. 当前读法

这 35 项是本次审计的需求分组，不是 35 个全部没有代码的模块。A01 基础自助与 Q01 个人策略/到期清理/单条删除已验收；Q02 基础共享也已验收，Q03 的计划比较、SQL AI/建议通路、修错与显式命名基础通过；S01 有界基础及运维/分组也已验收；其余 30 组仍有代码或独立验收缺口（Q03 基本交互已通过；已验收组的默认自动命名、完整 S3 等扩展边界未自动关闭）。Functions WS/触发器、大对象、GC、OIDC/SSO 等明确缺实现；multipart 基础、管理员逻辑中止和分组已验收，仍有大对象/校验/物理回收扩展缺口；小数 CPU、内存缩回、HA、PITR、TLS 等已有部分代码，仍须补齐并独立验收。不能把它们概括为“代码都写完，只差测试”。更多官网运营集成的子项须继续按实际合同细化，不用本表行数声称官网全部能力已穷尽。
