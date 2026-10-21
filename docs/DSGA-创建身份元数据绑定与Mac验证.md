# DSGA 创建身份元数据绑定与 Mac 验证

2026-10-21，V3 增量。以已验证的 `60497e5` 为基线，包含自有控制面主线
`69c611de9cdb5528c16c19e54d8c099c44f085f8`。本批实现真实 PostgreSQL 元数据
UID 绑定，不再只提供创建执行器中的回调签名。**公开创建入口、
安全配置 Renderer 及实际 worker 调度仍待接线，公开 DSGA 创建保持关闭。**
后续[冻结启动计划与存储版本贯通](DSGA-冻结启动计划与存储版本贯通.md)已补资源/
身份计划、版本化 Operation 数据库生产者和原计划恢复，不代表完整创建链已启用。

## 原意图与持久事实

`api/internal/control/dsga_creation_binding.go` 只从原 Operation 读取身份 view：
`create_{project,branch,endpoint}_dsga_v1` action、协议 1、PG17、可信 namespace、
原 project/branch/endpoint/tenant/timeline、固定 incarnation/Compute ID。
这些新 action 与原 PG16 创建隔开；旧 worker 不认识时失败，不能忽略新字段后
调用旧 Driver。完整启动预算/不可变镜像/角色尚须由原生计划契约校验，不能用
本批的身份 view 充当有效启动配置。

指纹对数据库返回的**全部原始 JSONB 字节**做 SHA256，包括 view 不解释的字段。
不使用 `map[string]any` 重编码，不更新 `Operation.payload`；最大 uint64 数字
保持原样。实际 VM UID、配置 Secret UID 单独存在 `dsga_creation_bindings`，
每个原 Operation 最多一行；同一个 VM/Config UID 不能归给第二个创建意图。

加载与每次绑定都重新锁原 Operation，检查 action/摘要/资源类型与 ID、当前
running worker lease，以及未删除的父资源、PG17/tenant/timeline 和合法生命周期。
只有 VM 首次绑定可以 INSERT；Config 必须在原 VM 已绑定后由 NULL 推进一次。
原 UID 不同就拒绝；原值相同不 UPDATE，审计时间与 xmin 不变。

回调接收实际 typed API 对象并核对作用域、UID/RV、原 bootstrap、未删除状态。
配置额外核对 immutable/Opaque、唯一 VM controller ownerref 与原生 v2 VM/Compute
身份。完整 Data 的确定性比较与读回由 `prepareDSGACreation` 继续承担；不以
仅身份合法取代完整原生 spec、预算或角色核验。

## 锁、租约与故障恢复

锁序仅为 Operation 行 → 该操作绑定行。父资源只读，不在绑定事务中加父行锁，
不 UPDATE 原 payload 触发父准入锁；所有 Kubernetes/Secret/Render 网络操作都在
数据库事务外。这样不新增与既有父资源 → Operation 准入的反向锁链。

**获得行锁后**单独查询 `clock_timestamp()` 核验租约，提交前再查一次。
不能用固定在事务开始的 `now()`：worker 等行锁时租约可能已经过期。检查失败
回滚，不留下“已接受”的新 UID。只持有曾经加载的 binding 不构成持续授权，
换主后旧回调也必须失败。

VM CREATE 或 Config CREATE 丢回复后，上层重载同一 Operation 的原 UID 事实，
将其传给创建执行器的 ExpectedVMUID/ExpectedConfigUID；只能确认同一对象。
缺原对象、同名重建、原意图变化和 lease 丢失不重新初始化身份。数据库绑定成功
只证明一项元数据事实，仍不等于跨 PostgreSQL/Kubernetes 的原子执行票据；最终
Controller/Runner 必须独立检查当前持久 fence 和实际资源。

## 追加迁移 058

保留所有已应用的 001—057，追加 `058_dsga_creation_identity.sql`：

- 新 action 需协议/版本/项目范围匹配；旧创建 action 携带 `dsga_creation` 字段，
  即使字段是 JSON null，也拒绝，防止旧 worker 降级执行。
- 原创建意图的 action、作用域、payload 和授权审计身份不可改写；状态、租约、
  回执仍按既有规则推进。绑定后不能改变 VM UID、指纹或已固定的 Config UID。
- 原 Operation 还存在时不能 DELETE 绑定再重建身份；仅原 Operation 已删除后的
  FK CASCADE 清理可移除该事实，不提供重置接口。
- 完整保留新主线 041 失败资源清理守卫；042 的到期分支限制也覆盖新 endpoint
  action。不能从旧草稿复制 018 函数覆盖 041，或绕过未来到期分支禁止新 endpoint。

## Mac 实际验证

测试在一次性真实 PG、独立 schema 和实际 SQL 行锁上运行。Kubernetes 一侧为
TLS/UID/RV/RFC6902 协议夹具，调用生产 typed 客户端、完整 journal Store 与创建
执行器；这不是实际 apiserver/etcd/RBAC 认证，也不是完整 Neon spec 启动测试。

覆盖真实 UID 持久化、幂等原值/xmin、最大 uint64 原意图不变、数据库不可变
约束、VM/配置 UID 替换、旧 worker 换主、实际锁等待后租约过期；9 类准入子例
区分有效新 action、旧 action/JSON null 降级、协议字符串/未知协议、PG16/范围
错配，以及未来到期分支。最终专项 **134 个 pass 事件，0 用例 fail/skip**。

四项实际生产反向分别将租约时钟改成 `now()`、去掉原 VM UID 比较、放开原
payload 改写、放开旧 action 降级；均产生对应业务断言失败，未以编译错误充数。
每项后与最终均逐字节恢复生产源码，见工作区
`build/control-binding058-negative/summary.json`。

最初在旧基线的两项集成测试因夹具没有提供 001 schema 要求的资源时间而失败，
已按生产入口显式提供 created_at/updated_at；没有放宽 schema 或修改旧迁移。
原失败证据 `build/control-plane-binding-mac-focused/` 保留，修复后旧基线专项
124 事件通过；之后新主线组合的 134 事件才是本批专项结果。

恢复后完整 API Mac race **1262 个 pass 事件（351 顶层/911 子事件），11 测试包
通过、9 无测试包单列，0 用例 fail/skip**；实际 SCRAM PG/PostgREST 16.4 已运行
并关闭，最终源码摘要与测试前及四项反向原始源码一致。全仓 vet 与全部
Linux/amd64 CGO0 cmd 交叉编译通过；不是 Linux 运行验收。
对应证据目录：`build/control-plane-binding058-mac-final/`（含 source-sha256 与
summary）、`build/control-binding058-{vet,linux-cli}.log`。
没有构建镜像、部署或合并 main。

## 仍需实现与后续环境验收

创建生产者需把可信模式、PG 版本、不可变镜像、显式安全预算及随机身份固定到
原计划；完整 Renderer 必须隔离内部 cloud_admin 超级用户与普通租户凭据、路由
和实际角色权限。还需实际 worker 调度、预算/quiesce、Monitor/Scheduler/最终
Runner、跨 owner/boot 受控关闭、多 PG 和完整存储端到端。这些是**代码与本地
集成缺口**，不能归为只有 Linux 待测。

后续目标环境验收：真实 CRD/admission/RBAC/TokenReview/etcd，Linux/KVM/NeonVM/
HugeTLB、Host/Guest 物理内存回收、迁移、故障注入及性能长稳。Mac 的 TLS/SQL
协议与交叉编译通过不能替代这些证据。
