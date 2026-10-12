# SQL 计划与结果导出

## 1. 范围

沿用下载的官方 SQL Editor 文档：Explain、Analyze、结果下载是 Console 功能。
本切片提供真实 PostgreSQL JSON 计划和当前有界结果的 CSV/JSON 下载。
不依赖第三方账号，不增加 API/Worker 服务或元数据表；XLSX、查询历史/保存、
Time Travel UI、AI SQL 和图形计划分析仍有独立开发门槛。

## 2. 执行与数据合同

单条模式新增 **查询操作**：普通执行、Explain、Analyze。
Explain 发送 `EXPLAIN (FORMAT JSON) <SQL>`；Analyze 发送
`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) <SQL>`，仍通过现有 Go `/query`
授权、CSRF、公共角色、Proxy、TLS 和 PostgreSQL extended protocol 单语句解析。
不在浏览器拆分 SQL、不执行客户端正则授权、不自动重发请求。
脚本模式不提供这些单条操作；用户仍可在脚本中显式写标准 PostgreSQL 语句。

Analyze **实际执行语句并可能提交写入**。选择后显示持续提示，按钮明确叫 Analyze；
不能把它描述成只读诊断。切换项目/Endpoint 重置到普通执行。SQL 计划以 React
转义 JSON 显示，不作为 HTML。密码遵守现有一次请求清除规则。

下载只读取当前浏览器中已经返回的结果，不再次查询或唤醒 Compute，不上传数据。
CSV 使用 UTF-8 BOM、CRLF、双引号字段及引号转义；危险字符串（含前置空白的
`= + - @`）加单引号前缀，避免常见表格公式执行。数字不改值。
CSV 的 NULL 为空字段，且公式文本经过保护；需保留原始类型请使用 JSON。

JSON 格式为 `neon-console-result-v1`，保留 `columns`、位置型 `rows`、`command`、
`truncated` 和已有结果元数据，避免重复列名覆盖、NULL/对象丢失。
JSON 保存的是服务返回值，不将已舍入的数值恢复成数据库原始精度。
脚本的大整数字符串保持精确。结果已截断时显示明确提示，导出的也是已显示子集，
不宣称全量导出。每条脚本结果独立下载；无结果列的命令不显示下载按钮。

## 3. 验收

Linux 单元测试覆盖 CSV 公式/分隔符/换行/Unicode 与 JSON 重复列、NULL、嵌套值、
精确字符串和截断元数据。`sql-tools.spec.ts` 从实际 UI 登录，复用已验收 SQL fixture：
Explain INSERT 后新连接确认没有写入；显式 Analyze INSERT 后确认实际持久写入；
Chromium 下载并校验真实 CSV/JSON 文件；脚本截断下载保持 200 行和标记；最后正常缩零。
保留写入和证据。失败后先核验原始 checkpoint，不能盲目重放 Analyze 写入。

本版实际现场 UI 验收待精确版本回执。独立全链路 TLS、分布式准入、HA/DR、
原始全量导出、CSV 客户端的所有解释差异和官网全部 SQL Editor 功能未由此放行。
