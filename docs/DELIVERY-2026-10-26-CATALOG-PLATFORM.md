# 2026-10-26 平台身份与 Backend 数据库名称迁移验收

C02 的固定本地基础已验收：平台/默认身份首次确认、原 OID 持续观察和差异展示，以及显式停用的 Auth、Data API、Storage、Functions 数据库名称依赖迁移。原有普通目录身份、首次启动继承、默认连接/Owner、当前展示和原证明衔接沿用已有回执。默认系统数据库外部改名的维护环境验证仍单列，未宣称全部生产认证完成。

13 组新增 Go 检查通过，其中九组纯检查、四组真实 PostgreSQL；另有四组既有改名/存储检查和三组 OpenAPI 合同按受影响范围复验。覆盖初始记录原子回执与审计、不可变历史/timeline 隔离、原生 OID/选项、原配置来源、四类依赖快照、停用与 Pod 退出、原私有文件保留、DNS 合法命名、未知创建恢复和原始错误保留。开发中加固后的针对性复查不再次计为新增组。TypeScript、Vite、格式、页面编译、API 索引与实机套件注册检查通过。

真实 React/原生 PostgreSQL 验收沿用同一专用项目和检查点，累计保存 30 个检查点，含建夹具和 SQL 准备，并非 30 个独立业务功能。页面确认首次平台记录后，模拟服务端已提交而响应丢失，原键恢复同一记录；改变 control_probe 的 CREATEROLE 后展示原 OID 选项差异，恢复后四个原平台身份匹配。活动 Backend 拒绝迁移；挂起 Function 并停用三类服务后，页面提交数据库改名，原键返回同一 Operation。

同一个数据库原 OID、Owner、测试表行、Auth 用户和 Storage 桶/22 字节小对象保留。Auth 与 Data API 的新不可变文件仅修改 DSN 数据库路径，原密码、verifier、Cookie/TOTP、委派和其他私有文件逐字节保留；原 Secret UID 不变。Functions 原 SQL 代际 1 回执保留，代际 2 采用同一私有字节签名新数据库范围。显式重新启用后，原 Auth 用户登录、小对象读取成功；原 Function bundle 在新数据库和 SQL 代际 2 查询到原测试行，最后恢复挂起与实例退休。

实际联调发现并修复了三处代码问题：配置读取误用 bootstrap 返回项、API 缺 Pod 列表权限导致错误提前探测、data_api 下划线不符合 Kubernetes 名称规则。API 只绑定 Deployment 状态/UID，Worker 在 DDL 前证明无 Pod，不扩张 API RBAC；连接文件创建未知结果核对原提案，后续 404 不遮盖原 POST 错误。失败的原 Operation 在修复部署后显式重试并成功，没有再次创建改名任务。初始组织配额、测试选择器/返回字段和维护库限制的失败记录保留，已通过检查从原检查点续验。

默认 postgres 的实际外部改名返回 SQLSTATE 55006：原生 pg_cron、compute_monitor、vm-monitor 持有连接。没有强杀这些后台连接，也没有把该项标为通过；相应维护环境验证见[环境后置边界](ENVIRONMENT-DEFERRED-VALIDATION.md)。当前运行中的角色选项漂移及恢复已真实通过，默认库改名的 OID 比较逻辑已有纯检查，二者证据不能互相替代。

API/Worker 0.91.0 / Chart 0.9.88 / schema 069 / Core Helm 192；原生格式 7 及 058–069 已应用迁移保持原摘要。API 因上述修复共构建四次，Web 一次；每个修复镜像在 Linux 构建后经内网导入三节点，其余组件复用，无新增外网依赖下载。最终 API 构建与 965 个非文档输入、555 个 Go/SQL 输入字节一致，保留的 Web 运行源码与其原构建一致。

最终三个节点、三个 Safekeeper 和七个关键数据面组件就绪，Auth/Compute/VM 和非退休 Function 实例为零。原 955 个 Secret UID、九个 PVC、六套其他 Helm 发行保留，当前 971 个持久 Secret 包括专用验收数据；17 张目录表和三个既有 AI 额度表的范围摘要一致。旧付款记录、额度净额及默认关闭状态保留。14 个终态 QA Job 连同失败尝试的配置/日志按原 UID 归档退休。所有业务字节、原生数据和凭据回执保留。

当前 22 个需求组有固定基础验收；剩余五个业务组 S03/S05/L02/L03/F06，以及 O01/T01/O02 三组。性能、可靠性、HA/DR 和 5 GiB 全量字节验证继续后置；本次小对象验证不扩大为容量或生产认证。后续继续原分支恢复与相关 Backend 一致性。

见[功能合同](CATALOG-PLATFORM-IDENTITIES.md)、[改名合同](CATALOG-RENAME.md)、[结构化回执](acceptance/2026-10-26-catalog-platform-functional.json)和[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)。当前提交的 GitHub 门槛在推送后记录，上一付款修正提交的 15 项成功回执独立保留；本地构建不冒充统一公开发行。

提交 `d08be0ce60bbfd962909144c0015f6ea09ae129a` 的 [GitHub 运行 40247155239](https://github.com/william-lbn/control-plane/actions/runs/40247155239) 已完成，15 项检查全部成功，包括 Go/真实 PostgreSQL、Web、Auth、CLI/MCP、Functions、Helm 和八项组件发布。此回执在下一功能提交中归档，不为 CI 状态重复构建或验收。
