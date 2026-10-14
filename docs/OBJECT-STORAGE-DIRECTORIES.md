# 对象目录与 S3 Delimiter

## 功能合同

当前批次增加路径分组和分页，控制面 OpenAPI 0.25.0。Go 使用同一条 PostgreSQL 目录查询服务 Console JSON 和 S3 XML，先对匹配 prefix 的 key 分组，再按 C 字节排序应用 cursor 与页大小。

Console `GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` 新增 `delimiter=/`。省略时保留平面列表与原响应；提供 `/` 时返回 `common_prefixes: string[]`。对象和目录合计每页最多 100 个，`next_after` 根据整页最后一个对象或目录计算。分支、存储桶和权限仍由服务端固定，目录不是另外的 bucket。

S3 `ListObjects/ListObjectsV2` 支持非空 UTF-8 delimiter，`CommonPrefixes` 每个计为一项，与对象共用 `max-keys`；跨页不重复目录。`StartAfter/marker` 过滤完整目录名称，不按内部对象名称判断。V2 continuation token 沿用现有不透明客户端协议；客户端不得解析或拼接。`encoding-type=url` 编码返回 key、prefix、delimiter 和目录名。V1 默认返回对象 Owner，V2 按 fetch-owner 选择；Owner 是授权分支标识。

UI 增加“按目录浏览”、子目录按钮和上级目录。切换分支清空目录状态；载入/导航会读取真实数据库并可能冷醒 Compute。状态和监控页面保持原来的无 SQL 检查。

## 验收状态

实现已通过真实 PostgreSQL/MinIO Go SDK 的 V1/V2 非递归分页、平面 UTF-8、`StartAfter` 位于目录内部时的排除以及 URL 编码检查。前端类型/19 项测试/构建与 Helm 检查通过。Linux 本地镜像/三节点导入与真实浏览器八组检查已通过，综合 Go/PG 回归 793 pass / 0 fail / 0 skip；精确来源与复测见 [本地批次回执](DELIVERY-2026-10-15-LOCAL-BATCH.md)。

`storage-directory.spec.ts` 从 UI 启用已有分支存储、创建专属私有 bucket、上传三份保留的根目录/子目录/中文对象；验证 Console 的真实分组/导航，UI 创建只读 S3 凭据，再运行同源 Linux SDK 验证 V1/V2 每页一项以及递归列表。凭据放在 0600 文件，SDK 无 mutation 自动重试。测试结束停用存储、Compute 缩 0，数据/记录保留。

复测使用 [Linux 本地流程](LOCAL-LINUX-DEVELOPMENT.md)，本地构建可加 `--sdk-probe` 导出 `storage-sdk-probe` 和与镜像相同的源码 SHA 回执。不得把旧 SDK 二进制冒充新源码验收。保护 fixture 文件由操作员维护，不上传公共仓库。

## 当前边界

本项不实现 multipart、COPY、bulk-delete、CORS、公开读取、STS、超过 8 MiB 的对象或物理 GC，也不替代权限/HA/TLS 独立准入。目录和所有 blob 引用保持原有分支与 PITR 生命周期。

协议依据：[Amazon S3 ListObjectsV2](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html)，平台目标：[Neon Object Storage](https://neon.com/docs/storage/overview)。
