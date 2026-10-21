# 普通目录操作原结果恢复 · 2026-10-21

API/Worker/Web 0.64.0，AI Gateway 沿用 0.58.0，migration 053，Chart 0.9.58，
Helm 141。六类普通目录操作的原结果恢复功能通过。普通 CRUD 的当前原生 OID
谱系、rename 原结果恢复、默认/系统身份完整漂移及 Backend 名称依赖迁移继续；
C02 未关闭，性能/可靠性、HA/DR、外部 fencing 专项后置。

## 变化

之前普通目录 Worker 在重试时会重新应用 SQL/旧 SCRAM/Owner，再增加管理
版本。现在新操作绑定准入时的目录版本和原凭据引用；角色创建/轮换还绑定
immutable 目标凭据 Secret UID。执行与提交检查原操作 payload/租约、父资源、
固定 Writer、原过渡状态/版本/凭据引用及 Owner。管理状态/版本、不可变原
结果与成功审计同事务提交；审计失败一起回滚。

原结果已经提交时，恢复只补同一原操作的四个步骤，先于任何当前凭据、SQL、
配置或路由读取，不重复密码/Owner 发布，也不推进后来的目录版本。原 HTTP
幂等请求的解析移到当前 Writer 查找之前，保留已退休 Writer 的原回执。
原请求仍须满足当前授权及合法输入规则，不能改变键下的内容。

053 只增加不可变结果表，不根据当前 ready 状态回填旧操作。缺少准入版本的
历史未知结果保留并拒绝自动重放；部署前只读核对没有此类未完成旧目录操作。
该结果表不等同于所有目录对象的当前 OID/凭据代际表，也不提供外部 DDL 栅栏。

## 验证

一次性迁移元数据库中的一个新增 Go 顶层测试、六个动作子用例通过：
create_role、rotate_role_password、delete_role、create_database、
update_database_owner、delete_database。覆盖原租约/内容/父资源/凭据代际拒绝、
审计回滚、回执不可修改/删除、后续目录版本与原 Writer 退休。直接调用实际
恢复驱动时 Kube 客户端为空、没有 Proxy 地址，恢复仍完成，证明未访问外部
依赖；HTTP 原回执也不要求当前 Writer。编译/vet/API 合同通过。没有重复所有
旧 SQL 业务套件，未将六个元数据子用例表述成六次新增原生 SQL 演练。

格式、TypeScript、Vite、181 个 API 操作索引和 71 个受保护测试入口通过。
首个质量记录中的新测试未使用变量已修正，失败回执保留。继续同一 C02 分支
`br_7d4c985f85c13039` 和原 Writer/Reader，同一夹具累计五项真实 Console/API/SQL 检查通过：

1. `original_role_database_and_retained_data`
2. `later_password_and_original_http_receipt`
3. `later_database_owner_and_original_http_receipt`
4. `latest_password_owner_and_data_on_original_cold_replicas`
5. `original_customer_data_and_all_computes_zero`


本次专用角色 `c02_receipt_1792522761772` 和数据库 `c02_receipt_1792522761772_db` 保留为后续夹具。
原生 OID 分别为 `245760` / `245764`。
两次密码轮换后角色版本 4，旧请求重放后最新密码实际登录成功；两次 Owner
迁移后数据库版本 4，旧请求重放未撤销最终 Owner。原 Writer/Reader 分别从零
冷醒，最新密码、Owner、OID 和 `retained-catalog-receipt-data` 一致。

六条不可变原结果、六条成功审计、每条原操作四个成功步骤逐一相符；所有原
Reader/Writer 配置 UID、全局路由 UID 保留，其角色凭据均匹配最新 immutable
凭据 Secret UID。原 049 三条基准仍为原 OID/版本 2；默认 Owner platform/
version 3、客户连接默认值 platform/version 5、根业务目录计数及原客户数据
保留。最终三节点、三个 Safekeeper 与关键存储就绪、VM 0，原 Function 定义
1、非退休实例 0。4 个终态质量/UI Job 按原 UID/
resourceVersion 归档退休，一次性 Go Pod 也已精确退休，没有删除业务字节或
Secret。原结果驱动恢复的依赖隔离由上述 Go/PG 用例验证，现场未注入宕机。

首个 UI 运行通过前三项后，冷 Reader 查询返回 503。随后集群出现选举丢失与
RKE2 自动重启；宿主机 03:08:45 的 stornvme 129 事件记录 NVMe 控制器重置，
与虚拟机长时间停顿相关。根文件系统仍为 rw，集群重新就绪后从原 Linux
检查点续跑最后两项，未重复目录变更，也没有重建业务对象。原失败、内核/
系统事件和恢复时点留证；没有重置集群、格式化磁盘或手动重启 RKE2。该业务
通过不代表硬件长期可靠性已经认证。

## 运行来源

源码 SHA256 `b9f8062ff9c67b8bc51a20dac4785c5a06ef5bb99da53ac01d1d3552c6bc6127`，归档 SHA256
`5d50993e24f3d027f9e4f782e8d9b1526babb05c7955bac0597f5d7c9607bb21`，Git 父 HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07`
不是本次运行源码提交。API/Web 同一不可变快照，374 个运行
输入一致；复用 Linux 缓存，一次构建后内网分发并在三节点校验。没有镜像/
Git 推送或模型下载。

| 组件 | 镜像 |
| --- | --- |
| API / Worker | `neon.local/control-api@sha256:8e382dab0772e0c29e1d7f4ca80c4b6c3d568fa015437b2f1838816e34ef532a` |
| Web | `neon.local/control-web@sha256:18a33f726ec895bd9690df0fa5ac8e6618572da15ceb9c63c2ca8e64a839af8e` |
| AI Gateway（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

049–052 摘要保持原值，053 冻结 SHA256 `329418d4c44ab66e13f0d527a6650d4fbb23b5b97d668d17cc03925edcf2cfb3`。
升级前 Linux 元数据备份 SHA256 `dab1cdc794d3f05ae53aaaae953c981bc9e2f1eac1d31604aecdf8e1fc03782c`。
24 对象语义核对仅允许相关镜像/Chart 标签变化，保留 9 个 PVC、
879 个既有 Secret UID 和其他六个 Helm
发布；内网直连、外网代理设置不变。

私有证据 `production/evidence/20261021-catalog-mutation-results` 的
`accepted-functional-slice.json` 汇总精确来源；另有 `ui-b\result.json`、
`final-readonly-proof-a/result.json`、`go-20261020184316/result.json`、
`quality-b/result.json`、冻结、镜像、部署、源码匹配和终态 Job 退休记录。
受保护密码输入留在 Linux，公开/本地结果不含密码/verifier。账本维持
13 组基础回执、22 组继续，继续完整业务开发。
