# Bucket CORS 与对象用户元数据

## 1. 范围与数据模型

API 0.29.0 / 独立 chart 0.9.13 候选。`neon_storage.buckets.cors` 为 jsonb 数组，默认 `[]`；`neon_storage.objects.metadata` 为 jsonb 字典，默认 `{}`。数据随 branch timeline 复制/恢复，blob 仍为不可变引用。安装以及旧实例的首次访问，只有在既有项目 marker、service generation 校验通过之后，才通过事务锁补齐这两个列；不改行、不截断数据、不创建新租户。

本批包含 CORS 的 REST/UI 管理、S3 GET/PUT/DELETE `?cors`、真实 OPTIONS preflight 与实际响应头；用户 metadata 的 REST PUT、S3 PutObject、GET/HEAD、COPY 保留与 Console 查看。COPY 的 metadata REPLACE、完整源时间条件仍为后续功能。

## 2. 控制面接口

`GET/PUT/DELETE /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors`

读取需要现行项目读取权限。写入/清空需要 Editor 与 Console CSRF、`If-Match` 当前 quoted SHA-256 ETag。缺版本 428、版本冲突 412，均不覆盖别人的新配置。GET 和成功 PUT 返回 `{rules,branch_id}` 与 ETag，no-store。DELETE 清空规则，不删除 bucket/object。

PUT 示例：

```json
{"rules":[{"id":"application","allowed_origins":["http://127.0.0.1:19001"],"allowed_methods":["GET","HEAD","PUT"],"allowed_headers":["content-type","if-none-match","x-amz-meta-*"],"expose_headers":["ETag","x-amz-meta-purpose"],"max_age_seconds":60}]}
```

最多 100 rules / 64 KiB，单 rule 16 origins、5 methods、32 allowed/exposed headers、maxAge 0–86400。origin 为 http/https 无路径/账号/query，支持一个 wildcard 或 `*`；headers 支持最多一个 wildcard，exposed headers 不使用 wildcard。按顺序选择第一个匹配 origin/method/全部 requested headers 的 rule。

UI：选择 bucket，加载 CORS 规则，编辑 JSON，保存。版本冲突须重新读取；不会自动用新 ETag 重试旧意图。CORS 不授权数据操作，所有实际 S3 请求仍经 SigV4、存活 credential、branch scope 和服务生命周期锁。

## 3. 浏览器与 SDK

S3 `?cors` 按标准 XML/Content-MD5 配置；DELETE 204。未知 XML 层次/字段、DOCTYPE、多个根、错误 MD5 拒绝。没有规则 GET 返回 NoSuchCORSConfiguration，SDK 解析为 nil。

匿名 preflight 使用显式 branch endpoint：`/s3/{branch}/{bucket}/{key}`。仅 OPTIONS 无需 SigV4；裸 `/bucket/key` 没有可认证的 branch 时不提供 preflight。浏览器使用完整 branch 路径签名 URL，其他 server SDK 可继续凭 credential anchor 使用裸桶。服务停用或 bucket/rule 不匹配返回 403，不附加 Allow-Origin。

实际响应仅在 rule 匹配时提供 Allow-Origin、Expose-Headers、Vary。不会启用 Allow-Credentials；签名 URL 请求使用 credentials: omit。CORS 许可不是公开读或浏览器 Cookie 许可。

## 4. 用户元数据

REST 普通 PUT 通过 `X-Neon-Object-Metadata` JSON 字典提交；S3 使用 `x-amz-meta-*`。key 归一化为小写 ASCII `[a-z0-9][a-z0-9_-]{0,63}`，重复大小写 key 拒绝；最多 64 条、key+value 合计 2 KiB，value 当前支持可打印 ASCII。此预览子集明确限制，未宣称支持完整 Unicode/RFC2047。

GET/HEAD 返回 `X-Amz-Meta-{key}`。覆盖未提交新 metadata 会得到空字典，COPY 默认完整保留源字典。普通 Console 上传支持 JSON 编辑器，对象行“元数据”读取实际 HEAD，显示服务器存储值。摘要绑定 REST 临时上传不授权额外 metadata，附加此类头拒绝；S3 SDK 预签名可将 metadata 签入 signed headers。

## 5. 验收与手工复测

新套件 `storage-browser-metadata.spec.ts` 使用已有 owner fixture，在专属 bucket 完成 UI policy/文件/metadata，实际 MinIO SDK policy GET/PUT、metadata PUT/HEAD/COPY，然后在 Linux test Pod 内启动两个无 Cookie 的真实 HTTP origin。Chromium 对不允许的 origin 预检失败，对允许的 origin 自动 OPTIONS + PUT，能够读取 exposed ETag；最后 HEAD 验证 browser metadata，停止服务/Compute 缩 0。全部私有签名地址与原字节留在受保护 fixture，公共回执不含 secret。

本地 Helm revision 79 已部署，原 UI 案例续测 12 项通过，精确版本、原失败与复用记录见 [交付](DELIVERY-2026-10-15-STORAGE-BROWSER.md)。旧功能复用既有验收；只针对本批改变的 COPY metadata 增加必要检查。

依据：[AWS CORS](https://docs.aws.amazon.com/AmazonS3/latest/userguide/ManageCorsUsing.html)、[User metadata](https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingMetadata.html)、[MinIO CORS SDK](https://github.com/minio/minio-go/blob/master/api-bucket-cors.go)。公网 TLS、multipart、物理 GC、HA/DR 保持独立门槛。
