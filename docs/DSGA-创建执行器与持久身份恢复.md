# DSGA 创建执行器与持久身份恢复

2026-10-21，V3增量。当前模块实现可信Driver的typed API创建执行器与恢复协议，
真实数据库UID绑定模块已补充，详见
[创建身份元数据绑定](DSGA-创建身份元数据绑定与Mac验证.md)；
冻结计划、版本化Operation数据库生产者和PG17存储入口已补充，详见
[冻结启动计划与存储版本贯通](DSGA-冻结启动计划与存储版本贯通.md)；
**公开创建入口、安全配置Renderer及实际worker调度尚待接线**。
公开创建仍为原PG16流程且拒绝收养DSGA；本模块没有启用该公开入口，不代表
完整Driver、R02预算或最终VM执行链已经完成。不构建镜像、不部署、不合主分支。

## 实际代码职责

`api/internal/control/dsga_creation.go`的`prepareDSGACreation`使用实际生成的
Kubernetes/NeonVM typed客户端及自有Autoscaling8e的唯一完整journal Store。
`dsga_kube.go`为它与现有凭据签发器共用同一可信API客户端构造，保留中心TLS/SA
身份、投影token轮转、5秒请求期限、组内共享有限速率器和拒绝redirect。
Agent入站Bearer不能替代中心访问Kubernetes的身份。

执行顺序：

1. 检查原始创建模板必须是同namespace/project/endpoint、无UID/RV/执行历史、
   无任一DSGA日志标记且held的版本1记录；随机incarnation/Compute ID已固定。
2. 直接GET原VM；已持久UID后404或同名新UID拒绝，不再CREATE。首次尚无UID时，
   孤儿配置存在也拒绝；仅VM/配置均不存在才CREATE held。
3. 交给`Bind`将真实VM UID按原Operation/worker lease持久CAS；失败停在held，
   不写journal、不读生产配置依赖、不解除屏障。该回调必须校验当前操作范围，
   不能是无条件覆盖。恢复传入原UID，而非重新按同名发现替换。
4. 没有任何日志标记且从CREATE即held、无执行历史时，显式Initialize。已有任一
   标记则只Load，坏日志/缺半边不能当空状态重建；held必须仍是初始pristine模型。
5. `Render`按原计划、已绑定VM UID和固定Compute ID生成确定性完整配置。
   执行器检查原生v2身份、总大小上限，以immutable Secret和唯一同VM控制者
   ownerref创建；CREATE成功也必须按同UID读回，配置内容逐字节一致。
6. `BindConfig`把实际Secret UID按原Operation/VM UID持久CAS，失败保留原held/
   journal/Secret，不解除。恢复时Secret丢失、同名重建、owner变化、可变配置或
   内容不同均拒绝，绝不PATCH配置“修复”现场。
7. 原完整快照的UID/RV/held记录做一次ReleaseBootstrap，同时固定VM UID、
   Config Secret UID及ready；超时/冲突不透明重试。返回前再直接读原VM完整
   journal和原Secret UID/内容，复用CheckReady。返回的是观察事实，不是资源许可。

所有网络/回调共享30秒总期限，不持本地锁跨网络。普通finalizer更新可以导致RV
冲突；下一次原Operation重读处理，不去掉RV强行完成。initial owner/journal不因
恢复而重造；ready后的读回不要求仍为pristine，以允许真实Agent已开始接管对账。

## 崩溃与未知写入

VM CREATE、journal初始化、Secret CREATE和ready释放四阶段的丢回复均返回错误，
没有可执行成功对象。上层恢复同一个原Operation后，只GET原身份事实。确认已有
配置/ready不会重复POST/PATCH或清空日志；原UID发生变化一律拒绝。

Secret UID相同并不等于内容相同，因此immutable和逐字节检查同时保留。初始化
配置中的安全字段在VM生命周期固定；catalog更新须走独立的受限投影。Renderer
不能在重试时改用可变env、重新生成随机身份/operation UUID/时间戳或新启动预算。
证书/密钥轮转与受控重建的边界也必须由上层计划明确，不能改immutable配置兜底。

## Mac验证及边界

真实TLS/RFC6902协议夹具调用生产typed客户端与完整Store，不另造journal编解码。
覆盖四阶段丢回复、原UID/配置UID/owner/内容变化、丢日志/坏日志、删除/重建、
Render期间替换VM、两项持久绑定失败、14类非法初始意图、held执行历史及错误/
超大配置；ready/Running恢复确认没有写入或重置历史。Fixture的Render含原生
身份/预算字段，但不是可启动的完整Neon spec，不能据此声称PG/预算E2E通过。

最终DSGA race122个pass事件、0用例fail/skip。四项实际生产变异分别忽略VM UID
绑定失败、忽略Config UID绑定失败、跳过原配置内容比较、删除最终原VM重读，
均触发对应业务断言，finally逐字节恢复。恢复后完整Mac API回归：**1027个pass事件（265顶层/762子）、9测试包通过、
10无测试包单列、0用例fail/skip**。一次性SCRAM PG、官方Darwin PostgREST16.4及
隔离schema实际运行并在结束关闭；源码hash与全量执行完全相同。全仓vet及全部
Linux/amd64 CGO0 cmd交叉编译通过，非Linux运行认证。

日志入口：根工作区`build/control-creation-focused-final.jsonl`、
`control-creation-negative/`、`control-plane-creation-mac-final/`、
`control-creation-{vet,linux-cli}.log`。后者的source-sha256清单绑定执行源码。

## 后续必须接线

实际生产者要持久化可信模式/PG版本/不可变镜像/显式预算/随机身份/确定性配置
元数据，使用旧worker不会按PG16执行的版本化action；新增migration保留历史文件。
真实Bind/BindConfig现由`dsga_creation_binding.go`在原Operation行锁/有效worker
lease下核对作用域，原值存在时比较后只读确认，不覆盖其他UID；追加058迁移
固定原意图与UID、保留新主线失败清理/TTL约束。实际生产者/worker必须把这两个
回调和已持久原UID接入本执行器，不把数据库成功当跨对象原子执行权。

原Driver用户密码就是cloud_admin verifier，而Compute会以SUPERUSER创建该角色。
新的DSGA Renderer必须隔离内部管理/普通租户密码与路由，核对实际角色权限及
SET ROLE/既有会话/RO/恢复后准入。不能只给spec追加NOSUPERUSER就认为已执行：
官方apply_spec对既有角色只有密码变化才ALTER，必须通过真实PG角色事实核验。
项目/Operation/Timeline/Guest大版本一致；原生v2安全预算及最低未来Guest都要
核验，不能只改变PG_VERSION环境变量。完整预算/quiesce、Monitor/Scheduler/
最终Runner、受控恢复、多PG及完整存储E2E继续编码，不归为只待Linux测试。
真实CRD/RBAC/TokenReview/etcd及Linux/K8s/KVM/HugeTLB/物理/性能后验。
