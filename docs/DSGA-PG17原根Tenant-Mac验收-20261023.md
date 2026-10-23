# PG17原根Tenant：Mac验收

2026-10-23，控制面代码`4c25b1fb970b0ebf230fad759d4c2399e8da4c3c`，原统一功能分支，当前仅PG17。
整体V3仍未完成，Goal active；此模块是公开worker的存储前置执行器。

按实际官方TenantCreateRequest/TenantDescribeResponse及TenantShardId格式实现，
仅原`create_project_dsga_v1`可通过当前lease/完整原计划创建根Tenant。固定HTTPS
apiserver/SA和storage-controller服务，先GET、一次POST、再GET确认；429不透明
重发，未知写入下次原意图GET恢复，409本身不是成功。1MiB/值数量/深度有界、
原始JSON拒绝重复键，身份按精确字段名读取。完整shard集合必须同tenant且无重/
无缺，兼容unsharded、正式count=1及合法分片；拓扑不冻结成单pageserver。

900000064追加独立存在事实，先于VM绑定，Operation→Tenant锁序不跨网络。确认
后同源恢复只读，不改xmin；404保留原事实并拒绝重建同ID空存储，也不武断断言
数据已丢（可能是代理/服务暂不可用）。旧步骤即使被重试改回running也不能抹掉
事实。endpoint/其他action及外Tenant不能经代码或SQL守卫冒充根项目创建。

Mac准确fcada44b PG17.11：首轮19pass、1个409子项及其父项失败；服务代理将
冲突归为不同StatusReason，修为按有类型实际409进入GET对账后，仅补跑success/
409/新增异源409，4pass；新增endpoint与SQL外租户权限1pass。后两批0fail/skip，
515源清单稳定，最终vet/diff通过。失败日志保留；分批重叠不相加冒充唯一覆盖，
不称最终源码单次全套通过。记录见机器摘要。

真实执行公开Session/CSRF接受、原随机plan/资源图、元数据库/typed TLS/事务与
原事实恢复。Kube/存储controller为协议夹具，不声称真实Neon存储端到端。覆盖
未知写入、409与异源409、429、来源漂移/重复JSON、完整分片/缺片/混合/跨租户/
别名/大小写拒绝、租约在HTTP中到期、无跨网络SQL锁、SQL失败原对象恢复、迁移
重放及不可变事实。未重复未变内核、Rust/SDK、Web/全API。

本模块不发放存储ready、预算/SQL或VM许可，不实现新action的失败清理授权。
已发送的未知外部操作仍须持久协调，lease超时不等于远端取消；后续删除/清理
必须另行对账，不能仅据存在表宣称最终远端fence已完成。

相关官方与自有维护头本回合重新fetch后已完整包含。接下来控制器全分片timeline
来源、完整公开worker/安全路由/预算准入、分支RO/catalog与最终Runner继续编码。
真实Linux/K8s/NeonVM/HugeTLB/存储/物理/性能后验。默认关闭，不构建镜像、
准备部署或合main；不能把剩余代码缺口记为仅Linux待验。
