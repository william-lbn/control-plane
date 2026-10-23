# S3 配置、标签、表单与尾部校验合同

本增量沿用已有有界对象存储：单对象 64 MiB、单分片 8 MiB、分支可见对象 256 MiB/1000 个。原 multipart、COPY、预签名、CORS 和大对象回执继续复用。性能、可靠性、HA/DR 专项另行验证。

## 设置与对象身份

控制台新增以下 GET/PUT 入口，沿用项目成员权限及写入 CSRF：

| 资源 | 分支路径下的入口 | 可写内容 |
|---|---|---|
| 桶 | `/storage/buckets/{bucket}/settings` | 访问级别、标签、公开访问阻断、版本/生命周期/Object Lock 声明 |
| 对象 | `/storage/buckets/{bucket}/objects/settings?key=...` | 标签、`content-disposition`、`cache-control` |

GET 的 `ETag` 在响应头中。PUT 必须提交完整 `settings`、`If-Match` 和 `Idempotency-Key`；桶还提交 `access`。缺少前置条件返回 428，过期身份返回 412，原键对应不同请求返回 409。对象设置的身份绑定当前物理对象引用，不能把旧编辑覆盖到后续同名对象。

原键成功回执按分支和操作者隔离，写入原生 PostgreSQL 不可变表。成功回执优先于后续版本检查，重放不会重新应用旧配置。页面遇到网络或 5xx 时保留原键、请求体和 ETag，冻结编辑，允许重试原提交；确认后重新读取当前配置。请求材料仅保留在页面内存。

标签使用 `[{"key":"environment","value":"development"}]`；桶最多 50 个，对象最多 10 个，键不重复，不允许控制字符和保留的 `aws:` 前缀。响应头经过写入和读取校验；`sandbox`、`nosniff` 继续生效。设置、COPY 和 multipart 元数据在同一目录事务中保存，不重写源文件字节。

## S3 支持矩阵

| S3 资源/行为 | 合同 |
|---|---|
| `?acl`、桶 `?policy` | 只读，反映分支所有者及已配置公开读授权 |
| 桶/对象 `?tagging` | GET/PUT/DELETE；对象版本仅支持 `null` |
| `?versioning` | 保存 Enabled/Suspended 声明；MFA Delete 只接受空或 Disabled |
| `?lifecycle` | 有界类型化规则保存、读取、删除；兼容 SDK `withUpdatedAt=true` 读取 |
| `?object-lock` | 保存 Enabled 与默认保留声明 |
| `?publicAccessBlock` | 四个标准标志的 GET/PUT/DELETE |
| `?attributes` | ETag、ObjectSize、SHA-256 FULL_OBJECT、StorageClass 的选择读取 |
| PUT/COPY/multipart | 对象标签及两项响应头；标签 COPY/REPLACE 与 metadata COPY/REPLACE 独立 |
| 匿名读取 | 仅显式 `/s3/{branch}/{bucket}/{key}` 的 GET/HEAD，要求公开桶且阻断规则允许 |
| 浏览器 POST | SigV4 策略、单文件表单、成功状态 200/201/204、元数据/标签及有界校验字段 |
| aws-chunked 尾部 | 签名与非签名数据块模式；SHA-256、SHA-1、CRC32、CRC32C 校验和 |

**版本、生命周期和 Object Lock 仅保存配置，不执行对象多版本、自动到期或 WORM 保留。** 这与应用页面的声明一致，不能把配置读取成功当作保留策略执行证明。ObjectParts、完整扩展 checksum、大于 64 MiB 的对象和物理 GC 继续列在缺口中。没有宣称实现 AWS STS。

`BlockPublicAcls`/`BlockPublicPolicy` 阻止新增公开授权；`IgnorePublicAcls`/`RestrictPublicBuckets` 阻止已有公开桶的匿名读取。解除新增授权阻断需要先保存，再开放公开读取。任何失败或畸形的签名请求都不会降级为匿名请求。

## 浏览器 POST

支持普通 SDK 返回的 `/{bucket}`，目标为凭据锚定分支；也支持显式 `/s3/{branch}/{bucket}`。POST 策略不签 URL，跨分支同名桶不能仅依靠 URL 选择目标：凭据访问后代分支时，策略还必须精确绑定 `x-neon-branch`。前缀条件不能代替精确分支绑定。

策略按 [AWS POST policy](https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-HTTPPOSTConstructPolicy.html) 验证原始 base64 HMAC、过期时间、桶、凭据作用域、每项表单条件、文件名展开和文件大小。策略 JSON 最大 16 KiB，不允许重复键；最多 64 个字段、字段合计 32 KiB，凭据日期起有效期最多一天。文件必须是最后一个表单部分。重定向、临时会话凭据及不支持的字段明确拒绝。

文件首先写入私有临时缓冲，只有策略、权限、标签/响应头及校验通过才打开原生目录并发布对象引用。失败移除临时明文，不发布拒绝的对象。201 返回 `PostResponse`，Location 来自配置的 S3 入口。

## 流式校验

按 [AWS trailing signature](https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-streaming-trailers.html) 验证 SigV4 起始签名、声明长度、数据块、终止块、已声明校验和与签名尾部。兼容固定 MinIO SDK 的尾部换行格式；拒绝未知/重复尾部、数据变化、错误校验和、截断、额外字节和配额溢出。

解码每次最多读 64 KiB，块头有界，最多 4096 块，wire overhead 最多 1 MiB。流中的所有字节在验证完成前都属于未提交缓冲。只有解码器验证过的尾部校验和才交给原有对象发布校验，不因声明 `X-Amz-Trailer` 而跳过校验。

## 数据保留

原生目录格式增至 3，新增配置列与不可变回执表；控制面 metadata schema 064 不变。升级不删除旧对象、分片、WAL、历史引用或物理文件。克隆与恢复沿原生 PostgreSQL 时间线继承配置，回执仍保持确切分支隔离。GC 需要独立的历史引用证明，本增量不执行物理清理。
