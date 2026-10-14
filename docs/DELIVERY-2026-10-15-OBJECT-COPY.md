# 2026-10-15：对象复制端到端验收

## 完成项与版本

真实 Linux UI Job `publication-ui-20261014170701` 延续原案例，六组检查通过：Console 跨 bucket 条件复制与下载字节比对、重复目标/源版本过期/缺少条件拒绝、原 writer 私密保存、实际 MinIO SDK COPY 与真实字节/元数据、过期源不发布目标、原数据与复制数据保留后停止服务和 Compute 缩 0。

源码分别记录：API 快照 `4cdf56ddc48ef56c35279a2a1dfd3d19ac77db305e9c3f1e7f14d6998d1bdbc6`；Web 继续使用 `419952997d1f10c636d5105a2b78c908d36f64b052cc0c5c1f1449886def1e5d`；Auth 保留先前已验收镜像。Control API 0.26.0，77 paths / 113 operations；独立控制面 Chart 0.9.10，Helm revision 75。新增摘要均在三节点 containerd 验证，尚未发布公网仓库。

9 个 PVC 与 769 个既有非 Helm 内部历史 Secret 身份保留，其他六个 release manifest 不变。升级前 metadata dump SHA 与所有组件摘要保留于私有证据。

## 真实发现与修复

- 维护中的 MinIO SDK 将 source ETag 按不带引号的形式发送；Go 接受同一摘要的精确 quoted/bare 两种表示，仍执行版本条件。
- 第一次实际 COPY 被旧签名策略拒绝。原 SDK 请求复现证明其采用 `UNSIGNED-PAYLOAD`、body 为空、copy-source 在已签名头部中。例外仅限这一形状。正常 unsigned PUT、带 body 的 COPY、已签名源头部被改动仍拒绝；12 项相关真实 SDK signer/Go/PG 正反例通过。
- 首个 UI 跨桶复制已提交，SDK 目标未出现，先检查原目标 404，再调试原 SDK 输入；未重建 bucket、账号或发放替代凭据。
- 续测原始 `storage:write` scope：它按既有平台能力模型传递包含读取能力，不要求 JSON scopes 列表另外出现 `storage:read`。这是测试假设修正，未变更授权模型。

前端、类型、19 项测试、Helm/schema 与 API.md 自动生成通过。最后综合回归在目录阶段为 793 pass / 0 fail / 0 skip；COPY 后的生产批次全量回归另行执行，不把旧计数作为新代码全量证明。

## 保留案例

原 source bucket `ci-s3-1791993225187` 与 `directory/root.txt` 原字节保留；UI target bucket `ci-s3-copy-1791995580486` / `copies/ui.txt`、实际 SDK `copies/sdk.txt` 均保存。writer ID `bcr_13aa3edbdc9c37f3d8994120` 复用，没有额外重新签发。

启用与最终停用 `op_584dd7160f98f3cede158d73` / `op_b5158249c18db50f12bd7fd1`。失败、诊断、协议源码测试、原目标查询和真实浏览器续测的证据都保留在 `control-plane/production/evidence/20261014-auth-mail/copy-*`；口令/Token 原始值留在 Linux 私有文件，不上传 Git。

[复制合同与手动复测](OBJECT-STORAGE-COPY.md)、[Linux 本地交付流程](LOCAL-LINUX-DEVELOPMENT.md)。本项为 basic CopyObject；metadata REPLACE、源时间条件、multipart copy 等仍拒绝。下一项批量删除已进入代码/协议/UI 验收，未计为完成；完整生产准入仍独立。
