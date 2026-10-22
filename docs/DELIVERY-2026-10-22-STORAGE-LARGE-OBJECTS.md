# 2026-10-22 有界大对象、Range 与 checksum

单对象 64 MiB、分片 8 MiB、分支可见对象 256 MiB；私有文件缓冲、浏览器分块哈希、17 MiB 真实 UI/SDK 上传和分片合并、Range/HEAD/SHA-256/CRC32C 拒绝路径通过。21 项 Go、3 项页面哈希检查通过，原目录摘要保留。API/Worker 0.71.0，Web 重建，schema 058 不变 / Chart 0.9.68 / Core Helm 152；Gateway 0.66.3 / Helm 56、AI 0.58.0 保留，最终 VM 0。S02 的 5 GiB 和完整扩展 checksum 仍有缺口，性能/可靠性专项后置。

详见 [实现合同](STORAGE-LARGE-OBJECTS.md)。本次复用原数据分支和原存储数据库，不新建项目或替换原配置。新增专用桶中保留四个 17 MiB + 19 字节对象及一个 CRC32C 校验对象；Console 普通上传、四片上传，真实 MinIO SDK 单次上传、从大对象复制第一片并合并三片上传，均按完整文件大小与 SHA-256 验证。SDK 另验证跨分片 Range、后缀 Range、416、If-Range 不匹配返回完整对象、HEAD checksum、CRC32C 正确值和 BadDigest 不发布对象。

Linux Go 共 21 项顶层检查通过：18 项控制面/真实 PostgreSQL、3 项协议流式检查。页面哈希三项检查、TypeScript、Vite、78 套入口合同与 OpenAPI 128 路径/182 操作通过。失败记录保留；一次 Go 运行缺合同夹具，仅补齐缺失文件并用原二进制续跑一项，没有重跑已通过项。浏览器夹具修正为实际继承的存储数据库，分片 ETag 按原 MD5 合同核对，未改产品数据来适配测试。

原 17 张目录元数据表的全范围行数/摘要一致。原生存储六张表排除新专用桶和本次安装行后的行数/摘要一致，原 11 桶/25 对象和历史分片、上传、事件保留。原生格式 2、64/8 MiB 约束、两项 completed 上传和七个分片验证通过。PVC 9 个、已有 Secret UID 和其他六个 Helm 发行清单保留。三节点、三个 Safekeeper、Pageserver/Broker/MinIO Ready，最终受控 Compute/VM 为 0。

源码摘要 `2335625a31d97c7b3d4014a28f24ee9a5ebc6ef17a872b7596545893bc91080c`；API `neon.local/control-api@sha256:7c5c6484069e2463e2073a2845c6b58f30d32f428acccf255d445c50764c851a`；Web `neon.local/control-web@sha256:5a68170a54abe87baf515bea1fb0d3225ba31b036a6ff4074ed2920e657eaee5`。只构建本轮 API/Web，内网分发三节点，其他组件沿用原 digest。公开代码提交不等于完整发行认证。见 [机器摘要](acceptance/2026-10-22-local-functional.json)和 [剩余功能](FUNCTIONAL-STATUS-2026-10-21.md)。

S02 仍缺官网级 5 GiB 与完整 checksum 扩展；C02 默认/系统漂移与 Backend 名称依赖也仍未整体闭环。现有 13 组基础验收、14 个业务组和运维/TLS/发行三组剩余数量不变，不把新增测试条目当作整体完成率。
