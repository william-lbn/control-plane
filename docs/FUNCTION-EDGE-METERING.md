# Functions 独立 edge 与可信用量合同

该切片实现 F06 的独立执行入口、调用观测、原实例资源观测和逐请求版本解析。代码、功能测试和集群验收分别记录；本文不声明尚未运行的测试通过。性能、容量、压力、HA/DR 和跨分区外部 epoch 栅栏继续后置。

`control-function-edge` 是独立进程和 Deployment。它只持 edge 客户端叶证书、broker CA 和公开 guest CA；不持 metadata 数据库、Kubernetes token、对象存储、SQL、manager HMAC 或 guest CA 私钥。它直接连接经控制面验证的原 guest Service 的 9090/TLS 端口并执行请求，HTTP/SSE/WS 响应字节不经过 API 进程。

控制面私有 8795 broker 使用 TLS 1.3 双向证书认证，仅接受链验证成功且 CN 为 `neon-function-edge` 的客户端。它保留现有 public policy、actor/组织/项目/分支授权、冷醒、原 VM/Service/Secret UID、完整 spec、guest CA、boot/generation 与当前部署验证。broker 不挂在 Console 或 public 路由上。

每次请求通过一个双向 NDJSON 控制连接：edge 发送有界 admission，broker 在 PostgreSQL 原子保存 request event 和 edge admission 后返回 grant。grant 只含已经签名的精确 manager method/path/body/nonce；有效窗口 30 秒且 guest 只消费 nonce 一次。edge 得不到 manager key，不能把 grant 改写为 shutdown、其他 body 或另一个管理接口。发送失败、结果丢失和长流中断均不自动重放。

broker 从 admission 到完成回执一直保留原 Function 的共享 advisory admission lock；控制连接异常时 edge 取消 guest 调用。它沿用原先的合作式控制屏障，不能被描述为网络分区下的外部执行栅栏。每次 admission 都重新读取 active deployment 和原 boot，不缓存跨部署路由；deploy/rollback 仍走现有不可变发布/退休流程，新调用在新发布后解析新部署，旧调用保留原部署归属。停止和版本替换前增加原 guest 最后一次资源观察，失败时保留缺口。

`GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/usage` 使用现有分支读取授权，返回保留期内按 console/public_http/schedule/object_storage 区分的 admission、完成、进行中、未知结果、响应字节和持续时间；独立 edge 项另列确认 dispatch attempts、未确认 dispatch、完成回执、请求 body 字节和 WS 上行帧字节。完成信息只接受原 mTLS 控制连接上的同一 event，不能从客户 HTTP header、请求 body 或匿名 report API 填入。重放完成回执不会重复累加。

HTTP/SSE response_bytes 是已写给客户端的 body 字节；WS response_bytes 是 guest→客户端的原始帧字节，上行帧字节独立计数。HTTP 协议头、TLS 密文开销和节点网络开销不属于这些字节计数。`dispatched=true` 表示发送尝试；它不伪称应用一定执行成功。已保存 admission 但丢失完成回执明确保留 unknown。

资源来自 root supervisor 读取原 Node 子 cgroup v2 的 `cpu.stat/usage_usec`、`memory.current` 和 `memory.peak`，不采用客户 `NodeObservation`。worker 每 15 秒尝试采样，edge admission/完成和正常退休前补采样；每个样本验证原 scope/boot，并存入 migration 073 的 `function_resource_samples`。同 boot 重复/逆序/CPU 回退样本不计入；CPU 是每个原 boot 的累计值，不能按并发请求重复相加。内存记录实际 current/peak，不伪造 GB-hours 或单次调用 CPU 归因。新 guest endpoint 不可用、计数文件缺失、采样失败和过时样本明确为 unavailable/stale；已观察累计值是截至最近有效样本的下界，不能伪称覆盖未观察的退休尾部或故障期间。该接口不自动扣组织余额。

Chart 开关为 `functions.edge.enabled`，默认关闭；当前固定一个 edge/API/Worker 副本。`functions.edge.existingClientSecret` 包含 `tls.crt`、`tls.key`、`broker-ca.crt`；`existingBrokerSecret` 包含 server `tls.crt`、`tls.key`、`client-ca.crt`。broker 服务证书 DNS SAN 必须匹配 `<release-name>-function-edge-broker.<namespace>.svc`（实际前缀使用 chart name helper）。客户端叶证书仅需 ClientAuth，broker 叶证书仅需 ServerAuth；CA 签发私钥留在离线运维边界。edge 从原 functions common Secret 只投影 `guest-ca.crt`，不投影 config/backing-mac。ServiceAccount 不挂 token、无 RoleBinding，容器非 root、只读根目录、去掉全部 capabilities。

启用需要升级带资源 endpoint 的 guest supervisor 镜像并配置上述原始身份；旧 SQL/分支凭据验证和已有 Function 实例不得通过伪造样本跳过。切换公共 Service 的维护操作需先停止新 admission 并处理原流；schema 073 和用量证据保持保留。部署、真实 SQL、长流、版本切换及异常回执的新增实机检查应使用原已有业务夹具，历史通过的基础 SQL 不重复创造等价夹具。

2026-10-27 主代理已完成本文新增 Go/PG、独立 binary 编译、vet 及五组 Helm 检查；具体复用与失败续验记录见[增量回执](DELIVERY-2026-10-27-FUNCTIONAL-SLICES.md)。本记录不声明尚未执行的实机接管通过。
