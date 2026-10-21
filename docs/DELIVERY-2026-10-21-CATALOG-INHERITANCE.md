# 2026-10-21 首次启动保留继承目录

本轮通过 [继承目录合同](CATALOG-INHERITANCE.md) 的首次启动保留功能。API/Worker 0.68.0、migration 057、Chart 0.9.65、控制面 Helm 148；Web 复用 0.64.0、管理网关沿用 0.66.3 / Chart 0.1.1 / Helm 56、AI 沿用 0.58.0。该结论不关闭整个 C02 或全部产品需求。

新分支首次原生初始化只接收两个平台身份，不接收客户角色/密码、数据库 Owner 或删除意图。SQL 可连接后依据原创建操作冻结的来源、实际 tenant/timeline、原 OID 与凭据 UID 发布受保护视图。已有成功 SQL 初始化记录的分支，新增或替换 Compute 从第一次启动即跳过目录更新。无可信初始化记录的旧 Writer 不会被当成成功初始化。

继承身份绑定使用子分支独立 identity_id，引用原父分支不可变身份代际并保留 OID；凭据固定原 Secret UID。已知缺失、同名替换、Owner 漂移或服务权限缺失保留原 SQL 对象并排除平台管理，不通过改密、改 Owner、授予权限或删除来迎合意图。没有原 OID 证明的旧意图不会被补造成历史身份。

## 实际验证

- 8 个新增 Go 顶层功能测试和 9 个受影响既有测试通过；真实临时 PostgreSQL、vet、OpenAPI、TypeScript、套件与 API 索引检查通过。临时 PG Pod 按原 UID 退休。
- 新子分支保留父分支角色 OID 270336、数据库 OID 270344、原客户密码和 `catalog_binding_probe` 数据；已知独立 SQL 替换的 OID、密码摘要、LOGIN/INHERIT、连接限制、Owner 和注释在 Linux 内存中对比一致，摘要不导出。
- 使用父分支拥有的原凭据 UID 完成子分支角色 rename，再独立改密和切换数据库 Owner；父分支原名称、密码和数据保留。
- 新 Reader 与 Writer/Reader 冷醒保留最新客户密码和目录。无 Compute 子分支在创建时冻结原凭据代际，父分支后续改密不改变该子分支首次 Writer 的凭据。
- 删除本轮自有 Writer 后，同分支替换 Writer 保留客户 OID、密码和数据。原数据 Writer 退休，分支及 timeline 保留。最终父分支和三个新增存活 Compute 均休眠，集群 VM 0。
- 只读收尾核对 5 份来源、4 份观察和 4 份发布、各 4 条唯一观察/发布审计、18 条继承身份记录及 10 条分支排除；136 份初始化观察/发布一致。原 0.67 的 18 条身份代际及 17 份成功操作结果保留。三条既有未证明 Endpoint 未被补认领。

## 构建与保全

API 镜像：`neon.local/control-api@sha256:8b378c04585f1ecf63e354a5656998f8bec62d7ca25c16c705a1f1def672ff7e`。源码内容摘要 `c3519004b1d283eeb791a61f4160a89a972411afd9a7d702c40fdbc729ef0039`；归档摘要 `696156fb011f84bccbc4ce14a61f68caf2ceb6852ca2ab168161a82a6b1bb14c`。父提交 928eec95 仅为源码历史基线，实际部署使用本地不可变快照；386 项运行输入与部署快照一致。Web 的 88 项完整构建输入一致，因此复用既有镜像。只构建 API，一份镜像内网分发并在三节点核对 digest；没有推送镜像或 Git。

Helm 渲染语义检查通过 24 个对象，部署保留 9 个 PVC、897 个已有 Secret 身份及其他 6 个发布。升级前元数据归档摘要 `4e17d75ebf0aaeffdd3af54448ecd393d6fe5421f79a1cadb79b259de61301a1`；归档仍在同一物理盘，不构成独立灾备。057 应用后冻结摘要 `1a7cb23f7e4e1f0eb308ca54e3be03a90c819325c198d200c60fbc87f4386dcd`。三个节点及三个 Safekeeper Ready，Pageserver/Broker/MinIO 就绪。本轮临时终止 Job 的日志归档后按 UID/resourceVersion 退休，不删除业务数据、Secret 或 PVC。

私有证据目录为 `control-plane/production/evidence/20261021-catalog-inheritance/`。原 fixture、操作 ID、来源及失败检查点在 Linux 保护目录续用；final-readonly-proof-a, ui-a。密码和独立 SQL 对象的密码摘要不进入公开报告，浏览器媒体关闭。

## 后续范围

继续当前身份目录展示、旧 049/053/054 代际衔接、完整系统身份漂移及 Backend 名称依赖迁移。没有来源快照的旧无 Compute 分支和历史时点客户目录可信导入仍须单独处理。本轮不宣称所有原生初始化副作用、整个 C02、性能/可靠性、外部并发栅栏、HA/DR 或全产品已完成。
