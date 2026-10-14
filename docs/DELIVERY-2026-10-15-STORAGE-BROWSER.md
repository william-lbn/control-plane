# CORS 与对象元数据交付

API 0.29.0（80 paths / 119 operations）、独立 chart 0.9.13，Helm neon-control-plane revision 79 已部署。本批均为 Linux 本地构建、三节点 digest 导入与新功能验证，无远程 CI 触发或 registry push。

API/Worker `neon.local/control-api@sha256:d7ebd8090885734deea04344931c5a90d0bc384ed81e3559d2decd7f07360a6a`，实际源 SHA-256 `ef794cef5cac2444331eeabf1ef0efcfe4a8b3a5b6cf6d3e77206bdf908891b1`；Web `neon.local/control-web@sha256:974282a7979f43a8054b772855c672502727e86fbb06efd82a2a235b836a1a19`，保留其实际源快照 `bb9aee299ea718eb24e916b3385a7268b44a85815ece70434110d572174257b0`。父 Git 5449a11 是继承关系，不能替代上述源快照。Auth 和其他组件保持各自已验收镜像。

Linux 4 个定向 PostgreSQL/元数据/CORS/COPY 检查通过；新增实际 SDK CORS 签名检查通过，含 XML 篡改、MD5 篡改和普通 unsigned object PUT 拒绝。CORS policy 的真实 SDK GET/PUT/DELETE 和删除后 nil 在真实 PostgreSQL 检查中通过。前端类型/构建/Helm、30 套件入口与 API.md 检查通过，未重复旧 unit/UI 功能。

初次 UI Job publication-ui-20261014205337 已通过原生 policy、上传/读取 metadata、保存 writer；真实 SDK SetBucketCors 使用 unsigned payload + signed Content-MD5，原校验器拒绝。修复仅允许最多 64 KiB 的 signed-MD5 `PUT ?cors`；普通 object PUT 未放开。原失败证据完整保留。

原案例续测 Job publication-ui-20261014210458 共 12 项检查通过，复用先前 5 项及原 bucket/credential/enable operation，没有重复原 mutation。真实 MinIO SDK 配置和 metadata PUT/HEAD/COPY 通过；Chromium 对不允许 origin 自动预检拒绝，对允许 origin 自动 OPTIONS+PUT 成功且可读取 exposed ETag，实际 HEAD 校验 browser metadata。最后 Storage 停止、Compute 缩 0。

保留 bucket `ci-s3-browser-1792011220196` 全部文件/属性/CORS 以及私有原输入、签名 URL、日志和失败记录。Operations `op_9fcf1d7bb4dacd0811fdbeab`, `op_163f4b6df8be8dcf8be234a0`。部署期间全部 9 PVC 和 769 原 Secret UID 保留（包含原案例运行中的临时 compute Secret）；其他 6 个 Helm release manifest 不变。正常停止之后仅回收本案例临时运行资源。

[接口、模型和手工复测](OBJECT-STORAGE-BROWSER-METADATA.md)。实验 HTTP、本地预览及大对象/multipart/physical GC/HA/TLS 的独立门槛保持，未宣称官网全部功能或生产准入。
