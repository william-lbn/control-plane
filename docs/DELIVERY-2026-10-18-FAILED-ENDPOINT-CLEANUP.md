# 2026-10-18：独立 Compute 创建失败后的保留清理

就绪受管理父分支下，独立 Compute 的原创建失败后，现在可以从原删除界面安全退休。服务端核对唯一原失败 Operation，持久绑定 VM/Service 的原 UID 或确定缺席，按原身份回收计算与入口，保留凭据、配置、分支数据和原失败回执。合同见 [失败 Compute 清理](FAILED-ENDPOINT-CLEANUP.md)。

此切片已完成 Linux Go/真实 PostgreSQL、HTTP 故障及实际 UI 验收。L01 的失败项目/分支创建、分支 TTL、节点分区等仍须继续完成；需求账本其余 28 组的缺口保持开放。

## 精确交付

- API/Worker、Web **0.50.0**；migration **040**；Chart **0.9.42**（`0.50.0-local-failed-endpoint-preview`）；Helm **113 → 114**。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`；未提交代码通过不可变源码快照独立定位。
- 最终 API 源码 fingerprint `5a6b693e902da4b42b20becf98ef21df924d40d750600e9fd52e7f0a3f72eea8`，归档 SHA-256 `b0786636a195b2fd0e65cae197595c33460279947c5dabdf38108f950f85f3d7`。
- API/Worker：`neon.local/control-api@sha256:4821e459a88dc246dd3d3915babd1fed9a5408c0e89b18702672198b844d375c`。
- Web：`neon.local/control-web@sha256:429d61ca5dee7de4e850052d39756c1e9a2d35a088cce00cde8ca7911dc3c233`，源自本切片前一次快照 `93a7b16fb9fb4653e83cd815d2cc183cf039c8a953fb5b0b5296d9c2378bdc42`。155 个完整 Web/容器/构建输入相同，因此复用该镜像。
- Guest 及其余八个组件复用各自既有 digest；API/Web 在 Linux 构建后通过内网导入三台，没有推送公共 registry。
- 升级前完整控制库逻辑备份 SHA-256 `72ed356c36dd55e9e9595ccf838a37e5cf432343b6aed02ad4acadd24c8c3d45`。

部署对 24 个对象作语义检查，只允许 API/Worker/Web 镜像和 Chart 标签变化。9 个 neon PVC、805 个既有 Secret 身份保留，其他六个 release 不变。验收后全集群 106 个原控制器 UID/副本和预期模板、10 个 PVC 身份与绑定均保留，76 个活动 Pod 全部 Ready，VM 为零，实际 OpenAPI 为 0.50.0。

`runtime-source-match.json` 核对 408 个运行构建输入一致。两项测试工具修正分别留证，未把它们描述为已进入 API/Web 镜像：UI 源 SHA-256 `c41951bba95eb1143e5c50e1b318aa54b884a846f70215f26c7623584f18d930`，受信任 operator SHA-256 `7fdc032c87ff40a9bb532ef5035294af0625eebbb392999b676d528f61b2236f`。

## 验证与原失败保留

私有证据根目录为 `control-plane/production/evidence/20261018-failed-endpoint-cleanup/`。密码、私有 checkpoint、原日志、逻辑备份和 OCI 不进入公共 Git。

| 回执 | 结论 |
| --- | --- |
| `go-20261017234633` | 10 个受影响的顶层 Go/真实 PG 测试、race/vet、三个 OpenAPI 检查通过，零跳过；原失败绑定、权限/归属/版本/幂等、删除与旧创建重试竞争、不可变 witness、未知 VM/Service 删除结果后的同名替代保护均覆盖 |
| `go-runtime-source-match.json` | 317 个 API/合同文件与上述通过快照一致，未重复旧业务全量测试 |
| `quality-c` | Linux 格式、TypeScript、Vite、58 套 live 入口准入与 API 索引通过；不是 58 套现场 UI 回归 |
| `images-a` / `images-b` | 第一版 API 被最终 Service 身份保护版替代；Web 输入一致复用；最终 API 导入三节点，导入守卫采样峰值 28 ms |
| `deploy-b` | 备份、Helm 变更约束及既有资源身份保留通过 |
| `ui-a` | 验收工具查询了不存在的元数据列，在暂停 Worker/创建资源前失败；原证据保留，修正为实际 VM 缺席和 schema 前置检查 |
| `ui-b` | UI 工具误读创建响应字段，故障注入前中断；finally 恢复原 Worker，原 Reader 随后自然创建成功，没有改写为失败 |
| `ui-b-reader-retirement` | 私有恢复工具启动前的 artifact 参数检查失败，未提交新业务操作；原证据保留 |
| `ui-b-reader-retirement-b` | 三项实际 UI 恢复检查通过：核对原成功 Reader、正常退休、原成功 Operation/父分支/Writer 保留；VM/Runner 归零，没有创建替代资源 |
| `ui-c` | 修正工具后，七项新的实际 UI 检查及独立 native operator 通过，原失败 Compute 完成五步清理 |
| `owned-terminal-jobs-retirement` | 七个本轮终态自有 Job 先保留完整定义/日志，再按 UID/resourceVersion 退休；没有删除数据或 Secret |
| `platform-after-acceptance` | 106 控制器、10 PVC、76 Ready 活动 Pod、VM 零和 Helm/API 版本通过 |

实际成功 Job 为 `publication-ui-20261018001134`，沿用原项目 `prj_6f3c51bd15b772e3`、分支 `br_6f3c51bd15b772e3`、Writer `ep_6f3c51bd15b772e3`。新失败 Reader 为 `ep_0c1306edc3c8df71`，原创建 `op_1f75812df862a83668ffb374`，清理 `op_fdfa47d7bdec477ef59d4da0`。

受信任 Linux operator 暂停原 UID Worker，让 UI 受理原 Reader，然后只拒绝该原身份 VM CREATE。三台 Kubernetes API 的原模板 dry-run 均先成功、再被本次精确策略拒绝；恢复 Worker 后，原创建日志记录 dependency HTTP 403，原 Service 已存在且 VM 缺席。临时策略按 UID/RV 退休，三端点 dry-run 再次成功。没有伪造元数据状态。

UI 丢失一次真实清理 202 后，以原 key/version 取得同一个清理 Operation。五步成功后，原创建重试返回 409，原失败回执摘要不变。operator 独立核对 VM 缺席 witness、原 Service UID witness、Service 消失以及两个原 credential/config Secret UID/内容摘要保留；原 Worker Ready、VM/Runner 零。

首次中断的原成功 Reader `ep_d8d52fccf0dfa190` 与创建 `op_b1ad7733c7a969cad559f887` 已通过普通 `delete_endpoint` 操作 `op_bab4dbf93a0bb9670ae3c609` 退休。该记录用于说明工具恢复，不算作失败创建清理通过。

后续继续失败项目/分支创建清理和 TTL。保留物理 GC、跨实例外部 fencing、HA/DR、完整 TLS 与长期负载各自独立门槛；共享 SSD 的既有介质错误没有因为本切片通过而被宣布修复。
