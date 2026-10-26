# 时间点预览与精确历史分支

此合同补充历史恢复的时间点选择和原生范围确认。原分支 reset/PITR、跨 Backend 一致恢复和历史 SQL 查询辅助仍需各自实现与验收；预览不能代替这些结果。

项目 Reader 可调用 GET /api/v1/projects/{project}/branches/{branch}/restore-point，指定 timestamp 或 lsn 其中一项。服务读取 Pageserver 的实际 min_readable_lsn、initdb_lsn 和 last_record_lsn；时间戳由存储映射到规范 LSN。它不启动 Compute、不创建分支、不取得历史租约，也不写元数据。响应禁止缓存，明确 retention_pinned=false，返回 branch_id、source_timeline_id、lsn、window 和可选规范 UTC timestamp。

缺项、重复/未知参数、冲突、非法或未来时间、零/未对齐 LSN 返回 422。时间早于保留边界、存储无可用历史和未重放的时间分别明确拒绝。存储返回未知映射类型、非法 LSN 或不可用时不推测成功。读取存储期间分支切换到其他 Timeline 时返回 412。

创建/恢复分支窗口提供显式“预览时间点”。预览绑定当前输入和源分支；改变输入会清除结果，较晚返回的旧请求不能覆盖新选择。确认后提交预览的精确 parent_lsn 和 expected_parent_timeline_id。受理在锁定源分支后重新核对原 Timeline，并按最新保留边界为同一个 LSN 取得租约；它不会再次解析原时间戳。历史可能在预览后到期，届时明确失败。

expected_parent_timeline_id 是创建历史分支的可选范围条件，不能用于当前分支复制。旧请求省略该字段时保持原幂等摘要。无论是否提供预览条件，创建前读取的父 Timeline 都必须与事务内锁定值相同，防止把另一 Timeline 的 LSN 当成本次来源。重复原已受理请求先返回原资源/Operation，不因之后的历史或来源变化创建新分支。

预览不授予写权限；实际创建需要 Editor、Session CSRF 或合适的 API Key，以及原 Idempotency-Key。控制台创建响应丢失时保留原输入和键，重试同一请求；已知原 Operation 的状态与 retry 继续使用现有操作入口。预览结果本身不是持久恢复单或备份。

历史分支继续遵循现有原生目录与 Functions 复制合同：不会投射今天的普通角色/数据库 DDL 或凭据；Functions 可显式省略，或者在既有确认策略下继承当前冻结代码并暂停。本文不承诺将代码、外部事件、账单或密钥回滚到 SQL 的历史时间。
