# SQL Explain / Analyze 计划树

## 范围

对工作台通过 Explain/Analyze 动作实际返回的 PostgreSQL JSON plan 增加可折叠树。服务端仍走原授权、CSRF、角色、Proxy 和 PostgreSQL 查询协议，不增加计划执行服务。普通查询即使返回形似计划的 JSON，也不自动冒充 Explain 结果；结果和请求模式绑定，切换请求后不沿用旧节点折叠状态。

解释依据 [PostgreSQL 16 Using EXPLAIN](https://www.postgresql.org/docs/16/using-explain.html)：估算成本不是毫秒；父节点成本包含子节点，不能直接求和。Analyze 的实际行数/时间按循环报告，不把它们伪造为语句总耗时；执行总时间仅展示顶层真实 Execution Time。

## 模型与 UI

planModel.ts 读取顶层 Plan 与已知标量指标，生成有固定路径 ID 的 PlanNode 树。保留 Node Type、Relation Name、估算行/成本、实际行/循环/时间、实际共享/临时 block、过滤行数、排序方式/空间与索引名称。不存在或非法的指标显示缺省，不填假 0；真实 0 正常展示。不会拼接关系名为 HTML、URL 或 SQL。

React QueryPlan 只展示已执行请求的结果。Explain 明确为估算且不显示实际执行指标；Analyze 显示实际执行提示、顶层规划/执行时间和实际节点指标。每个子树可独立折叠，使用原生按钮和 aria-expanded，键盘可操作。原始 JSON 表格与 CSV/JSON/XLSX 导出仍保留。

最多展示 200 个节点、32 层深度，超限给出可见提示，原 JSON 不被修改。未知或不完整格式回退到原 JSON，不使工作台崩溃。仅绘制当前收到的计划，不额外访问数据库、网络或遥测服务。

## 验证

两项新单测验证真实字段、合法零值、估算/实际区分、节点 ID、未知输入和树大小/深度上限。web/e2e/sql-plan-tree.spec.ts 仅提交两条新查询：Explain 和只读 Analyze 的排序/Limit 查询；验证真实三层树、折叠不发 SQL、实际行数来自原计划、原 JSON 保留，最后正常缩零。

该 UI 是基础树形分析，不包含统计建议、计划 diff、图形画布、自动索引或 AI 优化。Analyze 仍可能执行用户写入语句；原持续警告和显式按钮保留。生产 SLO/长期采样与数据库诊断权限属于独立能力。
