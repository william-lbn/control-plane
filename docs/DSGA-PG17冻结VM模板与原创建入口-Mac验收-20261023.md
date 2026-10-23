# PG17 冻结VM模板与原创建入口：Mac验收

日期：2026-10-23。**Goal active；完整V3仍未完成。** 所有者已澄清：要求PG17
全部功能代码完成，Mac必要基本验证后交Linux，不能把节约重复测试理解成暂停
业务开发。此前“基本验证后停止开发”的结论已撤回；其他PG仍暂缓，每仓同一
既有汇总分支。默认关闭，不构建镜像、准备/执行部署、合main或发布。

## 真实代码与边界

控制面代码`3948b2be3e8c70db31bd08b3e7f26ee98056b81b`。先独立合入新main f582（维护`3aabf76`），37个上游文件
逐字节一致、无冲突且DSGA未改，再实施本模块。Neon/Autoscaling/Neon PG17/
社区PG17 fresh fetch头未变且均已为原功能HEAD祖先；具体SHA沿用本日维护清单。
PG仍fcada，AS仍8e1e，Neon生产Rust未改；没有重编或改标旧内核证据。

新增`dsgaVMProfile`原计划值字段：protocol、架构、Runner/Kernel digest、根磁盘
bytes、scheduler、SA、TLS ClusterIssuer。接受时随完整原计划持久化，恢复不读
当前env；历史无profile计划可读，生产构造拒绝启动。CPU保持整数milliCPU，
内存不截断，修正slot上界为官方CRD实际512，非仅int32最大值。

`renderDSGAVM`生成官方typed模型，从CREATE即held；实际VM UID与Config UID
仍须原Operation持久绑定。明确固定官方端口、生产CLI、PG17环境、最小启动Use、
只读config挂载和无SSH，不传--dev；Linux Monitor连接私有peer socket，避免默认
TCP5432和网络SU路径。镜像fixture只有显式测试digest，不表示已构建或可运行。

官方`Spec.TLS`/ClusterIssuer负责证书供给与轮转，挂载`/run/neon/management`；
Compute使用真实`tls.key/tls.crt`名称，修正此前control.key/control.crt不匹配。
Guest叶子TLS私钥由官方证书路径供给；中心签名seed/CA私钥不挂载到Guest。
它是Guest PostgreSQL TLS配置，**不是中心Agent令牌签发监听器的独占TLS身份**；
没有新增自签CA或改写原中心wildcard管理身份。官方证书watch已存在，本模块没有
在Mac冒称验证实际签发/轮转。

`binding.prepareVM`作为原版本化Driver的生产接线点，重载当前lease和持久UID，
不接受任意VM/Render回调；完整规格核对后执行typed创建→原VM绑定→journal→
安全Renderer→原Config绑定→UID/RV释放，返回时再次检查最终VM规格。只放行
ready后范围内Use与TargetRevision；启动镜像/参数/挂载/SA/TLS等漂移拒绝，不
PATCH修补。旧binding空UID缓存也不能触发已绑定资源重建。VM/Config POST明确
关闭429透明重试，一次写入未知结果由原意图下一次GET对账。

**这个生产接线点还未接公开handler/worker。** 不发布普通SQL路由，不把ready
屏障当初始化回执/预算/物理许可；公开创建、catalog/初始化、完整预算/quiesce/
最终Runner和完整Neon存储E2E继续实施，不能据本模块通过申报完整功能完成。

## 实际Mac结果与去重

准确二进制为PG17.11 `fcada44b67d239aea8e699a721b99c470fc24d65`，一次性独占
PG运行元数据/真实SQL锁与迁移（DSGA关闭/local heap），结束已正常停止。Kube
为真实TLS与typed API协议夹具，不是实际apiserver、证书服务或Guest运行。

| 记录 | 实际结果 |
|---|---|
| 新main受影响控制面输入文件/Native/组织权限 | 17顶层/17pass事件，0fail/skip |
| 新main受影响Gateway媒体/Native | 8顶层/28pass事件，0fail/skip；control/gateway vet通过 |
| 新main Web | 新输入文件3项通过、类型检查/Vite构建通过；未重跑未变47项 |
| VM模板与受影响创建链首轮 | 10顶层/45pass事件，0fail/skip，8.810秒 |
| 增加返回终检后定向补验 | 仅受影响生产入口1项通过，0fail/skip，3.597秒 |
| 最终control vet / diff检查 | 通过；最终498源文件清单与补验一致 |

两轮之间只改`dsga_vm.go`返回时检查：重构移除该hunk后的前版本并精确匹配首轮
SHA256，证实其他函数字节相同。只重跑受影响入口，不重跑模板纯函数、旧内核/
SDK/Agent、凭据四类SCRAM等未变功能；不声称最终源码单次完整API或45项全套
重新运行。源码/日志SHA256和原始运行摘要见
`docs/acceptance/dsga-pg17-vm-template-mac-20261023.json`。

新增覆盖：生产模板held状态、冻结镜像/环境变化不影响恢复、分数CPU/整数bytes/
512槽边界、无profile/凭据/Issuer与mutable镜像拒绝、真实TLS文件名联动、完整
规格漂移拒绝与合法运行Use、真实原Operation来源到VM/Config的绑定和未知结果
恢复、同UID坏CLI不释放held、旧binding不能重建原UID，以及VM/Config429各一次。

## 继续实施和Linux后验

当前公开入口/worker、可信catalog、初始化回执/准入、完整预算及Monitor/策略/
Scheduler/最终Runner的领域接线仍须开发，之后补实际整条原生链Mac可执行E2E。
真正Linux/K8s/NeonVM/HugeTLB分配回收、ClusterIssuer签发/轮转、Guest启动、并发
故障与迁移/性能长稳必须在目标环境验收；Mac结果不免除内核Linux验证。
