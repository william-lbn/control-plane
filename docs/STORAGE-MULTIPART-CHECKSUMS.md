# Multipart 校验协商

API 0.89.0 / 原生存储格式 7 实现本项目固定的五种算法。完整字节校验与 S3 协商结果分别保存；不能将合并后文件的 SHA-256 冒充 multipart COMPOSITE。

| 算法 | FULL_OBJECT | COMPOSITE | 创建时省略类型 |
|---|---|---|---|
| CRC32 | 支持 | 支持 | COMPOSITE |
| CRC32C | 支持 | 支持 | COMPOSITE |
| CRC64NVME | 支持 | 拒绝 | FULL_OBJECT |
| SHA1 | 拒绝 | 支持 | COMPOSITE |
| SHA256 | 拒绝 | 支持 | COMPOSITE |

矩阵依据 [AWS 上传校验语义](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html)。本合同没有扩展到 AWS 后续的其他算法。

创建上传使用 `x-amz-checksum-algorithm`、可选 `x-amz-checksum-type`；Console API 对应 `checksum_algorithm`、`checksum_type`。不声明算法的原路径保留原有行为与幂等指纹。算法与类型保存在该上传记录中，后续请求不能更换。

UploadPart 校验实际分片字节，支持已验证的 checksum trailer。COMPOSITE 上传必须提供选定算法的分片 checksum；FULL_OBJECT 可在完成时校验完整对象。已提供的分片值或 SDK 算法声明必须正确。UploadPartCopy 从实际复制范围计算校验。错误、重复、未知或放错操作的 checksum 头会被拒绝，不会静默忽略。

CompleteMultipartUpload 的 XML 接受固定五种 `Checksum…` 字段；重复、未知、非规范 base64、错误长度及外来 namespace 被拒绝。显式算法上传的分片编号必须从 1 连续递增；COMPOSITE 的选择清单必须带选定分片 checksum。旧上传仍允许原已验收的非连续编号。

COMPOSITE 对按编号排列的原始分片 checksum 字节再次计算所选算法，再编码 base64，附加 `-分片数`。FULL_OBJECT 对完整合并字节计算。可选完成头 `x-amz-checksum-type`、对应 checksum、`x-amz-mp-object-size` 必须匹配。Console 完成请求可改用 JSON `checksum_type`、`checksums`、`object_size`，不能与 checksum 头混用。对象清单、完整字节校验、布局、协商结果与原完成回执在同一 SQL 事务发布。

完成回执保存原 checksum 策略，原对象被覆盖或删除后，重放仍返回原结果，且不重新发布对象；改变选片、条件或完成校验声明会冲突。格式 7 使用新增列，不更新旧对象、旧上传及旧完成指纹。旧记录不扫描历史字节、不臆造分片 CRC，也不回填未知布局。

GetObject/HeadObject 的 `ChecksumMode=ENABLED` 和 GetObjectAttributes 返回选定算法及准确的 FULL_OBJECT/COMPOSITE 类型。内部五种完整字节校验继续用于读取完整性验证，不能一起贴上 COMPOSITE 标签。原 multipart 布局还会独立复算 composite；损坏的策略或布局拒绝读取、属性查询和复制。Range 不声明完整对象 checksum。ListParts、ListMultipartUploads 与 Console 上传列表返回已保存的算法/类型及已知分片值。

CopyObject 默认保留原校验值与类型；这是 [AWS CopyObject 的默认语义](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html)。复制仍不伪造原 multipart 布局，分片数量由校验策略单独保留。显式 `x-amz-checksum-algorithm` 可选择已知的完整字节校验；旧对象没有的校验值不会凭空生成。普通 PUT/REST 覆盖会原子清除旧 multipart 策略并发布新完整字节校验。

大小准入沿用 [容量配置](STORAGE-CAPACITY.md)：本地默认对象 64 MiB、分片 8 MiB、分支 256 MiB。格式最高 5 GiB 的实现与基础检查已完成，5 GiB 全量上传/下载/合并/恢复字节验证仍在合适环境完成。

现场入口是 `tools/storage-multipart-checksum-live.mjs`，只允许显式 Linux 私有夹具。沿用 Boto3 1.34.46，通过其标准签名前钩子加入旧模型缺少的类型/CRC64 字段，并读取实际 XML/响应头。所有签名由 SDK 完成，无替代 SDK 下载。该旧版本的 composite 内容自动校验没有冒充通过：HEAD/属性验证协商结果，GET 另核对小对象实际字节。失败保留原上传和检查点，不能用重新创建资源绕过未知结果。

范围及结果见 [功能验收](DELIVERY-2026-10-25-MULTIPART-CHECKSUMS.md)。格式 7 上线后，兼容修订必须理解新策略与回执；旧二进制直接覆盖新对象不属于兼容回滚。
