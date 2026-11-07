# 2026-10-27 存储与 Functions 功能增量

本次交付新增安全孤儿清理、独立 Storage 网关、Functions Edge 与用量代码，以及恢复源和只读备份检查。新增检查已通过，业务部署仍保持 API/Worker 0.92.0、Core 193、schema 069、Native Storage 7。070–073 尚未应用；本报告不把源码检查当作实机部署完成。

| 增量 | 已实现及已验证 | 尚需完成 |
|---|---|---|
| S03 | 上传前持久分配登记、发布前证据、独立删除身份、精确计划/执行、未知结果核对；4 组纯函数及 5 组 PostgreSQL 通过 | 专用新对象实机删除；完整已发布/历史引用 GC 的原生 inventory、lease-aware horizon 和持续 fence |
| S05 | 独立 S3/签名 REST 数据进程、受限 metadata/RBAC、跨进程准入与可信 HTTP 字节账本、原公开入口接管配置；6 组纯函数、1 组 PG、5 组 Helm 通过，独立 binary 编译通过 | 受限身份实机小对象/事件检查、原 NodePort/Web 接管；物理容量归属和计费完整性 |
| F06 | 独立 Edge 直达 Guest，私有 mTLS broker、一次性委派、可信回执、原 boot cgroup 资源采样；6 组纯函数、2 组 PG、5 组 Helm 通过，独立 binary 编译通过 | 离线更新 Guest、应用 073、部署并验收 HTTP/SSE/WS、计量和维护式版本切换 |
| L02 | 无 Compute 历史源解析、完整备份身份、只读 Static 原生检查；累计 20 组唯一新增恢复检查通过 | reset API、Worker 全流程、原子切换/回退、UI 与真实原地恢复 |

三组并行 Go/PG 合计 24 个唯一新增顶层组，vet 通过。GC 首轮五组因夹具缺少 projects 时间字段失败；补齐夹具后只重跑这五组，其余 19 组结果复用。没有弱化断言。

新增 Helm 合计 10 个唯一组通过。初轮八组通过，两组发现新投影权限的八进制解析歧义，已改为明确十进制 288；续验一组通过，另一组因测试归档遗漏公开合同文件失败，补齐输入后仅续验该项通过。旧业务组没有因这些修复重跑。公开 API 已同步为 224 路径、314 操作；1 组新增及 3 组受影响合同检查和生成文档检查通过。

L02 新增实机检查复用原历史分支和已编译程序，验证原生 recovery/read-only、Proxy TLS 与原 SCRAM、原小业务行、写事务拒绝、原 Timeline head 保留及私有 VM/Service/selector 清理。原 Job 按固定 UID 退休，1099 个原 Secret UID、9 个 PVC 和原私有文件摘要保留。没有执行实际 reset，也未新建等价历史分支。

F06 当前版本合同是先完成长流、停止并确认原实例退休，再部署/回滚；活动实例直接 redeploy 返回 409。尚无热替换保证。HTTP body、WS 两个方向和真实 cgroup 观察分别记账；缺样本、丢回执和未知结果不记作零或成功，也不冒充发票与物理网络计费。

环境清理删除过期构建包、缓存和已验证后不再需要的测试 binary，保留源码、日志、验收回执和恢复数据。没有删除或合并 VM 业务磁盘及 fsck 前快照。后续不再向系统盘迁移数据；内网直连，外网才使用已有代理。性能、可靠性、HA/DR、压力和完整大对象字节验证继续后置。

固定基础计数仍为 22 组；S03/S05/L02/L03/F06 和 O01/T01/O02 仍开放，五组专项后置。详细合同见 [GC](OBJECT-STORAGE-GC.md)、[Storage 网关](STORAGE-GATEWAY.md)、[Functions Edge](FUNCTION-EDGE-METERING.md)；[结构化回执](acceptance/2026-10-27-functional-slices.json)记录各项范围。

提交 `6ed00f5` 的 [CI 40367025950](https://github.com/william-lbn/control-plane/actions/runs/40367025950) 中 Go/race/真实 PG、Helm、Web、Functions、Auth 与工作流检查通过；客户端目录检查仍使用旧的 308 操作总数而失败。已同步为合同实际 314 操作，并在缓存 Linux Node 只续验该项通过；镜像发布因该失败跳过，不能宣称该次 CI 全成功。
