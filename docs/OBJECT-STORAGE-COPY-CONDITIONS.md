# S3 COPY 源对象条件

## 合同

S3 CopyObject 增加四个源条件：x-amz-copy-source-if-match、x-amz-copy-source-if-none-match、x-amz-copy-source-if-modified-since、x-amz-copy-source-if-unmodified-since。使用既有 storage:read 与 storage:write 凭据；所有 x-amz 条件头必须包含在 SigV4 签名中。目的端 If-Match / If-None-Match 的旧覆盖保护独立保留。

语义依据 [AWS CopyObject 官方合同](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html)：源 ETag match 优先于对应 unmodified-since；源 ETag none-match 优先于对应 modified-since。因此不能将四个条件简单全部 AND。HTTP 日期按秒比较，和 Last-Modified 精度保持一致；只接受单值 HTTP 日期与单个强 SHA-256 ETag（可带引号）或通配符。弱标签、列表、重复条件字段及非法日期返回 400 InvalidArgument，不静默忽略。

源目录记录的 sha256/updated_at 在现有 PostgreSQL 目录锁事务内读取并判断，判断与目标引用更新是同一事务。条件不满足返回 412 PreconditionFailed；不会发布目标记录，也不会覆盖已有目标。成功继续引用原 project-owned immutable blob，无额外字节拷贝，不改变原对象。修改元数据后的 updated_at 属于当前源对象状态，不保留虚假的旧时间。

不涉及新控制库 migration，也不改变 Console copy 请求合同。Console 已有明确源 SHA 与目的端覆盖保护，仍用原路径。新能力在 S3 SDK 使用；UI 可以查看成功对象、失败目标缺失及原数据。不是跨云/跨区域 copy、版本化 copy、multipart 或 SSE-KMS 支持声明。

## 开发与手工复测

实现：api/internal/control/object_storage_copy_conditions.go；S3 入口调用带条件的目录事务，Console 包装器兼容已有行为。公开 Linux CLI api/cmd/storage-sdk-probe 增加 copy-conditions 模式，使用当前锁定 MinIO Go SDK 的 CopySrcOptions 发送签名条件。

1. 从 UI 登录；对授权分支启用对象存储，创建独立私有测试桶及 conditions/source.txt。保留项目、分支、bucket、key 和 Operation ID。
2. UI 创建仅该分支 storage:write 应用凭据。凭据仅写入 Linux 受保护 0600 文件，不写入 argv/env/日志。
3. 运行同版本 storage-sdk-probe，模式 copy-conditions。它先确认目标不存在，每个变更只发送一次，不在未知结果后自动重试。
4. 分别检查 none-match 相同/不同、modified-since 过去/未来、unmodified-since 过去/未来，以及两组优先级。成功读回原字节/ETag；失败确认目标不存在。
5. 用 SDK signer 发送非法日期及签名后篡改日期，分别必须得到 400 和 403，且没有目标记录。
6. 回到 UI 列表，检查只存在原对象和成功复制项；停止对象存储并正常缩零 Compute。保留原对象、目录、PVC、操作和回执。

web/e2e/storage-copy-conditions.spec.ts 实现该串行 UI→SDK→UI 流程；旧 copy、元数据、CORS、上传套件沿用已验收记录。真实运行结果、具体版本与限制以对应交付报告为准。
