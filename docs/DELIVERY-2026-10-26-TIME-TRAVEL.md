# 2026-10-26 时间点预览与精确历史分支验收

时间点预览和按原 Timeline/LSN 创建独立历史分支已完成基础验收。这是 L02/L03 的必要增量，**不关闭原分支 reset/PITR、历史 SQL 查询辅助或完整 Backend 一致恢复**。当前仍为 22 个基础组有回执，剩余五个业务组及 O01/T01/O02；性能、可靠性和完整容量保持后置。

新增 GET restore-point 按实际保留边界将时间戳或 LSN 解析到原 Timeline，不启动 Compute、不取得租约、不写元数据。控制台显式预览绑定源分支和输入，较晚到达的旧响应不能覆盖新选择；确认后使用精确 parent_lsn 和 expected_parent_timeline_id。创建事务核对原来源，随后为同一 LSN 取得租约；省略新字段的旧请求保留原摘要，重复已受理请求恢复原结果。

两组新增 Go 检查通过，分别覆盖原生 HTTP 映射/只读边界和真实 PostgreSQL 权限/来源切换/精确准入/原键重放。六组既有受影响检查通过，包括两组恢复校验、三组合同和历史分支集成。后者初次因测试 schema 环境变量缺失未启动；补齐专用 schema 后复用原编译产物，只重做该一组。随后将新增检查中的并发对端状态改为原子变量，两组新增检查的 race 复查及 vet 通过，未再次计为新功能。三个临时 PostgreSQL 测试 Pod 均按 UID 归档退休。

TypeScript、Vite、格式、API 索引与套件注册检查通过。新增可信恢复套件独立列出，仍为单 worker、零自动重试；后续测试脚本改动仅补格式/编译检查，复用原 TypeScript/Vite 结果。真实页面首次选择器误将组织切换按钮当成 select，失败发生在任何预览或创建前；修正为现有 listbox 模式，保留原测试检查点。Endpoint 保留核对业务身份和状态，不比较每次读取都会变化的观测时间。

真实页面通过四个检查点：原分支只读 LSN 预览；服务端受理后模拟 POST 响应丢失，页面以同一输入和键恢复原 Operation；Pageserver 实际创建的历史分支使用精确预览 LSN；原请求重放保持同一资源和 Operation，源 Endpoint 身份/状态保留，无新增 Compute。Function 复制显式选择 omit，原 Function 和 Backend 保留。最终直接读取原生 Pageserver，再核对目标祖先 Timeline、LSN、原 Operation 成功及源 Timeline 未变，全部通过。

API/Worker 0.92.0 / Chart 0.9.89 / schema 069 / Core Helm 193，原生格式 7 不变。API/Web 各构建一次，经内网导入三节点，其余组件复用，无新增外网依赖下载。968 个非文档输入、558 个 Go/SQL 输入与最终构建相同；唯一新增测试文件的受检查实机覆盖摘要另列，运行时代码未因测试修正重建。

原 971 个 Secret UID（包含 C02 之前的 955 个）、九个 PVC、六套其他 Helm 发行保留；17 张目录表和三个既有额度表在专用来源/目标之外的范围摘要一致。旧 Auth/Data API 私有文件、Function SQL 原代际和当前代际 2 回执仍保留，付款账本与默认关闭状态不变。三个节点、三个 Safekeeper、七个关键组件就绪，Auth/Compute/VM 和非退休 Function 实例为零，bootstrap 148。七个终态 QA Job 包括失败尝试按 UID 归档退休，专用历史分支、原始请求和所有业务字节保留。

见[功能合同](TIME-TRAVEL.md)、[结构化验收](acceptance/2026-10-26-time-travel-functional.json)、[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)。下一步继续实现原分支备份、原地切换、回退及 Backend 恢复语义；不会把已有独立历史分支当作原地恢复。

提交 `2283b1fa37bce34e78f9d5b172f98f5bece852f4` 的 [GitHub 运行 40250401484](https://github.com/william-lbn/control-plane/actions/runs/40250401484) 已完成，15 项检查全部成功。此门槛在后续功能提交归档，不再次运行本轮已通过的页面或原生存储检查。
