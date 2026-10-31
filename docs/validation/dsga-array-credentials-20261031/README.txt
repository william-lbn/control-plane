数组请求绑定JWT签发窄扩（PG中立；control-plane be30阶段）

范围
沿codex/dsga-control-plane-v3-20261020，原HEAD be30ba5e8d774781ff76194128720dd6f5d6e045。
仅4代码路径：dsga_credentials.go共享私有handler、compute_gateway.go新route、
新增dsga_array_credentials.go及其精确test。不fetch/main/迁DB/换key/改go.mod/go.sum。
这不是届时最新控制面完整整合、官网全部需求、生产Kubernetes/RBAC/E2E或PG内存降限认证。

固定版本化wire
POST /computes/{原proof.VMName}/dsga-array-credentials
仍使用原projected Agent Pod SA Bearer（audience neon-dsga-credential-broker）及专用管理TLS/SNI。
JSON envelope为protocol_version:1、owner完整原owner.Ownership、request：
{protocol_version:1,method:"POST",path:"/dsg/v1/resource/array-authorize"或
"/dsg/v1/resource/array-observe"或"/dsg/v1/resource/array-replay-original",body_sha256:原正文小写hex64}。
空/全0/非canonical SHA、其它method、大小写/escape/query/尾斜杠path、未知/重复字段全部拒绝。
新route自身也拒URL query/escape/fragment，不接受请求scope选择签发权限。

签名和身份
新JWT aud=neon-dsga，scope=compute_ctl:dsga:array-authority，完整request对象在签名覆盖内。
原compute_id/vm_uid/compute_instance_id/issuer_epoch字符串、nbf=-2s/exp=+60s规则不变。
回复沿旧7字段，仅新route加scope及完整request回显；客户端必须严格核全部原绑定及新回显。
通用token不含request/array scope；新scope只能针对3个array POST，不能普通apply。
实际消费端隔离/原始body重算属于P1 AS和Root Neon Rust后继，不能拿本issuer测试替代。

授权没有新增宽入口：原PodVerifier独立TokenReview+真实Pod/SA UID、holder Pod UID、VM Node、
新鲜Lease与完整journal owner；VerifyCurrent为Lease→完整Store→Lease，固定VMUID/incarnation/epoch。
读原owned config/JWK及原<workload>-credentials controlSigningSeed后，再复核同owner及Bootstrap。
签名私钥只留原Secret，未调用创建/轮转computeControlKey兜底，未复制到Agent/Guest。
一般tenant/未绑定Pod的泛SA token不是此权限；当前授权coordinator才可请求。
最后核验后仍可能换主，短JWT不能代替PG/Root单调epoch floor、原意图和资源fence。

正文来源职责
CP不解析数组正文、不构造成功预算或normal许可；SHA不是Guest代理提供的授权依据。
原pgexec必须在Start CAS后从其原持久Session/意图canonical编码准确正文，再向原可信issuer请求绑定。
P1负责AS凭据client/http/pgexec，Root负责Rust验证及原始raw bytes转发；双方必须保持exact SHA。
没有新的通用token缓存：新请求绑定不能被旧Token缓存或仅按epoch复用。

实际验证
实际Go1.27.1 linux/amd64，原api/go.mod go1.26.0/toolchain go1.27.1保持；-mod=readonly。
不可变be30+仅4文件overlay，532项Go/mod/sum实际COPY逐项吻合。
10个精确Go race根、62子例零失败零skip，首次冷编译总119.870s；无DB依赖或迁移。
新5根：3path精确scope/request签名与旧shape兼容、协议/路径/SHA/重复未知字段拒绝、
VM/Lease/owner/epoch/过期/held/读key后换主拒绝、签名字段篡改、defaultclosed/泛token拒绝。
旧5根：默认/partial配置、原短token、原Bootstrap/config UID、key/owner变更、malformed/TLS-SNI直接回归。
测试使用真实loopback TLS/Ed25519与生产typed客户端/journal.Store，Kubernetes对端为模型。
不是实际apiserver/TokenReview/RBAC、真实Agent/Compute/Guest或K8s端到端认证。

实际依赖锁证明
AS声明模块仍3eeafd，replace保持自有565410ad3787；执行器实际模块/PodVerifier/VerifyCurrent源码和hash已导出。
没有拿并行AS工作区代替此编译依赖。实际old Pod/Lease校验链已核，原go.mod/sum字节未改变。
所有日志不打印token/seed/正文/env；测试私钥为临时生成，不涉及集群实际凭据。
BuildKit/Cargo/Go缓存保留，显式代理从原env载入但值不写日志。
客户端cgroup2GiB/CPU2、执行器GOMAXPROCS2，未把客户端限制冒称BuildKit executor硬内存限制。

后继
Root独立审阅后统一中文阶段提交；P1/Root同wire消费者完成后再做跨仓准确版本锁/真实单机回归。
本窄扩不改变既有上层状态机，不提前宣称PG17或PG18完整控制面目标完成。
