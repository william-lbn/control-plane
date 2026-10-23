# PG17原VM内部Service：Mac验收

2026-10-23。控制面代码`139d4c011fd9a3da0206a09e8e84060d052a77f5`，每仓原功能分支，当前仅PG17。

新增实际typed生产者：重读原Operation/current lease、VM/Config UID、完整启动
模板、journal及原凭据，再创建postgres55433和management3080两个内部Service。
原Operation/incarnation/Compute注解及同VM controller UID不可省略，拒绝外部IP、
NodePort、额外端口/selector、提前发布、亲和和不支持的路由选项。API默认分配
ClusterIP/IP family，目标始终固定，不接受URL。创建不打开普通SQL准入或VM许可。

900000062仅追加Service UID绑定表/守卫。锁序Operation→原VM/Config→Service，
SQL不跨网络；首次固定后禁止UPDATE/DELETE/重绑，恢复连xmin也不变。一次POST
关闭429透明重试；未知响应下次原意图GET恢复；已固定UID丢失绝不重建，同名新
对象拒绝，不PATCH坏现场。外部Service已写但SQL绑定失败时保留原对象与原意图。

初始化消费者实际接到该生产者：只有相同持久Service UID和完整可信路由形状
才发送只读初始化GET，不再只靠同VM标签/ownerref。网络后继续检查原journal/lease。

Mac准确fcada44b PG17.11：Service新增8pass、路由逃逸11pass；受影响的实际
Service生产者→初始化消费链17pass；只补新增同名换UID子用例及共同父流程2pass。
均0fail/skip，509源清单前后一致，最终vet/diff通过。前两组范围不同；后两组与
旧初始化覆盖有重叠，不相加冒充唯一用例，不将旧首轮改记最终源码全套通过。
SQL迁移重放两次及已写Service事实/xmin保留、过期lease/no跨网络SQL锁、绑定
SQL故障回滚与同UID恢复均实际执行。Kube是TLS/typed协议夹具，不冒称目标集群。

本批再次fresh fetch相关官方与自有控制面维护头，当前全部已包含。Neon Rust/PG/
Autoscaling代码未改，不重复旧内核、SDK、Rust、Web/全API或无关用例。日志及
源码摘要见`docs/acceptance/dsga-pg17-services-mac-20261023.json`。

**完整V3仍未完成，Goal active**。公开版本化worker、初始化持久历史、完整预算
准入/安全路由、分支RO/catalog、pending清理和完整quiesce/策略/Scheduler/最终
Runner/存储链仍继续编码，不能写成只待Linux。实际K8s/RBAC/Service路由/TLS/
网络隔离，以及Linux/KVM/NeonVM/HugeTLB物理回收/迁移/故障/性能后验。默认
关闭，不构建镜像、不准备部署、不合main。
