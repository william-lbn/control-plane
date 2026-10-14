# 元数据编辑与 S3 REPLACE 本地交付

API 0.31.0 / metadata migration 030 / chart 0.9.15 / Helm neon-control-plane revision 81，82 paths / 121 operations，新增明确的响应 schemas。API/Worker `neon.local/control-api@sha256:8406fde23c811895c60fbe1dd646e004d9c31c1ba2c5fd5138b41c3502392d47`（源 `5087aa280acec2d1c6e15dbc483247425574899c7080ae95c541b3cec314a391`）；Web `neon.local/control-web@sha256:c4894078c95fe8939654aeadc6dbfd792c0c404fa311422699fff80572e228a3`（源 `dda8b191a969742944c8259c46d24168ae1ee15cb3684c46c95a3f484596fef7`）。Auth 与其他镜像各自沿用已验收来源。父 Git 5449a11 只是继承关系。

Linux 新 PostgreSQL 事务检查通过：元数据状态 ETag 条件修改、旧标签拒绝、跨 branch 标签拒绝、自复制 REPLACE、错误源 SHA 拒绝与原 blob 引用保留。前端类型/构建/Helm、32 套件入口/API.md 检查通过；没有重跑旧 UI/unit 功能。

真实 UI Job publication-ui-20261014222421 8 项通过：原生创建专属桶、一次文件/属性上传、UI 修改真实属性、旧 metadata 标签 412 且原文件 bytes 保持、实际 SDK self-copy REPLACE、内容 ETag/bytes 保持、UI 显示 SDK 已存属性、停服务/Compute 缩 0。

保留 bucket `ci-s3-metaedit-1792016664268` 的 metadata/edit.txt、实际属性与 private 原 payload/输入/所有证据；Operations `op_e1b78f98077ff63ddfdcea88`, `op_42cc8999ec40f63cb09031a5`。最终 managed compute Pods/VM 均空。9 PVC 与部署时 773 原 Secret 身份保留，其他 6 Helm release manifest 不变。

[接口、模型与手工复测](OBJECT-STORAGE-METADATA-EDIT.md)。当前仍是本地预览；multipart/大对象、完整 COPY 源条件、GC、跨实例栅栏、HA/DR、全链路 TLS 尚有门槛。未运行远程 CI 或发布 local image，后续整批统一发布。
