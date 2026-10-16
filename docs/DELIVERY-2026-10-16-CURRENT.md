# 2026-10-16 当前交付：S3 分段上传与原用例续传验收

## 1. 结论

本地运行 **API 0.42.0 / 控制元数据库 migration 036 / Chart 0.9.32 /
Helm revision 100**，**101 paths / 145 operations**。分段表由受控 Storage Driver
在分支数据库增量安装，不新增控制元库 migration。已实现并验收的基础切片：

| 项目 | 现场结果 | 边界 |
|---|---|---|
| Console multipart | 创建、第二片中断、重载原上传续传、跳过已确认第一片、完整下载、中止 | 文件仍限 8 MiB，UI 使用 5 MiB 串行分片，新对象条件发布 |
| 实际 S3 SDK | Create、UploadPart、UploadPartCopy、ListMultipartUploads、ListParts、Complete、Abort 均通过；非连续分片号/分页、来源范围、完整字节/摘要/metadata 核验 | 当前 credential/branch 绑定；未实现跨凭据接管、multipart delimiter 和进阶 checksums |
| 原子发布和重试 | 真实 PostgreSQL 约束故障同时回滚目录/回执；原完成重试不覆盖后来对象；创建幂等、中止和账号/分支隔离 | Blob 不可变字节保留；失败/覆盖/中止不等于物理擦除 |
| 运行资源 | 专用凭据从 UI 撤销；服务回到测试前 disabled；Compute 0；5 个终态 Job 按 UID 留证退休 | 历史 fixture、业务对象、原失败、源码/OCI 和日志全部保留 |

**官网全部功能和生产准入仍未完成。** 本次交付是 S01 有界基础切片，不把 8 MiB
上传称为官网 5 GiB 能力，不关闭大对象、GC、Storage 运维接管或全链路 TLS 门槛。
[剩余账本](REMAINING-REQUIREMENTS-2026-10-16.md)继续逐项实施。上一批共享历史与
计划比较见[独立交付](DELIVERY-2026-10-16-SHARED-HISTORY.md)，不重复旧 UI 套件。

## 2. 代码、API、模型和手工步骤

[分段上传合同](OBJECT-STORAGE-MULTIPART.md)包含总览、组件、SQL 模型、状态图、
时序图、并发/失败语义、六个 Console 操作和手工复测。机器合同见
[OpenAPI](../contracts/openapi-v1.json)，[API 索引](API.md)由实际合同在 Linux 生成。
UI 采用当前 React/TypeScript，产品后端为 Go；未向公开仓库加入 Python 运维/验证。
本阶段未修改原 Neon Rust 或 Autoscaling 内核代码。

`multipart_uploads` 与 `multipart_parts` 在原分支 SQL 数据库内。发布对象和完成
回执在一次目录事务提交，避免跨库 COMMIT 间隙；同一目录 advisory lock 与现有
PUT/COPY/DELETE 共用。S3 adapter 先验证 SigV4、当前 credential 和 scope；Console
走当前项目 Editor、session/CSRF 与 branch/user 绑定。新分支不接管父分支在途上传。

## 3. 精确构建和部署

所有构建/测试在 Linux 串行执行，使用固定 builder 和缓存。没有等待 GitHub CI，
没有为本阶段重复打包不相关的六个服务组件。首次构建 API/Web/SDK，签名兼容修复
只更新 API/Worker 和同源 SDK probe，已验收 Web digest 保持。

- 父 Git 提交 `63456a7ff01631e2106f3a18300515cb33aaf2cc`；它是父提交，实际运行输入由下列快照界定。
- API/Worker 输入 SHA-256：`426bd67931e9e02c2bb9c17aae8bc930def874e98a68faae368dff20b45bd070`。
- API/Worker 镜像：`neon.local/control-api@sha256:7072414f678d7c6350c31ab43e42cb2682b2a2694da227371915f67e5a40a792`。
- Web 输入 SHA-256：`24fc4e901123571bae346e5ed7f87d5799fad9d55c46c505738a528fbff35de6`。
- Web 镜像：`neon.local/control-web@sha256:8bcd4ab3de87665e2356bf72308af5810e58aed68d93de103aad7e3e4e75b8d9`。
- SDK probe 与最终 API 候选同一快照，二进制 SHA-256：`9de418e703085bf11584bee2a24528af110be4437b26396e42eb3c2d2b868c5a`。

两次 Helm 升级各自先保存元数据/values/manifest；最后 metadata backup SHA-256：
`9ff5b5c6503d2d5dc78537ebb921aa4fe70a534a0e1d07c5c17c4ad27a4c8f5f`。9 个 PVC、全部既有 Secret UID 与其他六个 Neon
发布保持。最终依 Docker COPY 输入重新核验 API/Web 源码摘要一致；文档和 test-only
续测改动独立记录，不冒充运行时代码变更。镜像已按 digest 导入三节点，未统一公开
发行这一批本地镜像；公开统一 Helm 0.1.31 候选仍锁 0.23.0 控制镜像。

## 4. 新增验证和原失败链

1. Linux Go race/真实 PG：`TestMultipartParserRejectsAmbiguousOrUnsupportedInput` 与
   `TestMultipartPostgreSQLAtomicPublicationIsolationReplayAndAbort` 通过；包含严格 XML、
   分片替换/MD5、分支/用户隔离、摘要、故障回滚、旧完成重试、来源范围和中止。
2. Go vet、三项 OpenAPI 检查通过；实际 MinIO SDK 的七类签名请求独立测试通过，
   包括请求方法/query/注入字节篡改拒绝。普通 unsigned 写入策略未放开。
3. React 类型检查、格式和 Vite 构建通过。公开 live suite 声明与准入同步。
4. 原 UI Job `publication-ui-20261016003314` 先通过实际中断/续传/下载和凭据创建，
   随后 SDK init 被旧 verifier 拒绝：唯一 HTTP 403、checks 为空、checkpoint 在
   before_create、无 SDK upload ID，证明失败发生在 admission 前。未创建新桶重试。
5. 修复仅准入签名绑定的空体 init/abort，不允许任意 unsigned POST/DELETE 或注入
   对象字节。SDK 七类请求先在 Linux 验证；最终 Job `publication-ui-20261016004103` 复用原桶、文件、
   Console upload 与凭据，从原 checkpoint 继续，完成七类真实 S3 操作和 UI 中止。
   累计 ledger 18 条包含原通过、失败定位和资源收尾，**不把它叫
   18 个独立产品功能或官网全量回归**。

测试桶 `ci-s3-multipart-1792110797526`、项目 `prj_6f3c51bd15b772e3` 与已发布
文件保留；第一轮上传 ID `mpu_f3f42a837887beff7ce4b7c4`。专用凭据已撤销。最终服务
disabled、Compute/VM 0。历史记录和所有私有输入保留在维护者
`control-plane/production/evidence/20261016-multipart`；密钥、原 DOM/trace 不上传公共 Git。

## 5. 终态和后续

最终资源记录：三节点 Ready/无 Pressure，87 个 Deployment/StatefulSet 与期望副本
一致（包含有意的 0），错误 Pod 0、托管 VM 0。5 个终态 Job 在保存描述和全部容器
日志后按 UID/resourceVersion 退休。瞬时健康不等于 HA/DR、物理域隔离或长期 SLO。

继续按账本推进分段运维恢复/兼容、大对象/安全 GC、Functions、Auth/生命周期与
分布式门槛。阶段提交后继续下一项，不能把本轮通过当作整体完成。
