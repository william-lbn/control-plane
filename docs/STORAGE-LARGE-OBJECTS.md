# 有界大对象与对象完整性

2026-10-25 更新：API 0.88.0 / 原生格式 6 已完成 [CRC64NVME 与完整 checksum 持久化](STORAGE-CHECKSUMS.md)及[最高 5 GiB 容量配置](STORAGE-CAPACITY.md)。当前实际准入仍为 64 MiB，下面 0.71.0 容量基线继续适用；5 GiB 全量字节验证后置，multipart 扩展协商仍缺实现，旧的缺口说明由这两个新合同覆盖。

此合同从 API 0.71.0 / Chart 0.9.68 生效。验收见 [2026-10-22 交付](DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md)。单对象和一个上传已确认分片合计 64 MiB；每分片 8 MiB；分支可见对象 256 MiB。活动上传和分片数量、身份隔离、条件写入、完成回执和物理引用保留规则沿用原合同。此增量未实现官网快照的 5 GiB 上限，也未完成完整扩展 checksum 支持。

REST、S3 单对象 PUT、预签名上传、完整下载与 multipart 合并使用私有 0600 临时文件，64 KiB 复制缓冲，增量计算 SHA-256/MD5/SHA-1/CRC32/CRC32C。每进程最多两个文件缓冲；API 专用 emptyDir 256 MiB。校验、大小或原请求失败后关闭并删除临时文件。启动只清理由本功能命名、权限及类型匹配的未完成文件；临时文件不是客户持久数据。

普通哈希 SigV4、预签名与签名分块 PUT 验证原请求、每个块链、终止签名、解码长度及尾随字节。写入临时文件的内容在全部验证完成前不上传到产品桶、不发布目录。XML 控制和小分片仍有独立大小上限，不能借新对象上限扩大控制请求。

完整对象先校验实际长度与 SHA-256，再通过 ServeContent 提供 Range、后缀 Range、416 和 If-Range。GET/HEAD 的 `x-amz-checksum-mode: ENABLED` 在完整对象响应返回 SHA-256 与 FULL_OBJECT；Range 响应不冒充完整对象 checksum。上传校验 Content-MD5、SHA-256、SHA-1、CRC32、CRC32C；不支持的 CRC64、composite、trailer 不宣称可用。最终 ETag 仍为 SHA-256，分片 ETag 仍为 MD5，调用者应将 ETag 视为不透明标识。

UploadPartCopy 可从大对象读取有界范围，multipart 完成按原分片顺序合并至文件并校验全文件摘要，再沿用原事务目录、事件和完成回执。失败或旧版本冲突不会覆盖原目录；孤立物理字节仍保留等待引用安全 GC。

原生 `neon_storage` 格式 2 为显式、可重放升级：只调整对象和上传大小约束，保留分片 8 MiB 约束及原记录。控制元数据 migration 058 不变。浏览器每次读取最多 1 MiB 文件切片计算摘要，发送 File/Blob；单次上传核对回执大小和摘要，续传仍只发送未确认的原分片。

5 GiB、完整 checksum 持久化/读取矩阵、公开读、STS、trailers、引用安全物理 GC、独立 Storage gateway 是继续开发项；性能、压力、可靠性、HA/DR 在其他环境验收。
