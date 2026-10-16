# 2026-10-17：数据库与角色重命名

## 1. 交付结论

**C01 自定义受管理数据库/角色重命名基础闭环已通过。整体仍为预览产品；官网全部
功能及生产准入仍未完成。** 日期按 Asia/Shanghai。前一阶段为
[Functions UTC 定时触发器](DELIVERY-2026-10-17-FUNCTION-SCHEDULES.md)。

| 范围 | 实际结果 | 边界 |
|---|---|---|
| API / UI | 两个异步 rename 接口、版本/幂等/当前授权、目录页确认与原请求恢复 | 自定义、受管理、非系统对象；默认连接/Backend 依赖被明确拒绝 |
| PostgreSQL | ALTER ROLE / DATABASE，原 OID、原 SCRAM 密码、原数据/Owner/ACL 保留 | 不复制数据库、不强制断开、不自动收编外部 DDL |
| 恢复 / 并发 | UI 丢失真实 202 后原 key 取回同一 Operation；不同 body 409、旧版本 412 | 控制库事务/协作锁不能代替跨系统 fencing |
| 配置一致 | 所有原 Endpoint 配置与 Proxy 路由用原 UID/resourceVersion CAS；未知 PUT 结果观察恢复 | 活动分支操作序列化；完整 HA 另验 |
| 冷启动 / 收尾 | 原 Writer、改名前创建的原 Reader 冷醒保留原 OID/数据/密码；Reader 正常删除、Writer 归零 | Compute 原策略未改，不声称该 Compute 自动缩零；业务对象与回执保留 |

架构、数据模型、API、五步时序及手动/自动复测见[重命名合同](CATALOG-RENAME.md)。
七组基本功能已有对应回执，其余 28 组仍有代码或独立验收缺口，已验收基本组的
扩展边界也保留。继续 L01 异常 Functions 自动退休及其余需求。

## 2. 精确版本与来源

- API/Worker、Web **0.48.0**；migration **039**；独立 Chart **0.9.40**；Helm revision **112**。
- OpenAPI **109 paths / 157 operations**；公开 live workflow **56** 个可选且被准入的串行套件。
- 构建父提交 `e0e2b81d6e86c839525946096340946eb97c9954`，不是未提交构建源码的完整标识。
- API/Web 源快照 SHA-256 `7b867e2edfc702b8b4766d43cfedf11157f50ec74fa4cebf33a1bd25e4fab77b`。
- API/Worker `neon.local/control-api@sha256:282fbdef52fc518d9a2df10050d53a192a3edbe84d8109e4eebdee7986753d4e`。
- Web `neon.local/control-web@sha256:223e39a135b43a895f00732cf0763c86716586470be4b4020f22408173ef700a`。
- 元数据备份 SHA-256 `d8c5a3f1199ff1d19509ff147f0feb99cc362c227c000963a42901ff8c26a202`。

Linux 串行构建并导入三节点；Helm 升级前备份。9 个 PVC、797 个既有 Secret UID
保留，其他六个 release manifest 未变。未修改组件沿用单独记录的既有 digest。
Functions Guest 保持 `neon.local/control-functions-vm@sha256:24e348fc125f644cfb00ba3335f8a291d3ce5a974e388b17495057ce94ed13c7`，
其 supervisor 派生边界见前阶段报告；本批未重建 Guest。Neon Rust/Autoscaling 源码未改。
公开统一 Helm 0.1.31 仍锁 0.23.0 控制镜像，最新本地镜像尚未统一公开发行。

## 3. 本批回执及失败记录

维护者原证据为 `control-plane/production/evidence/20261017-catalog-rename`，不上传公共
Git；私密密码/checkpoint、源码、OCI、失败日志均保留。`runtime-source-match.json`
逐文件核对实际构建与当前 API/合约/Web 运行输入；文档可在验收后更新。

| 回执 | 结果 | 内容 |
|---|---|---|
| `go-20261016171049` | pass | 初版四个新测试；后续增量见下一回执 |
| `go-20261016172135` | pass | race/vet、五个顶层新 Go/PG 测试，含六个 UID/CAS 子场景；另有三个 OpenAPI 合同检查 |
| `quality1` | fail，未创建 Job | 私有调度脚本用了仅 API 的回执，缺 Auth/E2E 依赖；修正使用合并回执 |
| `quality2` | fail | 新测试闭包的 string/undefined 类型错误，原日志保留 |
| `quality3` | pass | 修正后格式、TypeScript、Vite、56 套件准入及 API 索引；未重跑旧业务 UI |
| `images1` / `images2` | fail | API 构建通过；Web 基础 metadata 被出口代理 CONNECT 503 阻断 |
| `web-local-resolve` | fail | 本地解析模式不能替代当前缺失的基础 metadata，诊断保留 |
| `egress-recovery.json` | 恢复 | Linux gost 下一跳 Windows 端口无监听；恢复仅三个私网节点接入的转发后 TLS/Registry challenge 正常；未换 digest/禁用 TLS |
| `images3` | pass | 同一源码归档、API/Web 两镜像，三节点串行 digest 导入 |
| `deploy1` | pass | Helm 111 → 112；备份和原资源身份检查 |
| `ui1` | pass | 从浏览器发起的十项新功能检查 |
| `retirement-20261016173435` | pass | 三个终态 Job 留档后按 UID/RV 退休；3/3 Ready、87 workloads 符合期望，无坏 Pod，托管 VM 为 0 |

Go/PG 检查包括当前授权、版本、审计失败原子回滚、原 OID/数据/密码/ACL、
元数据未知提交、真实 Kubernetes HTTP CAS 冲突、丢失提交响应、同名 UID 替换拒绝。
一次性 PostgreSQL 测试 Pod 已各自按原 UID 退休；没有访问现场业务库执行破坏测试。

真实 UI Job：`publication-ui-20261016173221`。沿用既有项目
`prj_6f3c51bd15b772e3`、分支 `br_6f3c51bd15b772e3`、Writer
`ep_6f3c51bd15b772e3`，只新增本用例专用目录对象。角色
`rename_role_v1 → renamed_role_v1` 的原 OID **147456**，数据库
`rename_database_v1 → renamed_database_v1` 的原 OID **147460**。
`public.rename_probe` 原数据和原密码保留，密码未写公共回执。

十项实际 UI 检查：

1. 创建专用角色。
2. 创建专用数据库并指定其 Owner。
3. SQL 写入并记录原 OID、业务行和 ACL。
4. 改名前创建原 Reader `ep_94e4cd7ef1f630f0` 并归零。
5. 角色改名丢失真实 202 后原 key 重取同一操作；不同 body 409、旧版本 412。
6. 新角色名、原密码实际登录；原 OID、Owner、数据和 ACL 保留。
7. UI 数据库改名，五步操作成功。
8. Writer 缩零后用新名称冷醒，原 OID/密码/业务数据保留。
9. Writer 再归零，原 Reader 新名称冷醒；实际只读 SQL、原 OID/Owner/密码/数据一致。
10. Reader 正常删除、Writer 归零，业务表、角色和数据库保留。

原 Operation：

```
op_cf0225882dd2c7bf10fddc9f
op_449ecbab7c8ac958ecf9f7b1
op_aa9b50ccb4c5f58c3f5ac10c
op_827e7b00f20a5f2ad3a7ab4e
op_9a6acdc10ee54dd746f2920c
op_b7edccc3c8fa09e0d05e3a0f
op_a4059a194117fbc407b8bd29
op_afc80219c21b2d558766833c
op_ed6feac804d31657ab3700ab
```

角色改名为 `op_9a6acdc10ee54dd746f2920c`；数据库改名为
`op_b7edccc3c8fa09e0d05e3a0f`。未另建替代对象；未修改共享 `fn_native`、
原 Backend 身份或旧 fixture 数据。

## 4. 下一阶段

继续按[需求账本](REMAINING-REQUIREMENTS-2026-10-16.md)开发和本地验收。
L01 中 Node 异常不能因 `NodeReady=false` 永久占用 VM；应根据可信 supervisor
活动观察、原身份/UID 及正常 GC 退休。TTL、失败创建清理、默认身份、WS/对象事件/
完整日志、大对象/GC、OIDC/SSO、外部 fencing、HA/DR、TLS、长期监控和统一公开
发行仍未全部完成。前阶段通过不能关闭这些独立门槛。
