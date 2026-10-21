# 2026-10-18：分支到期自动退休

分支可在生命周期界面设置、更新或取消未来 30 天内的到期时间。到期后后台生成持久 `expire_branch` Operation，关闭入口并退休原 Compute/Service，保留原生 timeline、WAL、对象、Secrets 和审计。基础业务功能已通过真实 Linux UI；[合同](BRANCH-EXPIRATION.md)说明支持范围。

API/Worker、Web **0.52.0**，migration **042**，Chart **0.9.44**（`0.52.0-local-branch-expiration-preview`），Helm **115 → 116**。已应用迁移保持冻结。父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`，未提交源独立快照 fingerprint `72f2bfb39d31e95a951629acdb498a78012013c9740c05483c1a64d2a1ce978e`，源码归档 SHA-256 `f1cdedd22855f62c14675264d5b1e7e7908ce5c00060872c3c9d932f52f7d576`。

- API/Worker：`neon.local/control-api@sha256:e45edf4b1497a193cfe653bd25e739f67b5e195fbc4bc504eace7a59b1de270b`。
- Web：`neon.local/control-web@sha256:27c1ac755a1020a8892ce2f63275526bfe5cc05c59ed146188c3b1a7df2edc80`。
- 其余八组件复用各自 0.51 来源；镜像经内网导入三节点，没有公共 registry 推送或重复拉取冷启动镜像。
- 部署前控制库完整逻辑备份 SHA-256 `2a1455a3f015c127462121cb6fbedc7444ddbac508bf20f33047c87ed36e604d`。24 对象语义约束只允许三个业务镜像和 Chart 标签变化，9 个 neon PVC、813 个既有 Secret 身份及其他六个 release 保留。

私有回执根 `control-plane/production/evidence/20261018-branch-expiration/`：

| 回执 | 实际范围 |
| --- | --- |
| `go-20261018042553` | Linux 编译/vet/OpenAPI 及两个新增顶层测试通过：时间解析、真实 PostgreSQL/HTTP 设置/取消/限制/重放/到期、六步 Worker 退休与数据保留；独立临时 PostgreSQL 已退休 |
| `quality-a` | 格式、TypeScript、Vite、CLI 语法及 60 套入口准入通过；110 paths / 158 operations，不代表执行 60 套旧 UI |
| `images-a` / `deploy-a` | 同一快照构建、三节点导入、备份和部署约束通过 |
| `ui-a` | 一个原生子分支和 Writer，九项新检查通过；真实 SQL 写入读取，设置响应丢失后同 key 重放，更新、取消、最终到期和自动退休 |
| `ui-a/expiration-observer` | 独立只读 observer 核对同一 timeline/祖先、原 Secret UID/摘要保留及原 VM/Service/Runner 消失；没有修改元数据或替代 UI 操作 |
| `runtime-source-match.json` | 当前运行/合同/UI 输入与实际构建快照一致 |
| `owned-terminal-jobs-retirement` | 两个终态自有 Job 留档后按 UID/RV 退休；没有删除业务数据或 Secret |
| `functional-postcheck.json` | API/Worker/Web Ready，实际 OpenAPI 0.52.0、Helm 116，VM 零 |

原项目/父分支/Writer 为 `prj_6f3c51bd15b772e3` / `br_6f3c51bd15b772e3` / `ep_6f3c51bd15b772e3`。新分支 `br_1e56beb5a67a6f06`、Writer `ep_1e56beb5a67a6f06`，创建 `op_310ed911ffe42e61d7842436`，原自动退休 `op_ff496ac6a38eda7c78acd4a5`，Job `publication-ui-20261018043908`。前后同一 native timeline `54ff19eb948981d1091ebe78c493705c`，父分支和原业务资源保留。

第一轮 Go 工具因前一日自有 BuildKit 的 12 小时运行期结束而未连接成功；原日志保留，按原缓存/配置/限额恢复同一构建器后通过。Job 归档后的私有收尾工具发生局部变量覆盖，已修正并从既有归档继续最小只读检查，没有重复删除 Job 或业务操作。

按用户本轮要求，性能压测、可靠性、HA/DR、分区和跨实例外部 fencing 后置，没有重复历史整个平台专项。到期是后台退休合同，不是实时外部连接栅栏或物理永久删除。Functions/Data API 须先退休后设置 TTL；Auth/对象沿既有保留删除 Driver 停用。后续继续 Functions WebSocket 及其他业务组，不能据此宣称全部功能完成。
