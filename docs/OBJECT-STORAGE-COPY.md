# 对象复制：模型与验收合同

## 架构与安全

同一授权数据库分支内，源/目标可以在不同存储桶。Go 使用目录事务锁，与上传、删除、配额更新串行，复制一条指向源不可变 blob 的目录记录。源对象不删除，未读取外部 URL，不复制 MinIO root 凭据。校验 blob 属于该项目，其他项目的引用不能被复制。共享内容不会因源清单删除而立即回收，继续遵守分支/PITR 持有规则。

控制面 `POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/copy`，要求 editor、Console CSRF 和以下 JSON：

```json
{"source_bucket":"uploads","source_key":"directory/report.txt","source_sha256":"64位小写十六进制摘要","destination_key":"copies/report.txt"}
```

目标为 URL 的 bucket，源只能来自同一分支。新建必须 `If-None-Match: *`；覆盖必须提供目标当前 quoted ETag 的 `If-Match`。没有条件返回 428；源版本或目标冲突 412。事务成功 201 返回 key/bucket/sha256/size/content_type/updated_at。未知结果先读取原目标确认，不自动覆盖或创建替代目标。

UI 对象行新增“复制”，选择目标 bucket 和新 key。界面只允许创建不存在的目标，源版本由已观察对象 SHA-256 固定，成功后加载真实目标目录。切换分支清空待复制状态。

S3 基础 `CopyObject` 使用已验签的 `x-amz-copy-source` 和可选 `x-amz-copy-source-if-match`；必须同时具备源读和目标写权限。实际 SDK 的空 body `UNSIGNED-PAYLOAD` 仅当 copy-source 已签名才允许；普通 unsigned 上传仍拒绝。无请求体，复制相同源/目标、versionId、源路径混淆拒绝。目标 If-Match/If-None-Match 条件如提供则执行。返回真实 CopyObjectResult，摘要和 LastModified 来自已提交事务。

本增量仅 metadata COPY。REPLACE、自定义 metadata/tag/ACL/tier、源时间/none-match条件、multipart copy、跨分支/跨区域 copy 仍明确拒绝，不能把基础 COPY 验收算作完整协议。

## 验收

1. 真实 PostgreSQL 断言共享 blob 引用、元数据/配额事务、重复目标/源版本冲突、不存在目标 If-Match 拒绝、源删除后目标引用仍在、外国项目 blob 拒绝。
2. Linux TypeScript/Go 质量检查、独立 OpenAPI 与角色路由检查。
3. 本地 API/Web 与同源 Linux SDK probe 构建，三节点导入；保留其余组件和 operator Secret/PVC。
4. `storage-copy.spec.ts` 复用前一轮已保留的 directory source，UI 跨 bucket 条件复制、实际下载字节比对、重复/过期/缺少条件负例；UI 发放受保护 S3 writer，真实 SDK 签名复制与下载比对、过期源不发布目标。
5. 保存原数据/目标/用户凭据/操作记录；停止测试服务，Compute 缩 0。失败保留原 fixture 并检查已提交结果后续测。

本地真实 UI 与 S3 SDK 验收已通过，详见 [精确回执](DELIVERY-2026-10-15-OBJECT-COPY.md)。协议依据：[Amazon S3 CopyObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html)。
