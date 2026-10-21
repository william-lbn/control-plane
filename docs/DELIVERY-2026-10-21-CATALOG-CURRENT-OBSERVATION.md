# 2026-10-21 旧目录身份衔接

已通过 [旧目录身份与当前代际衔接合同](CATALOG-CURRENT-OBSERVATION.md)。API/Worker 0.70.0、Web 本轮重建、migration 058 / Chart 0.9.67 / Core Helm 150。Gateway 0.66.3 / Chart 0.1.1 / Helm 56、AI 0.58.0 保留。

## 功能结果

继承的旧角色可凭原 057 已发布来源、成功 SQL 观察、原 OID 和原不可变 Secret UID 完成首次观察，即使凭据仍归属父 Compute。复制标签的新 Secret 无法替代原 UID；当前意图已变化或原观察不完整时拒绝。

新增 `POST catalog-identities/{binding}/observe-current` 和控制台“衔接当前身份”。显式核对原首次观察的租户/timeline 与 OID、当前名称/代际/标记，以及数据库当前 Owner 身份。原角色 UID 或原成功改密/rename 凭据 UID 必须匹配，改名需原成功回执。058 不可变观察、持续身份、幂等回执及审计原子提交；后来改名、改密后原键仍返回原观察回执。

原首次观察和旧普通操作结果不修改；不会为旧普通结果补造 OID 历史。读取密码为 `not_observed`，历史选项为 `not_recorded`。衔接不执行 SQL 修复、重建或数据/ACL 迁移，也不覆盖已有持续身份历史。

## 已执行的功能验证

Linux Go 与隔离 PostgreSQL 共八项顶层功能测试：四项新增、四项受影响。覆盖继承原 UID/OID、无原发布/SQL 证明拒绝、复制当前标签的替代 Secret 拒绝、原生范围/Owner/标记冲突、未知历史选项、原 053 凭据 UID 证明、审计失败原子回滚、过期代际、重复衔接、禁用用户与原请求重放。vet、OpenAPI、TypeScript、Vite、77 套入口索引及 API 索引通过。临时 PostgreSQL Pod 按原 UID 退休。

五项真实 Console/API 功能检查复用 0.68 的原继承分支，没有另建客户分支：

1. 旧角色 OID 196608 与数据库 OID 196612 首次观察，保留原父凭据背书及客户表数据。
2. Console 显式衔接角色，再衔接数据库，当前表展示新来源且原历史独立保留。
3. 原角色改名、密码轮换、原 OID/数据库 Owner 与客户数据保留；原观察键在后来代际重放原回执。
4. Writer/Reader 正常休眠后重新连接，客户身份和数据仍在，原父分支名称及客户数据未变。
5. 原五个 Compute 及正常 UI 预加载唤醒的默认分支正常缩零。

只读收尾核对两条 058 观察、两条首次基线、四条观察请求回执和两条成功审计；冷配置与 Proxy 路由采用当前角色名称和凭据，旧名不残留。目标子分支之外的 16 张目录表范围前后摘要一致；目标范围的两条原基线保持第一代，新增身份与后来受控操作另存。

同时复核原 0.67/0.68 身份、继承、排除及私有配置 UID，不重复原业务测试。三个节点及三个 Safekeeper Ready，Pageserver/Broker/MinIO 就绪，最终 VM 0。原夹具、分支、凭据与数据保留，终止测试 Job 归档后按 UID/resourceVersion 退休。

## 构建与来源

API：`neon.local/control-api@sha256:290b96415921fd5000ca7ab7fa551a0c0873b6cfabad09d2da6b4d212e47bd03`。Web：`neon.local/control-web@sha256:2c0efb67823d2c20eec639872524e0731bac1269324db9aa8c35d07ff7f77394`。

源码内容摘要 `9b6b110b97b2914363c522bed4cc84781a0c6b1be7e51c5c8bc37e9dcdc8b537`；归档摘要 `36fcdd8bd81b6817aaee4aea893c1a9df226daaf4651cc49e76d783088a70b17`。391 项运行输入与不可变构建快照一致。父 Git 提交 69c611d 只标识基线，实际部署由这些摘要识别；Web 私有包版本 0.64.0 不替代新 bundle 摘要。

API/Web 各构建一次，复用原缓存依赖并在内网分发三节点。24 个 Helm 对象通过仅允许镜像/Chart 标签变化的检查；9 个 PVC、原有 Secret 身份和其他 6 个发布保留。升级前元数据归档摘要 `3052e13600189a75ae3cddf59ee7a8ff1ee6e3865612c83cdda1f6f1b386f10b`，仍在同一物理盘。058 冻结摘要 `ef77ef8aa674bba9f5a75750d944ba3bb297d988b8aa9a689c1ad99dccdaf282`。

本轮 ui-a 在 K8s API 中断时停止，保留两项通过记录；同一时段宿主机有 NVMe 设备重置记录。三节点 API 恢复后，原失败改名通过同一个操作 ID 重试成功，再由原检查点续测，不另建夹具或重放已通过步骤。设备重置的更深层原因未确定，此次恢复不构成硬件或可靠性认证。

私有证据目录为 `control-plane/production/evidence/20261021-catalog-current-observation/`，原凭据和私有检查点仅在 Linux 保护目录使用。失败批次：ui-a。此增量不关闭整个 C02；默认/系统漂移和 Backend 名称依赖继续，性能、可靠性、HA/DR 与外部并发栅栏后置。
