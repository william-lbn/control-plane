# 2026-10-16 当前交付：AI 修错与确定性计划提示

## 1. 结论

本地 **Web 功能切片 0.45.0 / Chart 0.9.35 / Helm revision 107**。
API/Worker 仍为 **0.43.0 / migration 037 / 103 paths / 147 operations**；没有新增
API、迁移或 Neon/Autoscaling 内核改动。此前受控 Schema、草稿及计划建议通路已在
`121a1eecdc034f3865ede1e300f3564c252a1e56` 推送，详见
[上一批交付](DELIVERY-2026-10-16-SQL-ASSISTANT.md)。

本批实现[AI 修错](SQL-AI-REPAIR.md)：真实 PG 失败、本页源 SQL/连接匹配、明确选入
SQL/错误、精确预览与确认、真实模型修复、独立载入并清除密码、人工执行。
固定 Explain/Analyze 测量说明由 UI 状态产生，不依赖模型断言。它没有认证模型
准确率；历史 AI 命名、其他未开发功能和[生产门槛](PRODUCTION-GATES.md)继续保留，
不能把本阶段验收视作整体完成。

## 2. 精确运行版本

- 构建父提交：`121a1eecdc034f3865ede1e300f3564c252a1e56`；实际运行源码由本地不可变快照识别。
- Web 快照：`eba21c1b295aa8d4368d63f770c1bb6d028ac21d07edfc9a2496d407e5afa75c`。
- Web 镜像：`neon.local/control-web@sha256:b28239b8d7c90e33808dbb670a2891ac656f8c951c647a3d59ef6cbf7021c924`。
- API/Worker 镜像保持 `neon.local/control-api@sha256:5a0bcdf294ae7b35120e76f49327002b22e625efa5405dc74c2aeb1ea950449c`。
- API/Worker 源快照保持 `18f34cad7281b7db8137f964a12bde203e5d1b9e9080cae10912c6b95693a3c4`。
- 最后 metadata backup SHA-256：`80cf55fb24b2b74b32e7f4ff4665bdbee16b2b24edca22dfeebf98093c78d6e2`。
- 最终 UI Job：`publication-ui-20261016052313`；运行镜像的实际 COPY 输入与当前 API/Web 文件重新核对一致。

Web 仅在 Linux 本地构建，三节点 digest 导入；保留 API/Worker/AI/数据面二进制镜像。
三次窄 Helm 升级分别部署 Web、运营模型候选和可用模型；每次保留 9 个 PVC、所有
既有 Secret UID 与其他六个 Neon 发布，先保存 metadata/values/manifest。
新增不可变模型配置仅修改原受限本地上游的模型路由；原 0.5B 路由、供应商凭据、
旧配置和失败记录都保留，无外部模型账号输入。

本地当前目录提供 `local-small → qwen2.5:0.5b` 与 `local-sql → qwen2.5:1.5b`，
后者用于本批修错。模型选择是产品目录输入，公开代码不硬编码用户供应商密钥。
Windows 只承载本地 Ollama 上游；控制服务、构建和 Playwright 均在 Linux。
最新本地镜像尚未统一公开发行；公共统一 Helm 0.1.31 的 0.23.0 锁不能冒充此版本。

## 3. 新增验收和失败

3 项新增 Linux 纯函数测试覆盖双上下文选入、错误格式/UTF-8 界限、拒绝控制符、
额外 transport/凭据字段不投影，以及普通草稿不附带错误。受影响的既有 6 项助手
检查一起通过；类型/格式/Vite 与 51 个可信 live suite 的声明/准入检查通过。
没有重跑其他旧全量功能套件，也没有等待 GitHub CI。

真实 UI 6 条 ledger（含收尾）通过：

1. 原 fixture 的 `SELEC 43::integer AS ai_fix_value` 经真实 Proxy/PG 返回 422，
   原历史 `qh_3192e3e76ef2128cbcd0fb8c` 保留；没有自动模型调用或 SQL 重试。
2. 编辑器与原失败不匹配时隐藏修复入口；选入 SQL/错误后完整预览，取消错误选择
   会重置确认并禁用生成。未附带连接密码、角色或库名。
3. 真实 Qwen 1.5B 通过 Go Gateway 返回修复草稿，模型 request_id
   `air_2284007d335d6bf23d35df4e`；此时数据库 POST 仍只有第一次失败请求。
4. 显式载入后密码为空，旧错误清除；人工输入密码/提交原模型草稿后实际返回 43。
5. 人工 Explain 后显示确定性的“仅估算/无实际行数与耗时”说明，切换未请求模型。
6. 结束 Compute/VM 归零，原失败与成功历史、业务数据和模型输出证据保留。

本次共 **3 次数据库请求、1 次模型请求**，不把它们算成完整官网回归。
最初 0.5B 模型原样返回错误 SQL，前端拒绝载入且未执行；随后已有 14B 模型加载
报文件读取错误，上游拒绝被准确呈现为 502，8B 的独立模型加载探测同样失败。
磁盘有余量，未未经证据归因或修改原模型文件。下载约 986 MB 的 1.5B 模型并通过
摘要验证和加载检查后，才接入同一受限路由并进行上述真实 UI 验收。

原失败 history ID `qh_228351b8d62c3fa0f494391e`、`qh_87e33a4f7c3d1ba0d0cb2ade`
和原模型响应保留。没有把错误 SQL 人工改写成模型输出、放松识别器或用假供应商
替代真实模型。该样本通过不保证其他模型/SQL 都能正确修复。

## 4. 资源与继续工作

4 个本阶段自有终态 Job 先归档描述和全部日志，再按 UID/resourceVersion 退休。
最后 3/3 节点 Ready，无 Pressure；87 个工作负载与期望一致（含有意的 0），错误
Pod 0、托管 VM 0。所有文件、OCI、原配置、fixture 和失败材料保留。

完整维护者证据位于 `control-plane/production/evidence/20261016-sql-ai-repair`，
私密模型输出、DOM、口令和 SSH 工具不进入公共源码。产品仍为 Go/PostgreSQL 与
React/TypeScript。随后继续历史查询命名，再推进其他[未完成需求](REMAINING-REQUIREMENTS-2026-10-16.md)。
