# DSGA 冻结启动计划与存储版本贯通

2026-10-21，基于已验证的 `d2afdbc`。本批继续实现生产创建链：可信资源/身份计划、
版本化 Operation 数据库生产者、原计划恢复，以及 PG17 存储 Timeline 创建/读回。
**公开 handler 选择、完整安全 Renderer、租户权限隔离及 worker 调度仍未接齐，
不因此启用 DSGA 创建或普通 SQL 准入。** 不构建镜像、不部署、不合 main。

## 接受时冻结，恢复时只读

`dsga_creation_plan.go` 将平台策略快照为原计划：协议 1/PG17、namespace、
独立随机 incarnation/Compute ID/UUID、规范 UTC 时间、不可变镜像摘要、内存槽
大小及显式 bootstrap/M/最低未来 Guest/non-buffer reserve。资源上下界保留在同一
不可变原 payload 中。镜像只接受 sha256 摘要引用；不把 mutable tag 当发布身份。

策略输入只供可信生产者接受前使用，公开请求不得自报。恢复通过真实数据库读取
完整原 payload、核对原 binding 的 SHA256，再严格解码原计划；未知字段/版本、
部分身份 view、错误 scope 或坏预算均拒绝。不重读 env、不重分配身份，不生成
新的 UUID/时间；JSON 不经 float64，不丢失 uint64。

单位均为整数 bytes：buffer 必须 8192 对齐、至少 16 buffer，最大不超过 PG 的
`i32::MAX/2` buffer；bootstrap 不超过 M。reserve 必须显式大于零且小于最低 Guest，
共享预算先减后向下取整为 KB，必须落在 PG int GUC 范围且可容纳 bootstrap。
与 Rust 原生 `DynamicSharedBuffersSpec` 对齐，但真实 PG 仍在所有 preload
登记后检查主共享段布局；这不是完整私有内存/后台工作/R02 的硬保证。

NeonVM 资源采用 8MiB virtio-mem 对齐及 int32 slot 数量边界；拒绝 CPU/内存/槽数
溢出、上下界颠倒或截断。最低未来 Guest 必须是完整槽且不高于初始 MinMem，
不会自动取某次较大 VM 规格替代安全最低值。降低 bounds 仍须由后续执行链
检查原计划下限；本模块不声称该执行约束已经全部实现。

`addDSGACreateOperation` 是真实数据库入口：父项目先取锁核对原 PG17/tenant，
随后与原资源/idempotency/步骤同事务插入 `create_*_dsga_v1`。旧 worker 不认识
新 action，不能按旧 PG16 创建执行。旧生产者与新生产者共享字节写入 helper，
旧 payload 编码和动作保持兼容。已有 Operation 的绑定仍是 Operation→绑定行
锁序，创建入口不跨网络持 SQL 锁，也不锁另一条已有 Operation。

## 存储与原生启动身份

`nativeBootstrap` 只在取得实际 VM UID 后生成协议 2 原生契约，保留原 Compute ID
和显式整数预算。它不是完整 Compute config Renderer；角色、认证、catalog、
LFC/后台预算及平台私有配置仍须后续模块安全接入，不能拼接旧实验配置就启用。

`createDSGATimeline` 不修改原 PG16 路径。先 GET 原目标恢复未知写入；首次创建前
GET 祖先核对实际 tenant/timeline/PG17，再向 storage-controller 提交原版本与
原 ancestor/LSN。成功或 409 均 GET 原目标，核对真实版本、作用域、祖先和数值 LSN。
目标缺版本、字符串/小数版本、PG16、外来 tenant/timeline/祖先都拒绝，不能以
POST 成功或冲突代表版本一致。保留点可用性仍由原 pin_restore_point 流程检查。

复用生产 typed Kube 客户端的中心 TLS/轮转 SA、有限速率与 5 秒请求期限，
总期限 30 秒；固定服务代理目标、响应 8MiB 有界、UseNumber 后严格整数解码。
禁止 redirect 和 client-go 透明重试，包括 Retry-After 429。POST 丢回复只返回
未知，由同一原 Operation 下次 GET 确认，绝不自动重试后改称成功。
当前按既有 Driver 的 unsharded tenant 路径核验；多 shard 真实存储链仍需独立
接线与验收，本机服务协议夹具不证明实际 storage-controller/pageserver E2E。

## Mac 验证与证据

原计划/生产者测试使用真实一次性 SCRAM PG，验证新 action、步骤/idempotency
原子写入、原计划恢复、可变 env 无影响、部分 view/坏摘要拒绝；纯边界测试覆盖
镜像、身份、CPU/内存/槽、原生预算和祖先。Timeline TLS 夹具验证 root/branch、
已存在/冲突/丢回复恢复，以及版本/身份/祖先/429/redirect 拒绝。专项 200 个 pass
事件、0 用例 fail/skip。随后补强 CPU 上界的独立步长覆盖与 6 项 KB/uint64 边界，
最终完整 API Mac race **1335 个 pass 事件（359 顶层/976 子），11 测试包通过、
9 无测试包单列，0 用例 fail/skip**。真实 SCRAM PG/PostgREST16.4 已运行并关闭；
测试前后源码摘要一致，并与四项反向原始源码、最终跨语言契约摘要吻合。
此前 1328 事件全量通过的记录保留，不替代上述最终测试源码的复验。

四项实际生产反向：放开启动预算取整、去掉原意图摘要比较、去掉存储 PG 版本
比较、允许 POST 自动重试，均触发对应业务断言，随后逐字节恢复源文件。
日志 `build/control-plan-negative/`。全仓 vet、全部 Linux/amd64 CGO0 cmd 交叉
编译通过；后者不是 Linux 运行认证。

自有 Neon `ci/test_dsg_creation_plan.py` 必须使用新证据目录，实际运行 Go 导出器
并核对准确 pass 事件，再交真实 Rust DTO `validate/postgres_settings` 消费；不会
以“无匹配 Go 测试仍退出 0”、旧 JSON 或默认 ignored 冒充互通。
11 组数据（2 有效/9 拒绝），含完整 uint64，已真实通过 1 Go/1 Rust 测试。
双方源码和数据 SHA256 记录于 `build/dsga-plan-cross-language-final2/`。
复验命令（工具链/SDK/缓存按本机或 CI 配置提供）：

```sh
python3 ci/test_dsg_creation_plan.py /path/to/william-lbn/control-plane --report-dir /new/evidence/path
```

Rust `compute_api` 默认 lib/tests 36 测试通过，新增外部契约 1 项默认 ignored；
上面的显式脚本已将其实际运行，不能把默认 ignored 那次算作运行通过。
真实 PG/VM/完整存储和整个中心网关链不在本契约脚本范围。

本批证据：`build/control-plane-plan-{focused,mac-final,mac-final2}/`、
`control-plane-plan-timeline-focused/`、`control-plan-{vet,linux-cli}.log`、
`dsga-plan-{compute-api-all,rust-contract}.log`。完整 Renderer/生产入口/worker、
运行时预算/quiesce、Monitor/最终 Runner、多 PG 及完整存储端到端继续编码；
Linux/K8s/KVM/HugeTLB/物理回收/迁移与性能按目标环境后验。
