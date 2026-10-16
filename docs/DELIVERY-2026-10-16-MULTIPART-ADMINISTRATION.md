# 2026-10-16 当前交付：分段上传运维与目录分页

## 1. 结论

本地运行 **API 0.43.0 / migration 037 / Chart 0.9.33 / Helm revision 102**，
**103 paths / 147 operations**。上一批基础分段上传在 `bfa6aaa203cfc9e5c89255311b632913c8e7714c` 已推送，
独立范围见[0.42 交付](DELIVERY-2026-10-16-MULTIPART.md)。本批新增并现场验收：

| 功能 | 结果 | 边界 |
|---|---|---|
| 当前项目管理员运维 | 查看跨发起身份上传/原终态；撤销原凭据后，UI 中止原三个上传 | Admin Console session；不转让写入/下载权，不操作 completed 对象 |
| 持久审计与恢复 | 外部动作前 admission 意图/审计；分支幂等中止；确认回执/审计同事务；原 key 重试 | admitted 依赖明确重试，未声称后台自动续跑或分布式两阶段提交 |
| 真实响应丢失 | UI 丢弃服务器已确认的第一次响应，再读取 aborted 并取回同 key、同 intent、同回执 | 保留原三个 ID，无新桶/替代凭据/替代上传 |
| S3 目录分组 | delimiter、UTF-8 URL 编码、组/上传共用页大小、同名初始化顺序、原 marker 终态续页 | marker 按本身份保留回执定位，调用者应使用返回值；进阶 checksums/trailers 仍缺 |
| 机器合同和 UI | 修正旧 s3_compatible=false 常量；实际 HTTP 状态闭合模型；恢复错误局部显示并在成功后清除 | 配置状态不代表完整 S3 或生产认证 |

**官网全部功能和生产准入仍未完成。** 8 MiB、100 MiB 分支对象/另计待上传引用限额
保持，物理 blob/历史记录保留；大对象、安全 GC、Functions、Auth/生命周期、跨实例
栅栏、HA/DR 和可信 TLS 等继续见[剩余账本](REMAINING-REQUIREMENTS-2026-10-16.md)。
不能把本阶段通过当作整体完成。

## 2. 方案、模型与复测

[运维/分页合同](MULTIPART-ADMINISTRATION.md)给出组件图、两数据库时序、状态模型、
权限与故障恢复、API、UI 和手工步骤；与[基础分段上传](OBJECT-STORAGE-MULTIPART.md)
共同使用。新增控制表为 `object_multipart_admin_requests`，包含 actor/目标、原因、
幂等 HMAC、原 request_id、admitted/confirmed、保存回执及审计状态约束。无模型服务
或公网输入依赖。公开产品仍为 Go/PostgreSQL + React/TypeScript，未新增 Python 后端。

本批未修改 Neon Rust 或 Autoscaling 内核代码。普通上传者权限保持，管理员中止
只更新原 branch/bucket/upload 的逻辑状态；与完成共用 directory lock。控制池的
intent 事务在分支 I/O 前结束，分支 gate 在 outcome 审计前释放。

## 3. 精确构建与部署

- 父 Git 提交 `bfa6aaa203cfc9e5c89255311b632913c8e7714c`；父提交不冒充运行源码。
- API/Worker 快照 SHA-256：`18f34cad7281b7db8137f964a12bde203e5d1b9e9080cae10912c6b95693a3c4`。
- API/Worker 镜像：`neon.local/control-api@sha256:5a0bcdf294ae7b35120e76f49327002b22e625efa5405dc74c2aeb1ea950449c`。
- Web 快照 SHA-256：`336dfa28869fb9f1f3ecf674bffb3cbca290404c37a5a885de283861762d962e`。
- Web 镜像：`neon.local/control-web@sha256:16c59f9ce0691a7de7e8917e36dbcb32aafd7e6b5f78806eb44d606cdcaa5921`。
- 首次 SDK/实际变更验证 Job `publication-ui-20261016011457`；最终窄读取恢复 Job `publication-ui-20261016012340`。
- 最后 metadata backup SHA-256：`98bc24170d215bae6c6d359105df4ca70fe4eb6bb24c808c3e169a4e972e3aa2`。

Linux 本地构建、固定 builder、三节点按 digest 导入。首个候选在部署前修正 Storage
合同，仅补 API 工件；实际 Go/PG 校验模型通过后才部署。之后截图发现全局旧错误
残留，局部化处理并只重建 Web，API/Worker digest 保持。每次 Helm 升级保存元数据、
values/manifest，9 个 PVC、所有既有 Secret UID 和其他六个 Neon 发布保持。
最终按实际 Docker COPY 输入重新核对 API/Web 文件均与运行快照一致。

没有等待 GitHub CI 或重跑旧 UI 套件。最新本地镜像尚未统一公开发行；公共统一
Helm 0.1.31 候选仍锁 0.23.0 控制镜像，不能当作当前完整本地版本。

## 4. 新增验收与失败记录

1. 三个 Linux Go race/真实 PG 检查通过：管理员/Editor/Viewer/外成员/API key 权限、
   admission 审计故障关闭、后置审计失败后的 admitted 恢复、幂等/原因冲突、
   确认审计仅一次、60 个新意图/小时、S3 实际状态闭合模型。
2. 同套新增检查验证真实 PG + 实际 SDK XML 分组/分页、反向随机 ID 的初始化顺序、
   页间目标中止、UTF-8、marker 在组内、外分支/外桶拒绝、completed 拒绝，以及
   填满活动配额后管理员中止释放一个槽。严格 query/filter/Location 编码检查通过。
3. Go vet、三项 OpenAPI 合同检查、React 类型/格式/Vite 通过。公开 live workflow
   声明/准入同步，49 项 suite 均有对应固定文件，仍然单 worker / 无自动重试。
4. 原 UI 从页面创建新桶/凭据，实际签名 SDK 创建三个上传和一个小分片，验证
   CommonPrefixes 页及组内 marker。页面撤销同一凭据，然后管理员中止原三个 ID。
   第一条故意丢弃成功响应，再查询原状态/取回完全相同的 intent/key/回执。
5. 最终读取恢复仅复用这些已确认 ID、已撤销凭据和原桶，**没有重建/重发 SDK、
   没有新增 upload/凭据或管理员中止意图**。新增一次目录读取失败→局部错误→
   原目录读取成功清除错误；不是重复执行前三个管理员动作。

最初 Go 校验发现首次 Go JSON 与重读 JSONB 字节形式不同，修正为第一次也返回
保存的数据库表示后通过；该失败及一次性 PG Pod 记录保留。Storage 历史合同把
configured boolean 固定成 false 的问题同批修正，并用真实 HTTP 响应核对。
首次 UI 本身通过；截图核对发现模拟断网后的全局错误残留，单独修复并验证局部
错误恢复。当前结论不隐藏这些发现，也不重建原对象来换取通过。

测试桶 `ci-s3-mpadmin-1792113300205` 与三个原 upload/intent 回执保留；原凭据已撤销。
UI ledger 最终 13 条包含接续与资源收尾，**不是
13 个独立产品功能或一次官网全量回归**。原材料在维护者
`control-plane/production/evidence/20261016-multipart-administration`，密钥/原始 DOM/trace
不上传公共 Git。

## 5. 资源与下一项

最终节点 3/3 Ready、无 Pressure；87 个 Deployment/StatefulSet 与期望副本一致
（含有意的 0），错误 Pod 0，托管 VM 0。两次各退休两个确认为本轮所有的终态
Job，均先保存描述和所有容器日志，再用 UID/resourceVersion 删除；一次性 PG Pod
另有自己的 UID 退休回执。业务文件、分片、历史、失败和源码/OCI 全部保留。

随后继续 Q03 的 SQL AI 草稿与计划建议，复用已部署的 Go AI Gateway 和本地模型，
明确发送确认、上下文约束和人工执行，再继续其余账本项；阶段提交后继续工作。
