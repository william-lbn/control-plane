# 2026-10-23 S3 设置、POST 与校验尾部交付

API/Worker 0.78.0 和 Web 已部署，Chart 0.9.75，Core Helm 166。metadata schema 064 不变，分支原生存储以追加列升级至格式 3。AI 0.77.0、Auth 0.25.1 和 Gateway 0.66.3 / Helm 56 保留。[功能合同](OBJECT-STORAGE-S3-SETTINGS.md)明确支持范围。

| 验证范围 | 结果 |
|---|---|
| 新增 Go / PostgreSQL / 协议 | 16 组通过：设置验证/闭合 XML、原键回执/CAS/克隆、COPY/multipart 标签、认证隔离、POST、四种校验算法、真实 SDK 认证和请求分发 |
| 受影响原检查 | 2 组通过：原生存储追加格式、checksum 与控制请求分类；其他既有通过检查复用 |
| Web | 格式、TypeScript、Vite、API 索引和 85 套可选入口门槛通过；入口数不是功能通过数 |
| 真实页面与 SDK | 同一夹具累计 19 个检查点通过，先前 11 个检查点按原凭据/对象续验，没有重复创建 |
| 源码/合同 | 编译、vet、OpenAPI 通过；162 paths / 226 operations；433 个运行输入匹配不可变归档 |

实际验证桶/对象标签 CRUD、响应头和原字节、桶设置响应丢失后原键恢复且保留后继修改、缺少/过期 ETag 与改键冲突；真实 MinIO 7.2.0 的配置读写、独立 metadata/tag COPY 指令、multipart 元数据；签名/非签名 checksum trailers 在发布前完整校验；浏览器 POST 201、无效签名与越界条件拒绝；匿名读取、公开阻断和恢复私有；最后仅撤销新测试凭据并停止测试 Compute。

版本、生命周期和 Object Lock 仅存储配置，响应/UI 均明确不执行版本历史、到期/迁移或 WORM/法律保留。ACL/policy 为从实际桶权限导出的只读表示。选定 ETag、大小、SHA-256 与存储类型可读；ObjectParts 仍返回明确的未实现错误，完整类型化 SDK 属性接口继续开发。没有将配置往返或选定属性检查当成整个 S04 完成。

实机发现标签 XML 被旧流式 PUT 分类误判为对象上传，修复分发后先补验真实 SDK 路由与受影响 checksum 分类，再使用原检查点续验剩余八步。部署前的 Secret 数量检查还发现一次受控 Compute 唤醒创建的临时 SSH Secret，已核对其精确 UID/控制器归属；保留所有原 Secret，临时 Secret 随 VM 由控制器回收。原失败记录保留。

## 部署与保全

最终 API 源码快照 `cc53c1daaf94c44da79b3d4ed9ea02010ed0b169e725e351181eb349e5408865`，归档 SHA-256 `e2511267262d0a58cc305719af685dcd42daa11c04b74751aa8c4a02a2c82e9b`。Web 复用首轮同字节源码构建，源码快照 `b8fd89ef87575f72c4718ddc9e19c536ff5d1257f59321b70fa4665f7e20daa5`。构建使用已有缓存，镜像经内网分发至三节点，未推送公开镜像仓库。

| 组件 | SHA-256 镜像摘要 |
|---|---|
| API / Worker | `65f84dfedb4187dc48fcab2b7c87876b968edb01d5d8c0c68a35c722c2b666bb` |
| Web | `1765b95821c1643e15b0966bff7c34c5b01bf9945c720612a34016a15dc4f90c` |
| AI（复用） | `36c9b2a25bc9126c320cfe77b4296c8f98dc46c8b96cb256a9f549d6bd743745` |

最终升级前元数据备份 SHA-256 `5781a191a7015f9758ac435f36950f96c0b261ece595d91ce597c33cc9349794`。24 个 manifest 对象逐项检查，仅允许 API/Worker/Web 镜像变动；058–064 迁移全部冻结。17 张目录表完整摘要一致，无排除范围；六类原存储清单的旧行摘要一致，仅新建专用桶排除在旧行比较之外，新增列的原行默认值另行验证。保留八个专用新对象、一次已完成 multipart、一条分片和六条不可变设置回执，不删除历史业务数据。

9 个 PVC、917 个原非 Helm Secret 身份及其他六套 Helm manifest 保留。四个本轮终态 Job 已先归档再按 UID/资源版本退休。最终三节点、三个 Safekeeper 和七个核心组件 Ready，VM / managed Compute / 非退休 Functions 为 0。私有凭据与原始检查点留在保护目录。

[脱敏回执](acceptance/2026-10-23-s3-settings-functional.json)记录本轮范围；[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)继续列出剩余业务。性能、压力、可靠性和 HA/DR 专项后置。
