# 2026-10-14 Functions HTTP/SSE 与自动休眠交付

## 结论

已实现并部署独立公开入口、UI 显式策略、自动空闲回收与首次 HTTP 冷启动。
真实 Linux UI、NeonVM、Proxy 和 PostgreSQL 已验证。运行源码
`e3e9d04686c2f2d98a99938fe3489ac2e3b70aff`，OpenAPI 0.20.0，迁移 028，
统一 Helm 0.1.28。Console `.100:30788`，Functions `.101:30892`。
已有定义默认私有。测试 Function 正常删除，结束后没有活动 Compute/Functions VM。

**仍是预览功能，不能视为官网全部功能或生产准入。** 独立最小权限 edge 进程、
WebSocket、触发器、后台计量、完整日志、分支/PITR 联动、跨进程外部 epoch fence、
HA/DR 与完整可信 TLS 均未由本次放行。HTTP/SSE 单次请求仍有 15 分钟界限。
设计/数据/API/操作步骤见 [FUNCTIONS-PUBLIC-HTTP.md](FUNCTIONS-PUBLIC-HTTP.md)。

## 精确验证

| 层 | 结果 | 凭据/范围 |
|---|---|---|
| 正式 Linux CI `38466202881` | 14 jobs success；775 Go/PG pass、0 fail、0 skip | 专用 CI 数据库，非现场元数据 |
| OCI | 8 个组件匿名 pull/source/digest 校验通过 | API/Web/Adapter/Gateway/Auth/DataAPI/PostgREST/AI |
| 三节点 | 24 次串行 import、digest 核对通过 | 未修改 Neon/Postgres/autoscaling fork 镜像 |
| Helm Linux | 15 tools tests、10 charts、25 unsafe negatives、public/private Functions 正例通过 | 无密钥写入仓库，issuer 仅 Worker |
| 统一部署 | 8 阶段 inspect/preflight/apply/verify；Secret/PVC/manifest audit pass | 最终远端 `unified-211501`，配置与历史备份保留 |
| 真实 UI | 原始 7 项 + 原实例继续检查 3 项，共 10 个检查组闭合 | 1 worker、0 自动重试；未重复 SQL 写入 |

原 Job `publication-ui-20261013211646` 已通过：UI 原生部署、策略/replay、公开 HTTP/
HTML/Header/重定向/Console 路由隔离、受限 SQL 单次写入、40 秒 SSE 跨过 30 秒 idle
而不回收、无请求自动 suspend、首次请求新 boot 且保留 code/env/SQL 身份和数据。
最后手动停止得到 412，原始记录保存。这是测试仅跳转同一 SPA hash、未等待实际
definition refetch，导致提交旧 If-Match；服务没有绕过版本保护。

测试源码 `4ce43cb0190019965a4d380dbc00d043b1d5750c` 仅修改 HTTP suite，和运行源码
的差异经过私有传输严格白名单验证。后续 Job `publication-ui-20261013212532` 明确
刷新并等待 metadata，继续同一个 `fnc_e1fad0f8e4113d95`：不重新部署、不重放写入，
完成手动 suspend 不自动唤醒、删除原实例和 compute=0。截图已人工查看。
这两个阶段不能虚写成“原始一次全绿”；原失败与恢复链共同组成验收证据。

## 修复与部署边界

- 持有 advisory lock 的调用必须给 metadata 查询保留 pool 余量，避免满池互等。
- 自动 suspend 的 idle marker 与 Operation 同一事务，避免 crash 后丢失冷醒来源。
- Console 与公开调用共享同一 Function admission barrier；stream 完成前保持锁。
- 浏览器 Cookie 不按端口隔离。最终部署使用不同 IP，Helm 拒绝相同主机和非法
  origin，不能只以过滤 HTTP Cookie 宣称 HTML/JavaScript 主机隔离。
- 新 common/issuer/AI/database 凭据未创建或轮换，均复核原 UID；数据、日志、失败
  文件保留。终态测试 Jobs 可在证据归档后，以 UID/resourceVersion 删除。

## 本地证据索引

完整证据保留在工作区 `control-plane/production/evidence/20261014-functions/`：
`http-admission-headroom-attempt1`、`http-final-chart-attempt1`、
`http-isolated-origin-deploy-attempt1`、`ui-http-attempt2`、`ui-http-recovery-attempt1`。
OCI/正式 CI 回执在 `20261013-ai/functions-http-e3e9d04-release-*`，名称沿用既有
传输工具但只记录这次精确提交。上述目录为私有运行证据，不含在公开代码仓库中。

## 下一步

Auth 本地账号安全 UI/密码与会话撤销有新增源码及真实 PG 单测；其现场部署/UI
验收单独记录。S3/SigV4/SDK 兼容仍在推进，不能由 Functions 的通过替代。
