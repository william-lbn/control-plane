# S3 对象属性与分片快照

`GET /{bucket}/{key}?attributes` 支持 `ETag`、`ObjectSize`、`Checksum`、`StorageClass` 和 `ObjectParts`。固定 MinIO 7.2.0 的完整 `GetObjectAttributes` 方法可直接调用，不再需要绕过其默认属性列表。协议参考 [AWS GetObjectAttributes](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAttributes.html)，本地容量仍以[存储合同](OBJECT-STORAGE-S3-SETTINGS.md)为准。

响应使用 `GetObjectAttributesResponse` XML 根和 `Last-Modified`、`x-amz-version-id: null` 响应头；ETag 位于 XML 中。对象 checksum 为真实整对象 SHA-256，明确标记 `FULL_OBJECT`，不伪造 AWS multipart 的组合 ETag 或组合 checksum。

`x-amz-max-parts` 为 1–1000，默认 1000；`x-amz-part-number-marker` 为 0–10000，默认 0。只列出序号严格大于 marker 的分片，允许非连续序号；`PartsCount` 是总数，`NextPartNumberMarker` 仅在截断时返回本页最后一个序号。每项含真实大小和 base64 SHA-256。畸形/重复分页头返回 400，分片快照损坏或大小不匹配返回 503。

multipart 完成时将所选分片的序号、大小和 SHA-256 随对象清单在同一事务发布。查询不依赖可变的上传状态，也不使用同名对象或相同内容摘要推断来源。普通 PUT 和单次 COPY 创建新的对象代际并清除此前的分片布局；已成功的旧完成回执仍可返回原结果，但不会重新发布旧对象或布局。只改标签/响应头不改变分片快照。

原生存储格式 4 仅追加 `objects.part_layout`，不修改控制面 schema 064 或已冻结的迁移。升级前历史对象的默认空布局表示未记录原分片信息，响应省略 `ObjectParts`；不会根据旧上传回执猜测或回填。完整历史分片回填仍列为缺口，原字节和旧回执保留。

另修正生命周期日期的 UTC 判定：RFC3339 的 `Z`、`+00:00` 和 `-00:00` 午夜等价；非零时区、非午夜和小数秒仍拒绝。日期配置仍只保存声明，不运行生命周期策略。
