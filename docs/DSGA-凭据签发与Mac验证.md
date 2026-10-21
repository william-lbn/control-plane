# DSGA 专用凭据签发与Mac验证

本页按模块记录实际证据。当前中心凭据/六路由代理已接线，启动身份消费者已
固定自有Autoscaling8e1e0ea；**整体弹性功能仍未完成、默认关闭**。Driver完整
创建接线、租户/管理角色隔离、预算/quiesce、Monitor/最终Runner、多PG版本及完整
存储E2E继续实施。没有构建镜像、准备部署、连接实际K8s或合并主分支。

## 2026-10-21：中心消费持久启动身份

本批固定依赖自有`william-lbn/autoscaling`提交
`8e1e0ea93d5f2775376622a38ed10f39d9d1fc8c`，Go伪版本
`v0.0.0-20261021061647-8e1e0ea93d5f`及go.sum同时锁定；保留官方模块名。
签发与代理复用生产`bootstrap.CheckReady`和唯一完整journal解码器，必须核对
ready记录中的真实VM UID、固定Compute ID、配置Secret UID、唯一controller
ownerref及业务归属。删除后同名重建Secret或外来owner均拒绝，不能只凭labels
和config.json自报身份签发/派发。读取私钥后第二次主体核验还要求完整bootstrap
记录与先前相同，阻止检查过程中改变Config UID等身份事实。

新增7类签发及2类代理拒绝场景，最终DSGA race共78个pass事件。受影响全API
Mac race **966个pass事件（256顶层、710子事件），9测试包通过、10无测试文件
包单列，0用例fail/skip**。与CI同版官方Darwin PostgREST16.4实际RLS、一次性
SCRAM PG0806及隔离schema均运行，结束关闭自有临时PG。不是Kubernetes实体认证。
日志：根工作区`build/control-plane-bootstrap-mac-final/`，含最终源码SHA256清单；
`build/control-bootstrap-bound-final-focused.jsonl`为独立DSGA测试。
三项实际生产反向分别移除原Config UID/ownerref校验、读key后完整bootstrap相等
检查、代理Config owner检查，均触发对应业务断言；首次注入因unused import导致
编译错误，不计成功，修正为保留类型引用后重新取得3项真实断言失败。源码finally
逐字节恢复，与上述全量执行源码SHA256一致，再跑全部78个DSGA race事件及全仓
vet通过。Linux/amd64 CGO0全部cmd交叉编译通过，非Linux运行认证。
日志：`build/control-bootstrap-negative/`、`control-bootstrap-restored.jsonl`、
`control-bootstrap-vet.log`及`control-bootstrap-linux-cli.log`。

本批只加强已经接线入口的启动身份约束。它没有把身份ready变成预算或VM许可，
也未开启对外DSGA创建。实际Driver仍使用旧PG16创建顺序，必须改为持久意图→
CREATE held→实际VM UID/安全原生spec/owned Secret/完整日志→UID/RV一次释放。
复核还确认旧Driver把租户密码赋给cloud_admin，而Compute以SUPERUSER创建该
内部管理角色；PG启动准入门禁为管理超级用户保留通道。因此在启用DSGA新建前
必须先隔离租户角色与管理角色、覆盖已有连接的新SQL/后台工作及恢复后准入。
这是代码缺口，不能写成只待Linux验收，亦不能靠单独改PG_VERSION为17完成。

## 依赖与实际入口

复用自有Autoscaling提交3eeafd997c8cc9079fcf90ffd0df27aa32c13ce5：
主模块名仍为github.com/neondatabase/autoscaling，但go.mod显式replace到
william-lbn/autoscaling v0.0.0-20261020225115-3eeafd997c8c，且go.sum锁定内容。
不是另写日志解码器，也不是只复制一个未接线的独立PG项目。新增typed K8s依赖
与既有库共同由Go MVS选择；原go1.26.0/toolchain1.27.1及主要既有依赖未降级。

真实RunComputeGateway调用同一个routes构造函数，缺省只提供原configure。
仅显式给出NEON_DSGA_AGENT_NAMESPACE、NEON_DSGA_AGENT_SERVICE_ACCOUNT及独立
NEON_DSGA_GATEWAY_CERT_FILE/KEY_FILE才创建独立TLS监听（默认:8444，绑定可配置），
并注册POST /computes/{compute}/dsga-credentials。原:8443只保留原configure身份。
配置不完整构造失败，打开任何监听前拒绝启动，
不静默转general/admin认证。TLS业务测试直接使用这个生产routes，未另造mux
冒充真实接线；另外核验缺省404、configure保留及两种不完整主体配置均拒绝。

旧每Compute证书私钥也挂载给Guest，不能作为Agent向签发器发送SA令牌的TLS
身份。dsga_listener.go只从控制面独占文件读取新的管理域证书，强制当前有效、
非CA、ServerAuth及*.namespace.svc wildcard，明确拒绝原Guest单Compute证书。
证书回调不读取Guest配置Secret；每次握手重读投影文件并验证准确Compute SNI，
不回退Guest密钥或旧身份。Agent只应取得可信平台CA，独立私钥不得挂入Guest。
这项Secret/RBAC隔离与证书轮转仍须目标环境验收，代码不代替部署权限证明。

这是私有管理协议，不向普通Console用户分发SA令牌，也不在用户业务流中暴露
实现细节。HTTP body为Autoscaling owner.Ownership：namespace、vm_name、vm_uid、
incarnation、lease_uid、holder_id、issuer_epoch；epoch必须规范uint64十进制字符串。
这些值只是待核对声明，不是权限。路径Compute固定为自有Driver的cp-16位hex名字，
namespace与运行配置相同；不能提供任意目标URL/签名key/其他VM名称。

## 签发顺序与不变量

入口要求TLS与该Compute的准确SNI、单一Bearer/Content-Type、JSON且无Content-Encoding，
8KiB body、未知字段/尾随第二文档拒绝。整个授权/读取链10s，typed Kube每请求5s；
真实HTTP还设置读截止时间，避免context超时无法打断恶意慢body。不支持截止时间的
ResponseWriter失败关闭。成功/失败均no-store，错误不回显原Kube body、Bearer或key。

中心使用自己的已验证TLS/RBAC Kubernetes身份。Agent Bearer只进入TokenReview
body，audience为neon-dsga-credential-broker；不作为中心访问Kube API的Authorization。
复用生产PodVerifier：专用audience、允许SA/真实UID、投影Pod UID、随机进程holder、
真实Running Pod/Node、同Node可派发VM、Lease→完整journal→Lease、保守续约新鲜度
及固定VM UID/incarnation/Lease UID/当前owner。没有正向权限缓存或空journal兜底。

确认主体后，从同VM业务labels查找本namespace的owned -config/-credentials Secret，
同时验证对象名/UID/RV/未删除及project/endpoint归属。只读原签名seed；不调用会
创建/轮转密钥的computeControlKey兜底，不修改任何Secret、Lease、journal或预算。
必须有protocol_version=2的dynamic_shared_buffers，VMUID准确、ComputeInstanceID有效。
完整启动预算仍由同一生产Rust/PG契约核验，签发器不另造预算成功回执。

私钥与实际配置JWK逐字节公钥相符，kid/kty/crv/alg、sig用途及单一verify操作一致；
匹配kid恰好一个。缺失/坏seed、其他VM、缺失实例、旧协议或JWK不匹配一律拒绝。
读取配置/私钥后再次核验相同主体与当前完整owner，检查业务归属仍相同后才签名。
测试在私钥读取中通过真实Store接管journal，确认第二次检查阻止已过期owner签发。

JWT独立使用aud=neon-dsga、scope=compute_ctl:dsga，绑定compute_id、vm_uid、
compute_instance_id与字符串issuer_epoch；nbf为当前前2秒，exp为当前后60秒，
在正式Compute的最大300秒窗口内。响应只有短Token、到期和固定身份，不含seed。
不复用general computeJWT/admin/Hadron，不签PG boot替代完整请求的boot核验。

Token不是VM许可、物理回收或预算回执，不推进PG issuer floor。Lease/多对象读取
不能构成最终原子执行权，短期Token可能与换主重叠；原生PG/Runner仍要独立fence，
未知原意图和仍可能被消费的预算不能因凭据过期被清除。中心typed API共享有限
50QPS/100突发限流器，不据本机测试宣称目标规模或时钟/吞吐SLO。

## Mac平台修复与真实验证

原通用bootstrap引用的GuestBoundary仅定义在_linux.go，首先真实复现Mac构建失败。
独立提交ef446e2把原字段/Validate原样移到通用文件并逐字节比对；Linux的Prepare、
防火墙/cgroup/设备操作仍留在_linux.go，未提供Mac伪隔离。既有原注释保留并补中文。
Linux/amd64 CGO0的Functions测试程序交叉编译通过，不能当作Linux运行验收。

本地使用缓存Go1.26.5满足项目最低1.26.0；原toolchain1.27.1没有改动，CI仍按其
既有版本执行。Xcode14.4 SDK/clang仅通过进程环境指定，没有改全局工具链配置。
所有数据库测试使用新建一次性ci/control_ci、只绑定loopback的随机端口、实际
SCRAM host认证及CI同样的隔离migration/auth/idle schema；结束后已关闭，证据保留。

真实PostgREST采用同CI版本16.4的官方Darwin arm64资产，不使用Linux程序或镜像。
[官方版本](https://github.com/PostgREST/postgrest/releases/tag/v16.4)发布元数据的
Darwin归档SHA256为5720fbde4a19ade9fb189791615c84beb9f0e58201b1b6d17fe3b698faf60776，
下载后核对一致且实际执行PostgREST16.4；生产Linux的原固定资产/摘要不变。
RLS、TLS、真实SQL/元数据及全部可执行Go测试实际执行，不把缺PostgREST称作Mac限制。

中间正确环境全仓904事件通过，增加独立JWK公钥/用途边界后907事件通过；最终
生产路由/独立TLS版本全仓908个case pass事件：248顶层、660子事件，9有测试包、
10无测试包，零case fail/skip；其中DSGA4顶层/16子事件。父/子不冒充独立顶层。
Linux/amd64 CGO0全部cmd交叉编译通过，非Linux运行验收。
四个真实生产反向分别去掉签名前第二次owner核验、JWK公钥核验、准确SNI绑定、
独立管理域证书约束，
均被对应业务断言拒绝，不接受编译错误充数；finally逐字节恢复，并核对完整运行
source-sha256清单后重跑全部凭据race与全仓vet。最终源码未混入反向注入。

最初全仓尝试851pass/10fail：缺CI auth/idle变量、缺固定PostgREST及使用trust认证
不满足强制SCRAM用例，不能算通过。补齐准确环境后完整904/907通过，之前的
Functions SQL取消子例也通过，未据错误环境修改其生产SQL认证/取消代码。

日志为根工作区build/control-plane-broker-mac-{full,full2,final,final-router,dedicated-final}，以及
control-plane-broker-{dedicated-negative.log,dedicated-restored.jsonl,dedicated-vet.log}和
control-plane-guest-boundary-linux-compile.log。最早网络直连下载失败后按依赖
分流使用原Go代理和own-fork固定SHA，不降级依赖；源码与sum以实际Git为准。

## 2026-10-21：实际Agent消费者与六路由代理已接线

Autoscaling自有功能分支`f60ff588b4dba403885dcd371caa412079ae87a5`已推送并
核对远程；本仓固定使用该精确模块，不从官方同名模块意外取得另一份实现。
`credential.Client`使用可信CA/固定目标/SNI、投影SA文件、有界并发刷新，
标准TLS验证后仍拒绝Guest旧叶身份。真实Agent入口先核对固定VM UID及受管标记，
再使用真实Lease owner Session和长期PGWorker；受控首次boot只适用于完整空日志。

独立:8444的实际生产mux增加`GET /dsg/v1/{capabilities,memory}`及
`POST /dsg/v1/{resize,resize/status,resize/cancel,issuer/fence}`。仅允许这些
方法/准确路径，无query/编码别名/GET body。POST body最多16KiB，10秒读期限；
响应最多1MiB，JSON有效才透传。不转发调用方的重试/连接/目标头或上游cookie/
Location；不跟随redirect，也不透明重试POST，失败保留未知原意图。

入口从TLS SNI确定Compute，核对同一owned配置的专用Ed25519 JWT及实际VM UID/
CIID。严格匹配原生Rust的aud/scope、规范字符串epoch和整数秒nbf/exp；拒绝
重复/未知成员、重复kid及过长有效期。每次派发前重读完整journal、Lease→journal
→Lease核验当前epoch/holder、保守续约新鲜度和可派发生命周期，不能只看缓存JWT。
最后一次读取仍不是跨对象原子票据，PG issuer floor及最终Runner fence继续必需。

内部目标只取同project/endpoint的owned ClusterIP Service，固定3080，严格
selector/端口/协议/UID/RV及未删除；原生Compute仍独立验签。执行70秒、观察/
查询/取消15秒分别使用有限连接池；总期限90秒、监听器写期限100秒，凭据路由
自己的10秒期限不变。Agent将凭据1..30秒与Compute90..120秒配置分开，兼容
原生PG最多60秒等待；超时不是取消已发送SQL，更不是成功/VM许可。

最终Mac全API race：**956个pass事件（255顶层/701子）、9个测试包通过，
10个无测试文件包单列，0用例fail/skip**。官方Darwin PostgREST16.4的实际RLS、
一次性SCRAM PG均执行，结束后自有临时PG已关闭。68个DSGA事件覆盖真实TLS
签发/六个代理路由、原样字节/最大uint64、旧owner/Lease/VM重建、错误Service、
双池长执行期间查询/取消、未知响应/redirect/限body及请求边界。

新增实际连接测试使用生产owner Elector/Session→credential.Client→中心TokenReview
及完整journal→Compute SDK→同一个生产代理mux→本机原生端协议夹具；固定PG
boot在返回SDK前绑定。这里最后的native响应仍是协议夹具，不能说已经覆盖真实
Rust/PG全链。独立真实Go→Rust Compute→SQL→PG17门禁9.24秒通过，但不能将
两条分别通过的链拼称中心网关到真实PG的端到端已全部通过。

连接测试最初10秒期限小于夹具旧Lease30秒观察期而超时；修正为等待真实短旧
Lease后UID/RV接管，生产选主规则没有放宽。三项实际生产故障注入分别去掉最后
owner核验、签名核验、重复claims拒绝，均被对应业务断言捕获；首次签名变异
因变量未使用编译失败，不计反向成功，修正后取得真实断言失败。finally逐字节
恢复，核对source-sha256清单后重新执行上述完整956项事件的最终回归。

日志：工作区`build/control-plane-proxy-mac-final2/`、
`control-proxy-chain-{focused,final-focused}.jsonl`、`control-proxy-negative/`及
`control-proxy-vet-final.log`（全仓vet通过）。新增源码的Linux/amd64 CGO0全部
cmd交叉编译通过，Autoscaling实际Agent Linux/amd64也编译通过，日志分别为
`control-proxy-linux-cli.log`、`autoscaling-agent-linux-compile.log`；沙箱只读
模块缓存出现一次非致命stat-cache写入警告，命令退出0。没有Linux运行验收。

## 后续完整业务与目标验收

现有原生HTTP仍监听内部普通3080；原:8443 configure、独立:8444的凭据及DSGA
操作代理已分别接线。管理域私钥只挂中心、投影令牌/可信CA接Agent、内部Service
网络隔离仍须在真实平台验收；不能因为Mac协议夹具通过就宣称RBAC/网络部署正确。
跨owner/boot的完整受控关闭、中心网关到真实Rust/PG整链仍须继续完成。

Driver必须在Runner启动前完成真实VM UID、安全Compute spec与完整journal，
持久屏障解除采用UID/RV；当前缺口仍是代码工作，不能归为Linux待测。预算/quiesce/
Monitor/Scheduler/Controller/最终Runner、多PG版本及完整Neon存储E2E继续按V3完成。
真实TokenReview/RBAC/etcd、Linux/K8s/KVM/HugeTLB、Host/Guest/VM物理回收、迁移与
性能长稳仍由后续目标环境验收。此处Kube是TLS/UID/RV/RFC6902协议夹具，非目标认证。
