# 2026-10-23 S3 兼容矩阵与双 SDK 交付

API/Worker 0.80.0 已部署，Chart 0.9.77，Core Helm 169。metadata schema 064、原生存储格式 4 不变，Web/AI/Auth/Gateway 复用。按项目固定官网文档快照的 41 项支持操作及预签名、POST、校验尾部逐项关联回执，S04 的该需求基线完成；不是完整 AWS S3 能力认证。[逐项映射](acceptance/2026-10-23-s3-compatibility-matrix.json)保存基线文件摘要和操作到交付的对应关系。

| 范围 | 验证及复用 |
|---|---|
| 桶/对象基础、列表、删除、签名读 | 复用[原 S3 SDK 验收](DELIVERY-2026-10-14-AUTH-S3.md)；批量删除复用[独立回执](DELIVERY-2026-10-15-OBJECT-BULK-DELETE.md) |
| CORS/用户元数据、预签名 PUT | 复用[浏览器 CORS/metadata](DELIVERY-2026-10-15-STORAGE-BROWSER.md)和[预签名上传](DELIVERY-2026-10-15-PRESIGNED-UPLOAD.md)；本轮补验响应 metadata 编码 |
| 七类 multipart 操作 | 复用[原 multipart](DELIVERY-2026-10-16-MULTIPART.md)；本轮补验显式 private/STANDARD 初始化 |
| 标签、配置、ACL/policy、公开读、POST、trailers | 复用[0.78 交付](DELIVERY-2026-10-23-S3-SETTINGS.md)，本轮补验对象 ACL 与拒绝语义 |
| 完整属性/当前分片 | 复用[0.79 交付](DELIVERY-2026-10-23-S3-ATTRIBUTES.md)，本轮以 Boto3 读取验证互通 |
| 本轮新增 | 3 组 Go、3 组受影响检查通过，15 个累计实机检查点通过；27 个授权/加密 PUT/COPY/multipart 负例、18 个未支持子资源均拒绝 |

不支持的授权、加密、保留、重定向和存储类型请求明确返回 501，在 SQL 或 blob 发布前结束；不会忽略请求后返回普通对象成功。合法的 `private` / `STANDARD` 写入及 multipart 初始化继续可用。拒绝的对象和额外存储桶未产生，原源对象字节保持。

MinIO 7.2.0 和服务器已有的 Boto3/botocore 1.34.46 完成互通。Boto3 发现并修复用户 metadata 的 HTTP/1 响应头被自动首字母大写的问题：现在在线路上保持小写，Boto3 返回原 `sdk` 键。原上传已确认成功，修复后只读取原字节/metadata 并记录恢复证明，未重复上传。最初测试的 `PartsCount` 与 Boto3 模型 `TotalPartsCount` 不同，单独修正夹具。所有已通过的八个检查点和两个 Boto3 原结果保留续验。

版本、生命周期和 Object Lock 仍按固定矩阵保存配置，不执行版本历史、到期/迁移或 WORM 策略；不提供 STS，原需求账本将它列为缺口与固定矩阵不符，现已纠正。单对象 64 MiB、分片 8 MiB、分支可见字节 256 MiB 的边界不变。5 GiB、完整扩展 checksum、历史分片回填、安全物理 GC、独立入口/可信计量继续由 S02/S03/S05 及扩展清单承接。没有认证所有 SDK 版本或全部 AWS S3 API。

## 部署与保全

最终源码快照 `554e5d58ae13aed5527bc73e086310576e78663dee30b8a941a1fa7a93e8fca7`，归档 SHA-256 `d6b45f0251b4a99bc2fc978ee7c1ebfae81ca13659f6ac63d4c00eeb4553641f`。API/Worker 镜像 SHA-256 `8394b3be911a12b40f712663576e34e870a4932934450255ef4417eb5b2a1914`；其他五个镜像复用。全部经缓存构建和三节点内网分发，没有额外下载 Boto3。

最终升级前元数据备份 SHA-256 `bcfc47e9ba1e75430c1f135243b605f6346737c7b58a50977c7580148630a2d0`。24 个 manifest 逐项检查，实际仅 API/Worker 镜像变化；058–064 迁移冻结。438 个运行输入、303 个 Control Go、12 个协议 Go、73 个 SQL 输入与最终源码及验证归档一致。最终页面脚本与最终归档完全一致，无测试源码覆盖。

17 张目录表完整摘要不变；六类旧存储清单包含上一轮对象及分片布局，摘要不变，仅本轮新专用桶排除在旧行比较之外。保留三个已验证新对象、一次完成上传和一条分片；拒绝请求未发布其他引用。原新凭据已撤销，七个本轮终态 Job 归档后按 UID/资源版本退休，Go PostgreSQL Pod 已按 UID 退休。9 个 PVC、917 个原非 Helm Secret 身份、正式 BYOK 策略和其他六套 Helm manifest 保留，临时 SSH Secret 由控制器回收。

最终三节点、三个 Safekeeper、七个核心组件 Ready，VM / managed Compute / 非退休 Functions 为 0。[脱敏验收](acceptance/2026-10-23-s3-capabilities-functional.json)记录本轮结果。[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)现为 15 组基础有回执，12 个业务、3 个其他功能组继续；性能、压力、可靠性和 HA/DR 专项后置。
