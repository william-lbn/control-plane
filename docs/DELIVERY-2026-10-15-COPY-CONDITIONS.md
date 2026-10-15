# 2026-10-15：S3 COPY 源条件本地交付

## 结论与运行版本

已实现 S3 CopyObject 源 ETag/时间条件，并完成真实 Linux React UI→SDK→UI 验收。完整语义、模型、安全边界及手工步骤见 [COPY 源条件](OBJECT-STORAGE-COPY-CONDITIONS.md)。

- API 0.36.0，85 paths / 124 operations；migration 032；Chart 0.9.21，Control Plane Helm revision 87→88。
- API/Worker 源快照 15bc5b7c78e85a0575a828aac7bd20cfaa437c58d74edaaa265959af2b239292，父 Git a8cb53d。
- API 镜像 neon.local/control-api@sha256:57730c7868ccb8bf5c3c5b06a600f3c7bafb854dd1cad3eb4bd85d4ef753f813。仅构建 API 与验收 SDK 工具；Web 和 Auth 保留已验收的各自 source/digest。
- 三节点逐一校验 OCI 归档并导入 digest，没有远程 CI 或镜像推送。

## 新增验证

两项新 Go 测试（含 13 个真实 PostgreSQL 条件子场景，共 15 pass / 0 fail / 0 skip）验证条件优先级、秒精度、原目标不覆盖、缺失目标不发布及非法字段拒绝。vet/编译、TypeScript、Vite、Helm、38 个固定 suite 准入检查通过。首次新 SDK 测试代码误用了 CopySrcOptions 字段名，在 vet 阶段被发现，按锁定 SDK 的真实结构修正；原失败回执保留，没有修改运行服务来适应测试。

真实 UI Job **publication-ui-20261015141420**，16 项检查：

- ui_owned_auxiliary_bucket_created_original_buckets_untouched
- ui_owned_source_bytes_uploaded_once
- ui_scoped_writer_saved_before_actual_sdk_conditional_copies
- real_sdk_copy_source_none_match_different
- real_sdk_copy_source_none_match_same
- real_sdk_copy_source_modified_since_past
- real_sdk_copy_source_modified_since_future
- real_sdk_copy_source_unmodified_since_future
- real_sdk_copy_source_unmodified_since_past
- real_sdk_copy_source_match_overrides_unmodified
- real_sdk_copy_source_none_match_blocks_even_if_modified
- real_sdk_copy_source_none_match_overrides_modified
- real_sdk_copy_source_malformed_date_rejected_without_mutation
- real_sdk_copy_source_tampered_signed_date_rejected_without_mutation
- ui_lists_only_original_and_successful_conditional_copies
- owned_runtime_stopped_compute_zero_original_data_and_evidence_retained

桶 ci-s3-copyconditions-1792073662674，原对象 conditions/source.txt；凭据 ID bcr_889f8bacf08efaa3e1f23c15，Secret 未记录到公开文档。启用/停止 operation 为 op_338a5dff1942acec27679450, op_dcd045b54c737031ecc58902。成功目标逐一通过真实 SDK 读回字节/ETag；失败条件和签名负例均确认目标不存在。回到 UI 查看原对象和成功目标后正常停用 Storage，托管 Compute Pod/VM 为空，目录、数据、PVC 和测试记录保留。

## 升级和限制

升级前控制库备份 SHA256 ae01f1291b90697e28206842b958f6b5a76e96ae089432bf6a5d40018cab4db6；9 个 PVC、779 个既有非 Helm Secret UID、其他 6 个 release 清单保持。Helm 历史不在此次升级中裁剪。

未重复旧 S3/CSV/Auth/Functions 或旧全量 Go 套件。multipart、大对象、完整 checksum/STS、物理 GC、HA/DR、跨实例栅栏和完整可信 TLS 仍分别保留门槛；本增量不构成完整 S3 或官网全部功能认证。维护者证据保留于 control-plane/production/evidence/20261014-auth-mail/copy-conditions-*。
