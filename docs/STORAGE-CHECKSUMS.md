# 持久化完整对象 checksum

API/Worker 0.87.0 / Chart 0.9.84 / 原生格式 5 在对象发布事务中保存 SHA-256、SHA-1、CRC32、CRC32C 和 CRC64NVME 五种完整字节校验值。普通 REST/S3 PUT、预签名/POST 上传及现有 multipart 合并共用发布路径；复制保留原校验值，覆盖写替换整组校验值，元数据修改不改变字节校验值。

CRC64NVME 使用原依赖树中已锁定的 `github.com/minio/crc64nvme` 1.1.1，无新增模块下载。上传可使用 `x-amz-checksum-crc64nvme` 与 `x-amz-sdk-checksum-algorithm: CRC64NVME`；错误、缺失、重复值及冲突的声明不能发布对象。其余四种上传检查沿用原路径。签名/非签名 aws-chunked 解码器新增 CRC64NVME trailer 算法，与原签名链、长度和完整结束检查共用。

`GetObject`/`HeadObject` 使用 `x-amz-checksum-mode: ENABLED` 时返回存储的完整校验信息。读取时先核对原 SHA-256 字节身份，再检查存储声明与实际字节一致；不一致返回 503，不能输出对象内容。Range 响应不声明完整对象 checksum。`GetObjectAttributes` 的 Checksum 也返回已保存的五种值和 FULL_OBJECT。

格式 5 仅新增 `objects.checksums` JSONB 列；在目录原身份和代次校验后事务升级，不改写已有物理字节。旧对象保留空映射，只从已有 SHA-256 身份返回原本可证明的值，不臆造 CRC、SHA-1 或自动扫描历史字节。新增列与对象引用在同一提交中发布；multipart 原完成回执重放不重写对象代次。运行格式已升级后，格式 4 的旧二进制不是兼容回滚方案，应使用包含格式 5 逻辑的修订。

这仍是本项目的完整字节合同。S3 multipart 的显式算法选择、逐分片扩展校验字段和 COMPOSITE 协商尚未实现；原 `advanced_checksums` 缺口继续公开，不把现有合并后的全文件 SHA-256 叫作 AWS SHA-256 composite。AWS 对 FULL_OBJECT 和 COMPOSITE 有独立算法矩阵，见 [官方上传校验合同](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html)。这里没有宣称支持 AWS 后续新增的所有算法。

单对象 64 MiB、每分片 8 MiB、分支可见字节 256 MiB 和两槽私有文件缓冲限制保持现有合同；[5 GiB 容量代码及基础检查](STORAGE-CAPACITY.md)现已完成，5 GiB 全量字节验证后置。详见 [大对象合同](STORAGE-LARGE-OBJECTS.md)、[环境边界](ENVIRONMENT-DEFERRED-VALIDATION.md)及[本次验收](DELIVERY-2026-10-25-STORAGE-CHECKSUMS.md)。

可重现实机入口为 `tools/storage-checksum-live.mjs`，只允许 Linux 和显式 `NEON_STORAGE_CHECKSUM_FIXTURE` 私有配置。它使用已有 Boto3 1.34.46；该 SDK 模型早于 CRC64NVME，通过标准签名前钩子加入校验头，并读取实际响应头/XML，没有修改签名算法或下载替代 SDK。入口只接受带专用桶标记的本地夹具，失败保留原检查点；不得重新生成桶、凭据或上传来绕过未知结果。
