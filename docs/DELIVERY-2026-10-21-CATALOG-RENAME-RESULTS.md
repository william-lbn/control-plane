# 重命名操作原结果恢复 · 2026-10-21

API/Worker 0.65.0，Web 复用 0.64.0，AI Gateway 沿用 0.58.0，migration 054，
Chart 0.9.59，Helm 142。角色/数据库 rename 的原结果恢复通过。
普通 CRUD 与冷配置的当前原生 OID/凭据代际、完整默认/系统身份漂移及 Backend
名称依赖迁移继续，C02 未关闭。性能、可靠性、HA/DR 和外部 fencing 专项后置。

## 行为

之前 rename Worker 会先读取原 immutable 凭据，然后重新执行原生改名/密码
和配置发布。现在完成目录改名、关联数据库 Owner 名称/版本更新、不可变
原结果与成功审计同事务提交。回执绑定完整原 payload：原 OID、旧/新名称、
原版本、凭据引用/UID、全部配置 UID 和原全局路由 UID。

原结果已提交时，只补同一原操作的五个步骤，先于凭据、SQL、配置或路由读取；
不恢复旧密码、旧名字，不推进后来的版本。后续合法轮换/再次改名、原名称被
新对象使用或原 Writer 退休，都不把原回执变成重新执行旧意图的授权。
新执行与提交重查原租约/payload、当前 actor 权限、父资源、固定 Writer、
原 Endpoint 集合、依赖以及原 updating 行的版本/引用；旧操作看起来已完成的
新名称不能制造结果回执。054 只增加结果表，不回填历史未知状态。

原 native rename 的 OID 后置检查、冷配置/Proxy 固定 UID/CAS 与依赖拒绝保持
原合同，见[目录重命名](CATALOG-RENAME.md)。这里的结果不是完整当前原生身份
代际表，也不能替代外部 DDL 栅栏。

## 验证与原数据

一个新增 Go 顶层测试的角色/数据库两个动作子用例通过：原内容/OID/UID/租约、
失效 actor、父资源退休和 legacy 推断拒绝；审计失败回滚改名、关联 Owner 与
回执；历史不可更新/删除。后续名字、凭据、版本及独立旧名已变化、原 Writer
退休、actor 失效时，直接调用实际恢复驱动；没有 Kube 客户端和 Proxy 仍能
恢复原五步，且成功审计只有一次。编译/vet/API 合同通过。

Web 的 88 个 Docker COPY 输入与 builder lock 逐一匹配 0.64.0 的原不可变
快照，合并时再次核对；复用其已验证运行镜像，没有再构建或冒称新 Web。
新测试的格式/TypeScript、181 个 API 操作索引和 72 个受保护入口通过；旧
Web Vite 构建与不相关业务套件未重复。

继续上一片保留的角色/数据库及同一原 Writer、Reader，五项真实检查通过：

1. `retained_original_catalog_scope_and_oids`
2. `later_role_name_password_and_original_rename_receipt`
3. `later_database_name_and_original_rename_receipt`
4. `original_reader_writer_cold_latest_names_and_credentials`
5. `retained_customer_data_and_all_computes_zero`


角色从 `c02_receipt_1792522761772` 两次改名至 `c02_receipt_1792522761772_final`，中间合法轮换密码；
最终角色版本 7，实际最新密码登录成功。数据库从 `c02_receipt_1792522761772_db`
两次改名至 `c02_receipt_1792522761772_db_final`，关联 Owner 同步，最终数据库版本 8。
重放第一次改名的原 HTTP 内容/版本/键仍返回原操作，未撤销后续名称和密码。
原 OID `245760` / `245764`、
`retained-catalog-receipt-data` 和原客户数据保留。原 Reader、Writer 分别
从零冷醒核对最新名字、Owner、密码、OID 和数据，最终均回零。

四条 rename 原结果、四条成功审计、各五个成功步骤一致；合法轮换的 053 原
结果为版本 6，上一片六条普通目录结果仍保留。原 Reader/Writer 配置和全局
路由 UID 保留，最新角色 verifier 与当前 immutable 凭据 Secret UID 一致，
两个旧角色名/库名没有留在冷配置，旧角色名不在两条 Proxy 路由中。
049 三条原基准的 OID/版本 2、默认 Owner platform/version 3、客户连接默认值
platform/version 5、根业务目录和原 Function 定义均保留。

最终三节点、三个 Safekeeper、关键存储与控制面就绪，VM 0，Function 非退休
实例 0。两个终态质量/UI Job 按原 UID/resourceVersion 归档退休，Go 临时 Pod
也已精确退休；没有删除业务数据或 Secret。

## 来源和部署

本次 API 源码快照 SHA256 `b6030996e44495e63bab9e6a86633f60943bd3398d6c2ad69f4d1a3feb531809`，归档 SHA256
`356e9481751832146ac11a1dcc2a0467cbe29e5e6300f502961a060f8489bae6`，Git 父 HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07`
不是运行源码提交。376 个当前运行输入与本次 API 不可变快照
一致。复用 Web 的原源码 SHA256 `b9f8062ff9c67b8bc51a20dac4785c5a06ef5bb99da53ac01d1d3552c6bc6127`
和独立输入匹配证据；不同组件的构建来源明确保留。

| 组件 | 镜像 |
| --- | --- |
| API / Worker 0.65.0 | `neon.local/control-api@sha256:9c29536e76ec6d1bebfa89e98f5037943872df8c3f25503526cc49314a8a9180` |
| Web 0.64.0（复用） | `neon.local/control-web@sha256:18a33f726ec895bd9690df0fa5ac8e6618572da15ceb9c63c2ca8e64a839af8e` |
| AI Gateway 0.58.0（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

仅构建一次 API/Worker OCI，通过内网分发到三节点并校验，无 Git/镜像推送，
无模型或旧组件重新下载。049–053 摘要保持原值，054 冻结 SHA256
`d2f0343f5fa90796fafdad4cc58b8f0fb631802c5eef234ab19a252627996fd6`。升级前 Linux 元数据备份 SHA256
`cace91b246c2d6148d539f4387dcf144b6fee9446700cecb565da79dfefb1e72`。24 对象语义核对限制相关镜像/Chart 标签，
Web 镜像保持原 digest，保留 9 个 PVC、882
个既有 Secret UID、其他六个 Helm 发布、AI 策略/挂载和 Functions 公共配置。

私有证据 `production/evidence/20261021-catalog-rename-results` 包含
`accepted-functional-slice.json`、`ui-a/result.json`、原生最终只读核对、
`go-20261020192704/result.json`、质量、Web 输入复用、冻结、镜像、部署与
精确退休回执。密码仍只在 Linux 保护输入中，未下载到本地结果或公开源码。
账本仍为 13 基础、22 继续，继续补齐完整业务。
