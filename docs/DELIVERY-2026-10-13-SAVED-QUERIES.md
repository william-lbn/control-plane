# 保存查询与本地 AI Gateway 当前交付

## 1. 当前可复测结果

现场源码 **`8c1b77755e3dbf75238f28ea90cf8abe58d27a5f`**，OpenAPI **0.19.0**，
统一 Helm **0.1.27**。Go API、独立 Worker、React Console 与八个控制面组件镜像
来自同一公开提交。Neon/Postgres/autoscaling fork 源码与镜像未修改。

公开 Linux CI **38421083958** 的十四项 Job 全部成功，包含 **772 个 Go/真实
PostgreSQL 测试，零失败、零跳过**，前端、Auth、Functions、Helm 和八个镜像发布。
Linux 匿名 OCI 校验通过；三台节点串行导入，24 次摘要/源码身份校验通过。
统一八阶段 inspect/preflight/apply/verify 和已安装 manifest/PVC/Secret 审计通过。
Helm 的 15 个工具测试、十张 Chart、21 个拒绝场景和打包门槛通过。

这里完成两个新增产品闭环，仍不宣称官网全部功能或生产准入。

## 2. 从 UI 开始的真实验收

| 功能 | 现场 Job / 检查 | 结果 |
| --- | --- | --- |
| 个人保存查询 | `publication-ui-20261013153541`，六组 | 首次浏览器执行通过 |
| 新版 AI Gateway | `publication-ui-20261013153558`，九组 | 首次该版本浏览器执行通过 |

保存查询：UI 创建；响应只有元数据；同请求键重放不重复创建，改变输入拒绝；
页面重载后中文/换行完全还原；加载清空连接密码且不执行；改名与文本更新；旧
If-Match 拒绝；删除后详情 404；匿名拒绝；自有草稿保留 tombstone。Go/PG 还验证了
Owner/项目/ID/内容版本的加密绑定、历史 key 保留、同项目他人/Viewer/跨租户拒绝、
密码/结果不入模、SQL 不进入 replay/audit 明文和重放不重复审计。

AI 回归仍调用 Windows **真实 Ollama `qwen2.5:0.5b`**，经过 Linux 独立网关；
JSON/SSE、实际 token 落库、应用凭据的模型目录、真实应用调用、分支拒绝、撤销和
不支持请求拒绝均通过。修复了分支服务列表的旧“未实现”占位：已接入静态路线显示
`configured`，不把它当成上游实时健康认证。新版 AI 表单和 SQL 面板截图已人工检查。

两套测试均没有执行 SQL 或新建 Compute/Functions VM；结束后 managed VM/Runner
仍为零。草稿/凭据只退休本测试创建的对象，业务数据、历史和证据保留。

## 3. 手动复测

1. 登录现有控制台；选择可访问的 Ready 项目。
2. **SQL 工作台**：输入两行 SQL 草稿，填保存名称，点击“另存为新查询”。
3. 刷新页面，选择保存项，点击“加载到编辑器”；核对中文/换行；不用填写数据库密码。
4. 修改名称/SQL，点击“更新保存查询”，再次加载核对；删除自己的样例，确认隐藏。
5. **AI Gateway**：选择分支和 `local-small`，输入简短问题，分别测试关闭/开启流式。
6. 核对输出、Request ID 和用量；**应用凭据**页面创建 self/model 限制凭据，用
   公开分支入口调用，再撤销，确认下一次请求被拒绝。真实推理不需要数据库 Compute。

机器/模型通道必须在线。Windows Ollama 保持 loopback 监听，现有 SSH reverse
tunnel 和 Linux 受限 relay 可用于当前本地验证；公网域名和商业供应商凭据后置。
完整模型/接口/配置见 [保存查询](SAVED-QUERIES.md)、[AI 实施](AI-GATEWAY-IMPLEMENTATION.md)
和 [AI 首次交付](DELIVERY-2026-10-13-AI-GATEWAY.md)。

自动化由固定、无重试、单浏览器的 `saved-queries.spec.ts`、`ai-gateway.spec.ts`
执行。使用 protected Linux runner 或现有 Linux Job，不把密码/密钥写进命令、Git、
公开测试附件。项目输入只引用明确归属的保留测试项目。

## 4. 保留的失败与后续门槛

保存查询候选的样式格式问题、HTTP 页面的 randomUUID 限制、AI 分支状态 SQL
漏字段已修复，全部失败 CI/日志留存；未通过门槛的候选没有部署。最终请求标识使用
已有 CSPRNG fallback，未使用 Math.random。新增 `content_generation` 保留删除后
密文的正确 AAD 版本，不假设删除资源版本等于最后内容版本。

仍需开发：查询历史/共享查询、XLSX/图形计划/Time Travel UI；动态模型供应商配置、
完整协议与财务预算；Functions 公共入口/自动休眠与冷醒/分支和触发器；对象完整
S3/multipart/CORS/GC；Auth 高级流程；目录/删除/PITR 完整生命周期、精细资源、
跨实例外部栅栏、HA/DR 和全链路可信 TLS。硬件压力和异地物理证明按用户约束后置。
