# 可配置的对象容量

2026-10-25 后续实现：[显式 multipart 校验、逐片字段与 COMPOSITE](STORAGE-MULTIPART-CHECKSUMS.md)现已通过[功能验收](DELIVERY-2026-10-25-MULTIPART-CHECKSUMS.md)；下面旧版本的缺口说明由新合同覆盖。5 GiB 完整字节验证继续后置。
API/Worker 0.88.0 / Chart 0.9.85 / 原生格式 6 已实现最高 5 GiB 的对象格式与运营配置。当前环境仍使用已验收的 **单对象 64 MiB / 分支可见字节 256 MiB / 两槽临时磁盘合计 256 MiB / 150 秒传输预算**。5 GiB 配置、元数据边界、64 位计数、65 MiB 私有文件路径和基础现场验证已通过；**5 GiB 完整字节上传、下载、合并及恢复仍待目标环境验证**。

格式上限和实际准入分开：OpenAPI 的最大值表示格式可表达的范围，`GET .../storage` 的 `limits.object_bytes` 才是当前写入准入，`object_format_bytes` 是 5 GiB 格式上限。调用方应读取当前准入，不能从格式上限推断运营者已提供所需磁盘。Console 既有上传流程读取动态准入，沿用分块哈希，无新增 UI 构建。

在具备容量的目标环境可显式配置：

```yaml
objectStorage:
  capacity:
    objectBytes: 5368709120
    branchBytes: 21474836480
    bufferMiB: 10240
    transferSeconds: 1800
```

分支预算必须覆盖一个对象，私有磁盘预算必须同时容纳两个最大对象；磁盘卷是 disk emptyDir，不是 10 GiB 内存申请。每分片仍为 8 MiB、每上传最多 1000 个分片，足以表达 5 GiB 对象。运营者需要为 API 所在节点安排对应可用磁盘；本次没有在当前 SSD 上配置 10 GiB 临时卷，也没有做容量压力认证。

Chart 和进程启动同时校验配置。进程的 `NEON_STORAGE_OBJECT_BYTES`、`NEON_STORAGE_BRANCH_BYTES`、`NEON_STORAGE_BUFFER_BYTES`、`NEON_STORAGE_TRANSFER_SECONDS` 只接受有界十进制整数，拒绝不完整预算、溢出、负数、无限时限及超过 5 GiB 的对象配置。配置随 Pod 启动固定，租户请求不能修改。传输时限为 150–3600 秒，仅为已准入的存储请求设置读/写 deadline 与请求 context；普通 JSON 接口保留原时限。预签名上传也在读取首字节前获得该有限预算，不唤醒 SQL 来校验无效内容。

REST/S3/POST、预签名及 multipart 写入均使用运营准入，复制同样检查目标对象大小；原生大小约束和内部完整性读取使用 5 GiB 格式边界。降低写入预算不会删除已有大对象引用，超过新写入预算的复制被拒绝。保留大对象时应维持足够读取磁盘和传输预算，避免因缩小运行资源造成读取不可用。

原生格式 6 的新 SQL 只放宽可表达的对象/预期上传大小，并保留 8 MiB 分片约束。升级和重放直接使用新的容量 SQL，不能先重新安装旧 64 MiB 约束，否则已有大对象行会阻止重放。旧 SQL 来源保持原字节；已有对象、checksum、分片、上传和事件记录不被重写。降级须采用理解格式 6 的修订。

现场仅验证默认准入、超限拒绝、真实格式升级和原数据保留，没有新建桶、凭据或对象。已有五种完整 checksum 和真实 SDK 回执复用。S02 的显式 multipart 校验算法协商、逐分片扩展字段及 COMPOSITE 仍缺实现，见 [checksum 合同](STORAGE-CHECKSUMS.md)、[验收](DELIVERY-2026-10-25-STORAGE-CAPACITY.md)与[环境后置清单](ENVIRONMENT-DEFERRED-VALIDATION.md)。
