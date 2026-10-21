# Compute 冷启动目录保留 · 2026-10-21

API/Worker 与管理网关 0.66.3，Web 复用 0.64.0，AI Gateway 沿用 0.58.0。
迁移 055，控制面 Chart 0.9.63 / Helm 146，管理网关 Chart 0.1.1 / Helm 56。
初始化后冷启动目录保留及新角色的明确原生管理请求通过本地功能验收。
C02 的当前 OID/凭据代际、完整系统身份漂移及 Backend 名称依赖继续；整个
C02 未关闭。性能、可靠性、HA/DR 与外部 fencing 专项按用户要求后置。

## 行为

此前冷启动和角色管理会重放全量角色、密码、数据库 Owner 与 delta 配置，
可能覆盖同名替换或外部 SQL 变更。现在首次 SQL 初始化和继承处理完成后，
记录原创建操作、项目/分支/Endpoint/native scope 与配置/凭据 Secret UID，
以固定 UID/resourceVersion 设置 `skip_pg_catalog_updates=true` 并读回确认。
首次初始化仍正常执行。原创建重试恢复同一见证，不退回目录重放。

旧 Endpoint 需要唯一成功原创建操作及成功 SQL 探测，再明确记录当前 backing
的首次观察边界；不补认此前原生对象或 UID 历史。七个旧凭据缺少独立
`probeVerifier` 字段，使用原受保护密码在内存核对已有有界 SCRAM，不改凭据。
132 份初始化记录和发布确认一致，均使用同一个已验证的固定
PG16 Guest。另三个导入端点缺少原记录/完整归属，配置与数据保持原样，未认领：
`ep_9526ada8cd0b049d`、`ep_210df4e9b12a46c6`、`ep_eb64544afb684be0`。

新建角色的原生管理接口也遵守该跳过选项，因此明确创建使用临时请求：仅新
角色与 `ADMIN control_probe`，数据库和 delta 均为空，临时启用 apply；然后
恢复跳过目录更新的完整运行视图。持久配置保护始终启用。现存同名对象、异常
数据库清理与缺失平台权限角色会被拒绝。管理网关只允许这一精确例外，保留
原 TLS/SNI/Endpoint JWT、所有已知及未知非目录字段和拓扑校验。

完整语义、原生引擎幂等平台维护阶段及升级边界见
[初始化合同](COMPUTE-CATALOG-BOOTSTRAP.md)。这些记录不为普通 CRUD 建立完整
当前原生身份代际，也不会自动把外部 SQL 密码接入 Proxy。

## 实际验证与保留数据

新增五个 Go 顶层测试：初始化/元数据两个、单角色投影一个、网关准入及实际
handler 转发两个；另一个已有拓扑测试受影响重跑。包括 15 种原创建/重试
状态、16 个初始化拒绝场景、9 个原生投影拒绝场景、17 个网关请求负例，
以及错误 Endpoint JWT/SNI/拓扑请求不转发。编译、vet、机器合同通过。
所有 Go 业务执行在 Linux，临时 PostgreSQL Pod 已按原 UID 退休；纯投影/
网关测试没有额外创建 PostgreSQL Pod。

真实 Console 会话、API、SQL 和页面检查在原夹具累计通过五项：

1. `fresh_writer_and_reader_bootstrap`
2. `owned_original_objects_created`
3. `explicit_sql_replacement_and_owner_options_observed`
4. `original_reader_and_writer_cold_preserve_sql_catalog`
5. `original_customer_data_and_zero_computes`


新分支 `br_597fe8e507cbd066` 的首次 Writer/Reader 及 Writer 冷启动检查在
0.66.1 通过；对应四份初始化运行输入与最终快照逐字一致，复用此检查。
其余受影响路径在 0.66.3 完成，沿用同一分支、Reader、角色操作和私有 nonce。
两条原失败操作 `op_afaf04d8cdec2ed4bbbba4ab` /
`op_a65fc2cf2cf907c1222eef38` 均重试原 ID，未创建替代夹具。

原角色 OID `262145` 保留在 SQL 改名后的名字下；原名字由
独立 OID `262157`、新 SQL 密码及不同属性的角色占用。数据库
OID `262149` 的 SQL Owner 改为 cloud_admin、连接上限
改为 17。之后再明确新建另一个平台角色，原密码摘要、OID、选项和 Owner
仍保持；原 Reader 与 Writer 真实冷启动后再次一致。
密码证明比较原生摘要，未把摘要或明文密码导出到公开结果，也未声称该外部
密码已经成为 Proxy 登录凭据。`retained-cold-catalog-data` 和原客户
`retained-c02-default-data` 保留。

原 0.65 四条 rename 结果/成功审计/各五步、0.64 六条目录原结果仍保留；
原角色/数据库元数据版本 7/8、最新凭据 UID 和固定配置/路由 UID 匹配。
此前最新名字/密码的原生证据按原版本保留，本次没有冒称重新执行旧整套验收。
049 三条历史基准、默认 Owner platform/version 3、默认连接 platform/version 5、
根业务目录及原 Function 定义保留。

最终三节点、三个 Safekeeper、关键存储/控制服务就绪，所有 Compute 为零，
Function 非退休实例为零。八个确认为本批的终态质量/UI Job 已保存日志并按
原 UID/resourceVersion 退休；未删除业务数据、Secret、PVC 或历史证据。

## 失败修复与来源

0.66.0 首次创建收尾误用了 Endpoint `creating`，实际模型是 `provisioning`；
修复包含原创建 `error` 状态重试及独立父资源状态检查。0.66.1 发现原生角色
管理也跳过了 apply；0.66.2 的单角色请求又被旧网关冻结字段校验拒绝，最终
0.66.3 增加精确网关例外。各候选、原失败操作和日志保留，未用新对象掩盖失败。
UI-a 使用旧提交而未找到新测试、Go 测试夹具约束/TLS SNI/JSON 类型及隔离
文档脚手架失败均有独立记录；修复后才进入对应下一步。

最终 API 与管理网关使用同一不可变源码 SHA256
`c8329e3cae58b8eb4fc8f2df2b4251b962afc9ecf6faf0cb7f2fb93266c51338`，归档 SHA256 `2adb7f05f95a8ca0c3315913db36aea5e4cdb1f600d88f2dbcd278f746895f15`。
Git 父 HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07` 不是本次运行源码提交。
379 份当前运行输入与最终快照一致。最终构建 API/Gateway 两份
OCI，仅从构建节点向另两节点内网分发并核对 digest；前面确需修复的候选各有
独立记录。Web 的 88 个 Docker 输入及 builder lock 与已验收 0.64 快照相同，
未重建 Web；新测试脚本与 quality-c 完全一致，复用 TypeScript，Linux 单独
生成/检查 0.66.3 的 127 路径/181 操作索引，73 个已公布套件入口保持一致。

| 组件 | 运行镜像 |
| --- | --- |
| API / Worker 0.66.3 | `neon.local/control-api@sha256:9e671f5c1c53363c127d02088f95ecc1210f4e4ac6c23e93a61430c1d1609bfa` |
| 管理网关 0.66.3 | `neon.local/control-gateway@sha256:cca8f70a3301af803a44cbdebb35c46c574e1660495676bbb66ac7581b859d71` |
| Web 0.64.0（复用） | `neon.local/control-web@sha256:18a33f726ec895bd9690df0fa5ac8e6618572da15ceb9c63c2ca8e64a839af8e` |
| AI Gateway 0.58.0（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

控制面升级前 Linux 元数据备份 SHA256 `fd6677cd7d27e09de875064926d081c41f91f026733a6248f9d24f6e4e1a546a`。
24 个控制面对象语义核对限制镜像及 Chart 标签；独立网关 5 对象核对仅允许
目标镜像/可选 Chart 标签，Deployment UID、RBAC、端口和 TLS 形态保持。
9 个 PVC、889 个既有非 Helm Secret UID 保留；
网关另核对包括 Helm 历史在内的 1014 个 Secret UID。
控制面和管理网关两项有意升级，其余五个 Helm 发布保持原清单。
备份仍在现有物理盘范围，未据此宣称独立 DR。没有代码/镜像推送或模型下载。

私有证据 `production/evidence/20261021-compute-catalog-bootstrap` 包含最终
`accepted-functional-slice.json`、`ui-e/result.json`、只读证明、三批通过 Go
回执、Web 复用/API 索引、各候选、部署与精确退休记录。密码和原生摘要比较
输入继续留在 Linux 保护目录。账本仍为 13 基础、22 继续，继续补齐业务逻辑。
