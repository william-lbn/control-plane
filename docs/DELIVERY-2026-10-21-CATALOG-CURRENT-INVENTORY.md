# 2026-10-21 当前目录身份展示

本轮通过 [当前目录身份合同](CATALOG-CURRENT-INVENTORY.md) 的只读 API、Console 展示及首次记录防覆盖。API/Worker 0.69.0、Chart 0.9.66、控制面 Helm 149；数据库继续使用冻结的 migration 057，本轮无新 SQL 迁移。Web 本轮重建，管理网关沿用 0.66.3 / Chart 0.1.1 / Helm 56，AI 沿用 0.58.0。

## 实际行为

`GET catalog-identities` 保留原 `items` 首次观察历史，新增 `current_bindings`。当前表以不可变代际核对原 OID、名称、Owner 和分支标记，显示来源及身份/意图版本。尚未提交的管理意图显示 `intent_changed`，不会把原观察静默升级。当前绑定没有历史选项快照，明确标记 `options: not_recorded`；密码与 verifier 不读取，凭据引用、UID 和原始管理标记不公开。

已有身份历史的名称/OID不能通过“首次记录”覆盖：UI 不提供候选，API 在原观察前和提交事务内均拒绝。已知同名 SQL 替换不显示为当前可管理对象；旧 052 及新 057 的角色/数据库排除均保留在完整原生目录里。原首次观察恢复入口继续只接收原基线，当前只读表不扩大修复权限。

## 现场验收

两项新增 Go 顶层功能测试及五项受影响既有测试通过，涵盖原 OID、名称碰撞、Owner、超级用户冲突、未知历史选项、代际未提交、排除及首次记录事务拒绝；真实临时 PostgreSQL、vet、OpenAPI、TypeScript、Vite 和套件/API 索引检查通过。临时 PostgreSQL Pod 按原 UID 退休。

五项真实 Linux Console 检查均使用 0.67/0.68 已保留夹具，没有新建客户对象或分支：普通目录当前代际与三条原基线分开；子分支 v4 与无 Compute 派生分支 v1 拥有独立身份、保留 OID 270336；数据库 OID 270344 及继承来源可读；同名替换与角色/数据库排除不能重新首次记录；Console 当前表、来源与完整目录排除提示可见。被拒绝的 POST 没有准入目录操作。

16 张目录表在部署前和现场检查后逐表行数及 SHA-256 完全一致，涵盖原基线、请求回执、持续身份、CREATE/普通/rename 结果、继承来源/观察/排除/发布、目录意图和默认身份。读取没有补造身份或覆盖原历史。只读收尾继续核对 0.68 的 5 份来源、4 份观察/发布、18 条继承身份、10 条排除、136 份初始化观察/发布及 0.67 的 18 条代际和 17 份成功操作结果。三条既有未证明 Endpoint 没有补认领。

最终三个节点及三个 Safekeeper Ready，Pageserver/Broker/MinIO 就绪，VM 0。正常 UI 预加载的默认分支唤醒也纳入公开休眠操作；数据、分支及凭据保留。临时终止 Job 日志归档后按原 UID/resourceVersion 退休。

## 构建与保全

API：`neon.local/control-api@sha256:3d166d4cfa380d0e74b22561057e8ffdd73cef9ece46b5452fdd06f861f3f99f`。Web：`neon.local/control-web@sha256:b4e9ade4b4d4a4edbe2df9514d22ef63296fc4288f53d575adcc162f50a6b9cb`。源码内容摘要 `205691f96a5c3b5e04f2c4d98e39231db7b960c2dfaedd20cbae5e870022cd2c`，归档摘要 `8ff7348681c2478c81f0666798c015ef3ab0cf1e3af368efad5d4f484acd4870`；387 项运行输入与部署快照一致。父提交 928eec95 是历史基线，不能替代实际快照摘要。API/Web 一次 Linux 构建并内网分发到三节点，沿用缓存依赖及其他组件镜像；没有推送 Git 或镜像。Web 私有 NPM 包元数据仍为 0.64.0，本轮新 bundle 由上述新 digest 与源码摘要识别。

24 个 Helm 对象通过镜像/Chart 标签差异检查，9 个 PVC、原有 Secret 身份及其他 6 个发布保留。升级前元数据归档摘要 `fd1dc260fec1e1e58002e55b06c954deb3f10b96a8f5b69c4fb4fece40066f47`，仍在同一物理盘。057 冻结摘要 `1a7cb23f7e4e1f0eb308ca54e3be03a90c819325c198d200c60fbc87f4386dcd` 未变。

私有证据目录 `control-plane/production/evidence/20261021-catalog-current-inventory/`；ui-a, ui-b。原凭据/夹具仅在 Linux 保护目录使用，不下载私有目录或密码摘要，关闭浏览器媒体。此项不关闭整个 C02 或全产品；继续旧身份链、系统身份漂移及 Backend 名称依赖，性能/可靠性与外部并发栅栏等专项仍后置。
