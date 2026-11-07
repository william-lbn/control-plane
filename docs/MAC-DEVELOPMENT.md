# Mac 控制面开发与集群后端

适用：控制 API / Web 在 Mac 运行，现有 Neon / Autoscaling 继续在 Linux 三节点运行。产品架构和阶段顺序见[完整方案](控制面完整实施计划-20261107.md)。本地启动成功不等于完整产品端到端通过。

## 独立环境

使用仓库指定 Go 1.27.1、Node 24.19.0，显式设置匹配的 `GOROOT` 和 `PATH`；不要改变系统默认版本。独立元数据库用于开发，测试另用可丢弃数据库，并显式设置 `NEON_V2_TEST_SCHEMA=v2_migration_<唯一批次>`。严禁将真实业务元数据库 DSN 注入测试。原始日志、凭据、SSH control socket 放在 Git 忽略的 `.local/`，凭据文件权限 0600，目录 0700。

## SSH 与 TLS

SSH 只在 Mac 的 127.0.0.1 监听，通过118转发至已核对的内部服务地址。不新增 NodePort，不改 hosts、VPN 或宿主路由。转发参数由现场实际 Service IP/端口决定，不写死在产品代码。

- `NEON_KUBE_API`：本地转发的 HTTPS 地址。
- `NEON_KUBE_CA`：实际 Kubernetes CA。
- `NEON_KUBE_TLS_SERVER_NAME`：证书中的原 API 身份，不能以关闭证书校验替代。
- `NEON_KUBE_TOKEN_FILE`：受限 ServiceAccount 的轮换 token 文件；与 client certificate 二选一。每次请求重新读取，文件缺失/为空即拒绝。不从浏览器收 token。
- `NEON_PROXY_HOST`：原 Proxy 证书中的服务名称；`NEON_PROXY_PORT` 为本地转发端口。
- `NEON_PG_HOSTADDR=127.0.0.1`：只覆盖 TCP 地址，TLS SNI、证书验证和 endpoint selector 仍保持原身份。
- `NEON_PG_TLS_MODE=verify-full`、`NEON_PG_CA_FILE`：保留真实 CA 校验。
- 管理 HTTPS 的既有 Gateway URL 可设为其隧道地址；原 per-compute SNI/CA 校验继续由现有客户端执行。

不要把 kube-admin 证书或 token 加入源码、日志、`.env.example` 或命令行。Mac 断线只影响开发控制进程，已经安全运行的 SQL 不因开发链路断开而关闭；未知写结果先对账。

## 启动和验证顺序

1. 固定源码、工具链和本地数据库；先验证迁移、编译和本次受影响测试。
2. 建立隧道，检查 TLS 原身份和真实 Proxy 只读 SQL。401 与 TLS 失败分开记录，认证仍需正式受限身份。
3. API 使用 `NEON_CONTROL_PROCESS_ROLE=api`，本地监听127.0.0.1:8788；Web 运行 `npm run dev`，监听127.0.0.1:5174并转发 `/api`、`/auth`。
4. 初次准备期间不启动 Worker，不启用创建/伸缩写入；驱动及平台发行策略完成后才启用。此状态只是开发底座，不作为功能验收。
5. Worker 单独进程运行，使用同一本地元数据库和授权 namespace。新测试资源使用独立身份；既有验收 tenant/timeline 保留。
6. 每批测试记录源码SHA、请求/Operation、实际资源/数据读回及退出状态；本地通过后将相同提交构建成Linux镜像并通过自有Helm回归。

2026-11-07首次建立的开发实例及回执见 `docs/控制面验收/20261107-重新启动/`。原始证据同步到118 `/data/neon-control-plane/`；服务重启、隧道重连和令牌续期均需核对原进程/对象身份，不能靠旧PID文件冒称运行。
