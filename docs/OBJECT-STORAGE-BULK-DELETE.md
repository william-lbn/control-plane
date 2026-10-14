# 条件批量删除与 S3 DeleteObjects

控制面 API 0.27.0 新增 `POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/delete-batch`。请求 `objects` 为 1–1000 个 `{key,sha256}`，JSON 整体沿用 1 MiB 限制。Editor 与 Console CSRF 必须通过；每个观察到的版本必须匹配，任一对象不存在或版本不符返回 412，整批不变。成功返回 `deleted_keys` 和物理内容继续持有的说明。

Go 在同一 PostgreSQL 事务中获取现有目录锁、锁定并验证版本、删除目录记录、提交。不会在多个未协调请求中循环删除，不立即删除 blob。源、复制目标、其他分支和 PITR 的历史引用保持。

React 对象行提供选择框，“选择当前页全部对象”仅选择已加载对象。切换分支、目录或加载新列表清空选择。删除按钮明确二次确认；提交使用当前列表完整 SHA-256，成功后从数据库重新加载。版本冲突要求重新观察，不能自动用新版本覆盖旧删除意图。

S3 `POST /{bucket}?delete` 支持最多 1000 XML identifiers、Content-MD5、真实 SigV4、Quiet、成功/失败逐项结果和不存在 key 的成功语义。默认对象无 S3 versionId；version/ETag/time/size 条件 identifier 和 MFA/Object Lock 等额外能力拒绝，不伪称支持。普通 POST 仍要求签名 payload，空 COPY 的兼容例外不会扩大到批量删除。

协议解析校验 XML root/namespace/重复尾部/未知字段/2 MiB 上限，Go XML 不加载外部实体。事务仅处理去重后的有效 key，错误项保留在响应中。桶不存在始终 NoSuchBucket，不因 Quiet 或全部错误项而返回假成功。

当前已通过 Linux 真正 PostgreSQL 的原子版本冲突/正确删除/共享参考保留/不存在 key/bucket 检查；实际 MinIO SDK DeleteObjects 请求证明单批 POST、签名与 Content-MD5。前端类型/19 项测试/构建、Helm 与 API.md 检查通过。本地 Helm revision 76 已部署，真实 UI 验收 8 项通过；最终 Go 综合回归 799 pass / 0 fail / 0 skip。此结果沿用，不因后续功能重复执行。

复测 `storage-bulk-delete.spec.ts`：UI 原生启用已有分支、创建专属辅助 bucket、记录并上传五份小对象，验证过期版本不会部分删除；UI 选择并删除两份辅助对象，再创建 writer，真实 SDK 批量删除另外两份及不存在 key。保留 `keep.txt` 实际字节、私有原输入、元数据与物理历史、所有失败与操作记录，最后停止服务、Compute 缩 0。

该测试不删除之前保留的业务/目录/复制 bucket 或证据。只删除本案例刚创建、已记录的辅助对象当前目录，正是功能验收内容。源 payload 在私有 fixture 文件单独保留。

依据：[Amazon S3 DeleteObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html)。生产 HA/DR、完整 GC、对象版本/锁与大对象/multipart 独立准入。
