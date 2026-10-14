# 2026-10-14/15：Linux 本地批次验收

## 已验收切片

- **Auth Email OTP**：真实 Linux UI `publication-ui-20261014145221`，八组检查通过。
- **Auth Magic Link**：真实 Linux UI `publication-ui-20261014153828`，八组检查通过；详细来源/续测见 [Magic Link 回执](DELIVERY-2026-10-14-LOCAL-MAGIC-LINK.md)。
- **S3/Console 对象目录**：真实 Linux UI `publication-ui-20261014155608`，八组检查通过，涵盖 UI 三对象上传、目录分组、中文导航、只读凭据、实际 MinIO SDK V1/V2 每页一项与递归列表。
- **Go 综合回归**：Linux Job `dataapi-quality-20261014155827`，793 pass、0 fail、0 skip，真实独立 PostgreSQL，race/vet/format；不是单纯编译或假数据验收。

上述均在有限资源上执行真实功能测试，不用物理压力测试替代业务验收。除 OTP 已启动的历史 CI 外，本轮没有为 Magic Link 或对象目录另跑远程 CI。

## 对象目录准确来源

本地源码快照 SHA-256：`1359bf1e57708439d6ad6929d87fad6513c397d400a7d3ebb5654b589e6da202`。API/Web 两镜像和 SDK probe 取该快照，三个节点精确摘要导入；Auth 继续保留已验收的 Magic Link 镜像。每个组件分别保存完整 source/OCI/hash 回执，不把 Git 父提交冒充未提交代码。

控制面 Helm revision 73；升级前 metadata dump 保存 SHA-256。9 个 PVC、768 个既有非 Helm 历史 Secret 的 UID 保留，其他六个 release manifest 不变。

复用原 branch `br_6f3c51bd15b772e3`、database `owner_transfer_v1`。Bucket `ci-s3-1791993225187` 保留三份对象。存储启用/停用操作为 `op_437aafa8ead7955fe7a1de25`、`op_74adf8e197d0b012bce33276`。测试最后真实 Compute 缩为 0，目录、对象、凭据和复测文件保留。

首次目录 UI 在启用前尝试改动已有不可变数据库选择，测试停止，没有资源 mutation。修正测试为读取并确认既有数据库，续测沿用原计划 bucket。该修复只替换明确的 TypeScript 验证文件，并记录它的摘要；未重新打包或伪称产品运行代码发生改变。

## 开源交付与下一项

[Linux 本地构建](LOCAL-LINUX-DEVELOPMENT.md)包括源码快照、摘要校验、串行 BuildKit、三节点导入、只升级受影响服务、实际 UI 与日志留存。已归档日志并清理 24 个已完成/失败的本批临时 Job，未删除数据、邮件、SQL、PVC、Secret 或测试记录。

代码与 Helm 仍在本地开发批次，未把新本地镜像写进公开发布锁。完成约定批次后统一回归、提交、远程 CI 与公开镜像/Chart 发布。不能将本地 `neon.local` 镜像当作其他人可匿名拉取的发行镜像。

下一项 **对象复制**已实现 API/S3 入口、条件事务和 UI，定向真实 PostgreSQL 检查通过；正在完成真实 SDK 与 UI 验收，尚未计入本报告通过项。全部官网功能、HA/DR、跨实例栅栏和全链路 TLS 仍由独立门槛约束，当前产品仍为预览。
