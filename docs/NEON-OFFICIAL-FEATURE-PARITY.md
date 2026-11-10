# Neon 官方功能对标与自托管控制面验收矩阵

对标范围：2026 年 Neon 公开提供的 Serverless Postgres 和 Branchable Backend 原语。本仓自托管机器合同目前有 225 个 API 路径、315 个操作，属于 /api/v1；**不能将它称为 Neon SaaS /api/v2 的完全兼容实现**。

Neon 官方资料：
- Branch/Endpoint API：https://api-docs.neon.tech/reference/createprojectbranch
- Compute API：https://api-docs.neon.tech/reference/createprojectendpoint
- 后端 GA：https://neon.com/blog/neon-backend-is-ga
- 分支身份：https://neon.com/blog/neon-auth-branchable-identity-in-your-database
- 分支对象存储：https://neon.com/blog/building-neon-object-storage
- Functions：https://neon.com/blog/neon-functions-backend-logic-next-to-your-data
- Neon MCP：https://neon.com/blog/give-your-agent-neon-tools
- Realtime：https://neon.com/blog/neon-now-has-realtime

| 官方功能域 | 当前可核实的控制面实现范围 | 生产层面缺少的验收 |
|---|---|---|
| Projects、Organizations、授权、Operation | 对应管理源码与机器合同存在 | 实际多租户授权、生命周期、故障恢复 |
| Branch、CoW、PITR、Restore、数据库角色 | Branch/Restore API 和元数据状态机存在 | PG17/18 真正 Tenant/Timeline、SQL 和 WAL/LSN 一致性 |
| Read-write/Read-only Compute | Endpoint 和 Replica 模式合同存在 | 真新实例启动、只读副本、故障重放与连接路由 |
| Autoscaling/Scale-to-zero/在线内存伸缩 | DSGA 分层合同与 Endpoint Autoscaling 只读查询已接入 | PG Buffer Pool、Guest/VM/Host 资源回收、Budget、Owner、故障恢复 |
| Neon Auth | Managed Auth API 和服务源码存在 | 分支隔离的真实用户、会话、配置和 OAuth 故障恢复 |
| Data API/RLS | Data API 接口和 PostgREST/RLS 集成测试存在 | 新真实分支上的 SQL、JWT、RLS 和权限撤销 |
| S3 Object Storage | Bucket、Object、Multipart 等源码存在 | 对象版本、分支继承、哈希、故障域与持久化 |
| Functions/Triggers/WebSocket | Functions 的构建、部署、调用、触发器合同存在 | 真实实例运行、分支继承、不可重放副作用和调度 |
| AI Gateway | Provider、Models、Usage、Budget、Billing 管理能力存在 | 真实调用、额度守恒、断网/重试和租户隔离 |
| CLI/MCP | 客户端操作目录与只读/审批写入工具存在 | 与当前运行的已认证 API 的集成 E2E |
| Realtime/Live Query | 当前管理 OpenAPI 未形成完整 Realtime 生命周期合同 | 独立分支订阅、增量变更、Scale-to-zero、Replica 和连接权限 |
| 可观测与发行 | 部分源码和测试证据存在 | 跨仓签名 BOM、持续压力 SLO、灾备切换和正式 CI |

## 成熟度标记

- SOURCE_PRESENT：存在实现代码和/或接口，不证明 K8s 已部署。
- CONTRACT_PASS：单元测试、Race 或 API/Helm 合同通过，不证明业务数据一致性。
- SANDBOX_E2E_PASS：在隔离 K8s 上完成真实操作，具备原 UID、LSN、操作回执和日志证据。
- PRODUCTION_GATE_PASS：通过多节点故障域、真实数据、并发恢复、性能、安全和可发布 CI。
- BLOCKED/UNPROVEN：关键证据不齐时不得发版。

目前不能把上述所有功能认定为 PRODUCTION_GATE_PASS。优先实现 PG17/18 数据库核心、Autoscaling、Branch/WAL 和多故障域验收，再逐步补齐 Realtime 等新增原语。
