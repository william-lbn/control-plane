# PG17 安全启动 Renderer 与 Mac 契约验收

控制面代码 `7c5da7fee6c60c8c73270647f46f1c277d65d6cb`；自有 Neon 门禁代码
`a9ce6453b4ff00bff61ecf511f8e447f61e95f91`。本阶段只做 PG17，其他版本暂缓。
每仓沿用既有 V3 汇总分支，不增加需要 Linux 拼装的模块分支。

本批完成**安全 bootstrap Renderer 核心及真实解析契约**，尚不是完整公开创建
链。完整 catalog/worker/凭据来源回执、预算和最终执行端仍须编码；不能称为
只有 Linux 待测。默认关闭，不构建镜像、不准备/执行部署、不合 main、不发布。

## 生产边界和输入来源

旧 compute-public-config 资产有 cloud_admin 租户口令、fsync=off、MD5 和未经
预算登记的后台扩展，不复用为 DSGA 模板。新 renderDSGAComputeBootstrap 使用
原 Operation 已冻结的 PG17 计划、实际 VM UID、已持久化业务/探针 verifier 和
管理公钥。无 env/时钟/随机数/网络/文件副作用，不生成盐、时间、UUID 或密钥。
相同原输入产生逐字节一致的完整 ComputeConfig，拒绝时不返回部分文档。

原生 v2 七字段以 uint64 typed 结构直接编码，不经 float64 重写预算。UUID/
时间保持原计划；tenant/timeline 和 primary/replica 存储字段使用原 payload。
当前使用现有 unsharded 服务拓扑，Replica 明确提供 Safekeeper 恢复参数，不将
单独 standby.signal 当作实际追赶。多 shard/拓扑版本冻结仍需完整创建集成。

业务为 app_owner 和 control_probe；数据库 neondb 归 app_owner。显式负权限
覆盖原生角色创建默认值：NOSUPERUSER/NOCREATEROLE/NOREPLICATION/NOBYPASSRLS，
不向内部 SU 写业务密码；实际原生权限图仍负责既有组/成员路径。业务 owner
CREATEDB，探针 NOCREATEDB，连接上限100/4属于本 bootstrap profile，不是完整
运行时内存预算承诺。公钥进入 JWKS；Renderer 从不接收或输出签名 seed、明文
业务密码或内部 PG 口令。TLS只固定 /run/neon/management 下的路径，实际证书
来源绑定/挂载/轮转须后续 worker 接齐，不能据序列化成功称为管理 TLS 已运行。

fsync=on、SCRAM、neon/pg_stat_statements 的明确 preload；当前禁用 LFC 自动
缩放并给出零 max cache，未登记的扩展/profile 不自动沿用。完整预算准入、旧备份
覆盖项和运行时后台工作仍须独立核验；此 Renderer 不提供 SQL/VM 缩容许可。
CatalogSpec/继承策略这类未冻结运行时投影明确拒绝，不能字符串拼入启动配置。
完整 catalog 尚未实现，不能把这个拒绝当作最终支持完成。

调用方仍须从原 Operation 核对完整计划，并绑定原凭据 Secret UID/内容；RO
使用源 timeline 已冻结的 verifier。当前纯函数本身不读取 Secret，不替调用方
证明来源。公开 handler/真实 worker 尚未调用此 Renderer，旧模式保持原行为。

## Mac 实际验证

新 ci/test_dsg_renderer.py 使用新的独占证据目录，运行本次 Go 代码，再让
Neon 的真实 ComputeConfig/ParsedSpec（含受管角色策略）消费本次生成的完整
配置；不使用手写 Rust JSON 替身或历史导出文件。

| 范围 | 本次结果 |
|---|---|
| Go 新增 race | 4顶层/15个pass事件，3.191秒，0失败/跳过 |
| 主/只读安全配置 | 重复渲染一致、原UUID/时间/整数预算、普通角色及负权限、fsync/SCRAM/preload、RO恢复参数通过 |
| 非法输入 | 9类拒绝：版本/预算/VM、MD5/明文probe、缺失/零公钥、未冻结catalog/策略；无部分输出 |
| 创建屏障组合 | 生产typed客户端经本地TLS API夹具，验证create→原UID绑定→journal→immutable配置→Config UID绑定→释放；恢复不重复写入 |
| Rust 真实消费者 | 1通过，0失败/ignored/filtered，2份完整主/只读配置；真实生产入口还拒绝业务身份改绑cloud_admin |
| 静态 | 全Go vet、Linuxamd64 CGO0全部cmd交叉编译通过；受影响Rust clippy通过38.75秒，依赖已有warning保留，无新增compute_tools warning |

本门禁不启动 PG/VM；创建屏障使用 TLS API 夹具，不代表实际 Kubernetes API/
admission/etcd 验收。旧 PG/SDK/Go/控制面业务测试未重复；不改变它们原 SHA 范围。
原生 PG仍为已发布 fcada44b，Autoscaling仍8e1e，均无源码改动。

Rust最终格式化仅空白变化，逐一比较全部非空白 token 和字符串字面值一致，
记录原始/最终 SHA256，未为格式化重复业务测试。最初 clippy 漏 SDK/CC 导致
jemalloc C编译失败；原失败保留，补齐 Mac SDK/CC 后通过，未改业务代码规避。
第一次命中旧 deprecated rustfmt 不支持 edition，改用已安装1.88工具链检查。

源码、格式复用证据、日志摘要见[机器记录](acceptance/dsga-pg17-renderer-mac-20261023.json)。
2026-10-23 13:00 fresh fetch 的 Neon/Autoscaling/Neon PG17/社区 PG17 与自有
控制面 main 全部祖先核验通过，快照随机器记录保存；没有新维护需要合入。

复验命令（由环境提供工具链/SDK/离线依赖；必须使用新目录）：

```sh
python3 ci/test_dsg_renderer.py /path/to/william-lbn/control-plane --report-dir /new/evidence/path
```

脚本在自有 Neon 仓库执行。严格要求4个 Go 顶层、零skip/fail、两个不同模式的
本次导出文件及实际 Rust 1项通过；源码期间变化即失败。测试默认ignored不是
通过，只有上述显式导出/消费实际运行才计入本批结果。

## 后续实施与 Linux 边界

下一步接齐原凭据 UID/内容及完整冻结 catalog、公开创建选择/真实 worker、VM
模板/TLS挂载与原 Operation/VM/Config/boot/镜像能力初始化回执；随后完整预算/
quiesce、Monitor/Scheduler/Controller/最终Runner fencing、跨owner/boot关闭及
真实 Neon 存储链 E2E。上述是业务代码缺口，不改记成 Linux 环境不足。

Linux 后验为同版构建回归、真实K8s CRD/RBAC/TokenReview/Lease/UID/admission、
Provider/HugeTLB/KVM/NeonVM/virtio-mem Host/Guest物理回收、迁移/故障恢复及
性能长稳；交叉编译和本机TLS协议夹具均不能替代目标环境认证。
