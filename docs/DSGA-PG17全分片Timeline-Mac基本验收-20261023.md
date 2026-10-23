# PG17全分片Timeline：Mac基本验收

2026-10-23，控制面代码`173ae909e7bd1446e3a598dfff22e0bb6312a757`；共享API依赖
`e1757bca375ffcb79b861b011e69459fa819c70f`已先推送并核验，控制面go.mod/go.sum
按远程pseudo-version锁定，无本地目录替换。当前仅PG17，原统一功能分支。

## 实现约定

正式执行器改读storage-controller的
`GET control/v1/tenant/<tenant>/timeline/<timeline>`；移除旧固定单pageserver
与SA代理候选路径。正式HTTPS/JWT客户端、平台来源与计划冻结沿前一模块。

官方TenantTimelineDescribeResponse的shards中tenant_id实际是TenantShardId。
必须整组同Tenant、无重/无缺，每片PG17、Active、原timeline和原祖先/LSN均匹配；
root的祖先字段必须明确null，字段按精确名字解析。分片拓扑可合法变化，不将
单片POST响应或shard0当整组证明。Branch实际版本继承祖先，因此写前必须读取
原祖先的整组真实版本；请求中pg_version字段本身不是祖先版本证明。

900000065在外部HTTP前持久保存原完整创建请求。锁序Operation→timeline，不
持锁跨网络；当前完整计划与lease在SQL阶段检查，外部读取后写前及首次确认前
再核验。原资源/PG/祖先/LSN/创建时间不允许修改，只有首次确认时间可从NULL
推进，已确认恢复只读不改xmin；endpoint创建不能借此新建timeline。

**503不表示不存在**：官方聚合在任一分片读取失败（含pageserver 404）时可返回
503。未确认操作只依据持久原意图及官方Bootstrap版本、Branch祖先/LSN的持久
强幂等约定，显式重放同一POST一次，不重选ID/LSN、不透明重试。429/鉴权/解析/
其他错误不触发新写。200/409之后仍须完整GET核验。确认后任何读取失败保留原
事实，拒绝自动重建可能已丢失的空timeline。

原控制JSON入口仍限制1MiB/65536值；AS新增ValidateObservationJSON只为显式
只读聚合允许8MiB/262144值，复用32层、UTF-8、重复键与尾随数据检查。只有
允许路径的timeline GET使用此入口；POST请求与其他观察的原限制不变。

记录原意图不等于远端已取消或可安全清理：旧HTTP可能迟到，lease到期不能作为
远端fence。清理/删除授权和完整最终执行链仍须另行协调；本模块不开放SQL/VM。

## 实际Mac验证

- 初轮11pass/6fail事件，0skip。共享ValidateJSON对参数有1MiB硬上限，导致
  8MiB聚合即使小响应也拒绝，已定位修复；原失败日志保留。
- AS独立只读入口新增6pass事件、0fail/skip，包vet退出0；旧写上限未扩大。
- 修后timeline新增范围17pass事件、0fail/skip；再新增迁移重放与超过1MiB
  实际TLS聚合读取1项通过，0fail/skip。两批各537源测试中一致，最终vet/diff通过。
- Neon index版本绑定检查通过：18 binaries、4 pinned PG versions、39 extension
  stages；实际PG17仍fcada44b，PG gitlink及两份PG锁未变。首次误填不支持的
  source-ref=working-tree失败，改为门禁规定的index后通过，未将前次计为通过。

真实执行准确fcada PG17.11元数据库、公开Session/CSRF接受、持久意图/确认、
TLS/EdDSA、原请求重放和lease恢复。覆盖部分未知创建后聚合仍503、冲突、已有
整组、第二片PG16拒绝、429、确认后丢失、不重写事实、lease跨HTTP到期、原
请求防改/防删、迁移重放、大响应及完整分片/祖先/状态核验。

Controller为协议夹具，不运行真实pageserver/storage-controller/K8s。各批
可能重叠，不累加成唯一覆盖或最终单次全套通过；未重复未变内核/SDK/VM套件。
机器摘要见`acceptance/dsga-pg17-timeline-aggregate-mac-20261023.json`（控制面）。

## 仍须编码与Linux交接

公开versioned worker/安全SQL路由、完整预算准入、最终quiesce/策略/Scheduler/
Controller/Runner及生命周期、分支RO/catalog等业务仍须接齐，不归为仅Linux
待验。Branch生产者尚未公开接线，Mac祖先整组校验不是分支全链认证。

真实非dev存储鉴权、分片/迁移/控制器或pageserver重启后的幂等和故障恢复，RBAC、
SA/证书供给及轮转、Service路由和集群生命周期留Linux K8s；KVM/NeonVM热插拔、
HugeTLB、物理释放和性能长稳仍须目标环境验收。Mac通过不能保证内核/集群正确。
遵守最新要求，只做新增业务必要基本验证，不扩展集群模拟；不构建镜像、不准备
或执行部署、不合自有main，Goal active、默认关闭，整体V3未完成。
