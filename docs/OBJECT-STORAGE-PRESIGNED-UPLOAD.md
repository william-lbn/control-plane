# 临时上传地址与 S3 Presigned PUT

## 1. 产品范围

控制面 API 0.28.0 新增两条上传路径。Console 用同源 REST bearer capability，无需额外 S3 access key、跨域浏览器许可或 Console cookie；S3 SDK 用标准 SigV4 query-presigned PUT。两者使用真实 PostgreSQL 目录、现有不可变 blob 后端和原配额。S3 SDK 的跨域浏览器入口仍须 bucket CORS，未因此宣称支持。

Console 在已有对象页选文件与新 key，点击“生成临时上传地址”，再点击“使用临时地址上传”。地址默认遮罩，5 分钟到期；文件摘要由固定版本的开源 `@noble/hashes` 2.4.0 计算，在实验 HTTP 页也可使用。地址为 bearer secret，不出现在审计/访问日志或公开测试报告。读 token 与写 token 使用不同 MAC 域，不能互换。

## 2. API 合同

`POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/presign-upload`

要求登录、现行 Editor 或更高权限、Console CSRF。JSON：

```json
{"key":"documents/example.txt","expires_seconds":300,"sha256":"64个小写十六进制字符","size":123,"content_type":"text/plain"}
```

到期范围 1–900 秒，文件 0–8 MiB，key 沿用相对 UTF-8 规则。桶必须存在，目标必须不存在；已有目标返回 412。成功 200 返回 `url`（同源相对地址）、`method: PUT`、`expires_at`、`headers`、`sha256`、`size`、`protocol`，响应 `Cache-Control: no-store`。

`PUT /storage/v1/{storageBranch}/{bucket}?key=...&token=...`

提交原始文件，必须携带返回的 `Content-Type` 和 `If-None-Match: *`。无需 Console cookie/CSRF，不能携带其他文件替换授权内容。签名绑定 branch、bucket、key、actor、generation、到期、SHA-256、size、Content-Type。错误 token/路径/头返回 403；摘要或大小不符 400；超限 413；存在同名目标或重复使用 412。失效/停用服务、世代变化、账号禁用或当前项目编辑权限撤销均拒绝。

上传继承 PostgreSQL 目录锁及事务，不存在目标的条件在事务内再次验证。先落盘不可变内容，再提交目录，异常不产生指向缺失内容的记录。停止服务或资源受压可能返回 503，调用方需观察实际目录再决定下一步，不盲目重试。

## 3. S3 SDK

`minio.Client.PresignedPutObject` 支持 UTF-8 key 和标准 `UNSIGNED-PAYLOAD` query signature。签名绑定 HTTP method、authority、路径、query、signed headers、日期与有效期。仍要求 live storage:write credential，权限/撤销检查发生在数据库打开之前；read-only credential 无上传权。

标准 S3 预签名 URL 本身不绑定上传 bytes，调用者应签入 Content-MD5 等校验头或使用 Console 的摘要绑定入口。当前 writer 要求 8 MiB 上限、原目录锁与 quota；普通 header-signed unsigned PUT、POST、DELETE 不因本功能放开。COPY 仍需原签名源头规则。长对象、multipart、checksum trailers 未实现。

复测新套件 `web/e2e/storage-presigned-upload.spec.ts`，单 worker / retries=0。保留 private 原文件、capability、writer credential、原 mutation 检查点，公共结果仅含 IDs 与状态，不含 URL/secret。新 case 使用已有 owner fixture，不重复 Project/数据库/GRANT 测试；最后停止当前 Storage、Compute 缩 0，保留 bucket 与实际字节。

依据：[AWS Presigned Upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/PresignedUrlUploadObject.html)、[MinIO Go SDK](https://github.com/minio/minio-go/blob/master/docs/API.md)。此为本地预览路径，公网 TLS、CORS、完整 GC 与 HA/DR 独立准入。
