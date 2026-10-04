# Backend 产品切片验收：Data API 与应用凭据

## 1. 交付结论

本次在 Go/PostgreSQL 控制面中实现并在 Linux Kubernetes 部署了原生 Data API
生命周期和分支应用凭据，使用 React/TypeScript 浏览器界面执行实际测试。
这是一版可使用、可复测的自托管预览产品，**没有完成 Neon 官网全部服务或生产认证**。
当前合同为 OpenAPI 0.5.0（41 个路径、57 个控制操作），元数据迁移为 001–011，
控制面 Chart 为 0.3.0。API、Worker、Web 独立运行。

验收对象是本仓库源代码和固定镜像，使用用户 fork 的 Neon/Autoscaling/PG16 数据面。
本次没有修改这些数据面源码。历史源码与私有 Python 验证工具没有进入公开控制面仓库。
发布镜像必须另行取得本次 commit 的 GitHub Linux CI、registry digest 和匿名拉取回执；
不能把候选镜像的现场通过结果直接写成发布镜像通过。

## 2. 实现与验收范围

| 产品路径 | 实现与实际验证 | 边界 |
|---|---|---|
| Data API 配置 | UI 输入数据库/schema、issuer、audience、公钥 JWKS、HTTPS 浏览器来源；API generation CAS、幂等、持久 Operation | 静态公钥；默认关闭；启用需显式实验传输配置 |
| Data API 身份 | 独立应用 JWT；短期受信委托；分支 audience；非 owner、非超级用户、非 BYPASSRLS 的数据库角色 | 不是 Managed Auth；Console Cookie/控制 API Key 不能授权应用数据 |
| 真实读写 | 从 UI 为真实 Neon 数据库准备 RLS；双主体行隔离；合法插入；伪造归属被 SQLSTATE 42501 拒绝 | Explorer 当前提供 GET/POST；公共 REST 支持 GET/HEAD/POST/PATCH/DELETE；RPC/视图/分区表不在首版验收范围 |
| 服务生命周期 | UI 启用、相同请求返回同一 Operation、停用入口、重新启用并保留原有行及浏览器来源配置 | 停用保留数据/角色/Secret；变更 schema 后需明确停用/重启刷新缓存 |
| Compute 生命周期 | Data API 仍运行时，UI 手动 1→0、REST 首请求 0→1；UI 设置 60 秒空闲策略，自动 1→0、首请求再次 0→1 | 实验环境单 API/Worker；外部 Proxy 新连接竞争和跨实例 fencing 未放行 |
| 应用凭据 | UI 创建、仅一次返回 Token、相同幂等请求不返回秘密、分支/后代/兄弟/模型约束、轮换及撤销、刷新不能找回明文 | 仅 ai_gateway:invoke；目前只检查授权，不提供模型推理 |
| 权限持续检查 | 真实 PG 集成校验当前组织/项目权限、issuer 降权、过期/撤销、代次变化、跨项目与 lineage 限制、并发幂等 | 不替代外部安全评审、SSO/MFA、完整生产攻防测试 |

Data API 本轮现场验收 Job：`publication-ui-20261004142909`。
应用凭据本轮现场验收 Job：`publication-ui-20261004143413`。
完整脱敏 JSON、截图、操作身份和故障记录保留在维护者部署工作区；数据库密码、
登录秘密、pepper、委托密钥和应用 Token 不进入本报告或公共测试 artifact。

## 3. 发现的问题及处理

### 3.1 数据库授权不能靠角色属性推断

实际 Neon 验证发现，CREATEDB/CREATEROLE 不提供对已有数据库的 CREATE 权限，
也不代替 CONNECT 的 GRANT OPTION。Driver 在修改角色之前检查前置授权，缺少时
返回 `data_api_database_grant_required`。数据库/schema owner 通过 Workbench
明确委派专用身份所需权限，步骤见 [Driver 手册](DATA-API-NATIVE-DRIVER.md)。
没有把应用登录或请求角色提升为超级用户或 BYPASSRLS。

### 3.2 浏览器 Origin 与真实 RLS 错误必须区分

实验 Console 使用 HTTP，公共 Data API 只接受配置中的精确 HTTPS Origin。
直接在浏览器写入被 CORS 拒绝不代表 RLS 已经过验收。
Explorer 使用受会话/CSRF/项目 Editor 授权保护的服务器测试入口，只转发应用 JWT；
公共 gateway 的 CORS 未放宽。伪造写入测试必须同时断言 HTTP 403 和 SQLSTATE 42501。

### 3.3 池的配置值不等于物理连接已经释放

实际 pg_stat_activity 显示 PostgREST 的 LISTEN 与空闲池连接持续数分钟，
阻止 Compute 缩零。首版静态配置关闭常驻 LISTEN，仅为拥有的平台服务登录设置
`idle_session_timeout=5s`。PostgreSQL 不因此结束活动查询或打开的事务。
手动缩零最多等待 10 秒正常关闭，仍校验全部 SQL 活动及 VM UID；未增加强杀或白名单。

真实 PostgREST/PG 集成观察三次物理连接关闭，每次随后只发一次写请求并要求成功，
不通过写重试掩盖失败。长查询与闲置事务跨过超时仍成功。真正 Neon UI 又验证了
手动/自动缩零后首请求冷醒。此前失败 attempt 保留，未删除或覆盖成通过记录。

### 3.4 页面状态与秘密生命周期

切换项目/分支或离开页面后，过期异步响应不能重新显示旧分支的结果或 Token。
凭据列表显示“未到期/未撤销”，区别于有效授权与实际服务运行状态。
浏览器来源配置在重新启用时保留；秘密输入默认遮罩，不持久到 localStorage。

## 4. Linux 质量门槛

| 门槛 | 本次结果 |
|---|---|
| Go 格式、vet、race、真实 PG/PostgREST、OpenAPI 路由/引用/安全 scheme | 215 项 pass；0 fail；0 skip；Job dataapi-quality-20261004142000 |
| TypeScript、Node 回归、Vite、Helm 默认/启用/拒绝门槛 | Linux 通过；最终源码 Job publication-quality-20261004144519；界面表单增加响应式布局与有界结果展示 |
| Native Data API 浏览器闭环 | 通过，包含上述手动/自动缩零与冷醒 |
| 应用凭据浏览器闭环 | 通过，测试凭据最后撤销，数据分支和审计保留 |
| 原生项目/分支与 Worker 故障接管 | Job publication-ui-20261004143527 通过；UI 操作排队时中断 Worker，新 Pod 接管同一 Operation，领导 epoch 增长；父子数据隔离、冷醒和休眠监控通过 |

验收运行在 Linux Chromium。测试后停用测试 Data API 并把对应 Compute 置为 0；
不删除应用数据、Secrets、PVC、对象、WAL、fixture 或测试记录。测试例始终保留
低资源上限，避免同时启动多个非必要 Compute。构建与现场测试串行，构建器完成后停止。

## 5. 手动复测

1. 根据 [部署手册](DEPLOYMENT.md) 准备已有 Neon 数据面、独立元数据 PG、
   最小权限 Kubernetes 身份、固定镜像、已备份的 HMAC/keyring 与明确传输配置。
2. 登录 Console，创建低资源项目。Workbench 运行 Driver 手册的数据库/schema 授权、
   表和 RLS SQL。创建真实身份提供商的公钥 JWT；不要输入私钥或控制面 API Key。
3. 在 Data API 页配置 issuer/audience/JWKS/浏览器来源，启用并查看完整 Operation。
   用两个主体分别读取；验证过期、错误 audience、特权 role 与伪造归属均被拒绝。
4. 合法写入后停用，再启用；确认原有行和浏览器来源配置还在。
5. 在 Compute 页手动缩到 0，等待真实“已休眠”。从 Data API 发第一条合法请求，
   确认数据正确且 Compute 恢复。随后打开自动休眠、设置空闲 60 秒并保存。
6. 不发数据库/Data API 请求；只观察 runtime。等待新的自动休眠，再发第一条
   Data API 请求并检查数据。监控页面不得因为查看状态唤醒休眠 Compute。
7. 在应用凭据页创建受限 Token，只保存到安全位置。检查当前/后代通过、兄弟/越界模型
   被拒绝；轮换后旧 Token 失效，撤销后新 Token 失效；刷新不能读回明文。
8. 停用自己的测试 Data API，关闭对应 SQL 连接，正常 suspend Compute。
   保存版本、镜像 digest、Operation ID、fixture、截图与正/负例结果。

自动测试命令、必需环境变量与秘密挂载见 [测试手册](TESTING.md)。真实模型调用
不属于这组验收，也不可以复用 Codex/ChatGPT 登录凭据来完成。

## 6. 后续产品与生产工作

**下一产品切片：**运营方 UI 录入/轮换上游，版本化模型目录、组织授权和预算，
固定分支入口调用与 Playground；真实供应商凭据配置后再放行推理服务。
设计与官方核验见 [AI Gateway 设计](AI-GATEWAY-PLATFORM-DESIGN.md)。
后续逐项实现 Managed Better Auth、Functions、产品 Object Storage 与 PITR/删除生命周期，
每项都需要真实 Driver、API/模型、UI、隔离负例和恢复测试。

**生产前必须完成：**可信 TLS/网络隔离、Go 化外部 adapter、外部 admission ledger/fencing、
多实例 HA、全栈 DR/PITR、权限审计、持久监控/SLO、小数 CPU 和完整内存缩回。
原因、完成判据及当前限制统一维护在 [生产门槛](PRODUCTION-GATES.md)。
不能用这组实验环境结果宣传官网全部功能已经完成。
