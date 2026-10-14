# 预签名上传本地验收

已部署 API 0.28.0 / Chart 0.9.12 / Helm neon-control-plane revision 77。新增 API 合同有 79 paths / 116 operations；应用 Auth 合同仍为 0.24.0。

源快照 SHA-256 `6ed83562f1a88edcda8819009ea0a1ba88a019fd9bc65e7d8061757338c19e6a`，父 Git `5449a11` 仅记录继承关系，未替代真实部署源快照。

API/Worker `neon.local/control-api@sha256:190ca75cf227f0e2f6a47a150cae0ff508f466822db895a8cebbb69cf54f6239`；Web `neon.local/control-web@sha256:72adcd57199319a1348b664a5e6953e68eab514f8af4d96489d5e32a6364718a`。Auth 与其他运行组件沿用各自已验收摘要，未更换 Neon fork 基础组件。

Linux 定向 Go/race/vet 检查 12 pass / 0 fail / 0 skip。前端类型、构建、Helm、29 套件入口和 API.md 检查通过。前端预检脚本沿用了 19 项既有 unit tests；此冗余已从下一批脚本移除，不再重复旧功能。

UI Job `publication-ui-20261014203336`，9 项检查通过：原生 UI 临时地址签发、遮罩与私有保存、无 cookie 浏览器实际 PUT、错字节/条件/路径负例、重复写入 412、读取真实原文件、实际 SDK PresignedPutObject 与 UTF-8 路径、停止服务和 Compute 缩 0。

保留 bucket `ci-s3-presign-1792010019417`，两个实际上传对象与 private 原 payload/capability、完整操作证据保存。Operations `op_13fdabc771ab6124bb6f0771`, `op_7b08a8a4035cf7cbfe365c78`。全部 9 PVC、768 原非 Helm Secret 身份保留，其他 6 个 Helm release manifest 不变。

[接口与手工使用](OBJECT-STORAGE-PRESIGNED-UPLOAD.md)。证据位于忽略目录 `control-plane/production/evidence/20261014-auth-mail/presign-ui-local-attempt1`，私有 URL、密码与 key 不提交公开 Git。本地成功不代表跨实例/HA/TLS/大对象或官网全部生产门槛完成。
