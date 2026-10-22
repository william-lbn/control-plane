# AI 原生协议与累计预算

本增量由 API / 独立 AI Gateway 0.73.0、Web、迁移 060 和 Chart 0.9.70 实现。当前现场验收见 [2026-10-22 交付](DELIVERY-2026-10-22-AI-NATIVE-BUDGETS.md)。已有真实 Ollama Chat/SSE 和供应商配置回执复用；本次协议验收使用明确标注的合成供应商，不能据此声明外部商业账号或视觉模型已验证。

## 原生请求

以下入口都要求有效的分支应用凭据和目录明确发布的模型协议。模型别名映射到已批准上游；客户端不能指定供应商地址、密钥或重定向。

| 协议 | 分支入口后缀 | 已实现范围 |
|---|---|---|
| Chat Completions | `/v1/chat/completions` | 文本、内联图片、custom function tools、JSON/SSE、`max_tokens` 或 `max_completion_tokens` |
| Responses | `/v1/responses`、`/openai/v1/responses` | 无状态 input、custom function tools、内联图片、原生事件和 usage |
| Embeddings | `/v1/embeddings`、`/openai/v1/embeddings` | 单条/有界批量 input、dimensions、float/base64 输出 |
| Messages | `/v1/messages`、`/anthropic/v1/messages` | 原生 messages/system/tools/thinking、JSON/SSE、缓存用量分类 |
| Gemini | `/gemini/v1beta/models/{alias}:generateContent` 或 `:streamGenerateContent?alt=sse` | 原生 contents、内联数据、functionDeclarations、JSON/SSE、累计用量 |

完整前缀是 `/ai/v1/{branch}`。模型列表位于 `/ai/v1/{branch}/v1/models`。Bearer 分支凭据适用于所有入口；Messages 也接受 `x-api-key`，Gemini 也接受 `x-goog-api-key`。这些客户端凭据仅供平台鉴权，供应商认证由服务端独立生成。Messages 固定使用受支持的 `anthropic-version: 2023-06-01`，不转发任意 beta 或供应商账号头。

协议对象直接保留，不把 Chat 请求改名后冒充其他协议。Responses 保留 `response.*`，Messages 保留事件名称、工具和 thinking 增量，Gemini 使用原生候选项和用量对象。依据 [OpenAI 流式文档](https://developers.openai.com/api/docs/guides/streaming-responses)、[Embeddings 合同](https://developers.openai.com/api/reference/resources/embeddings/methods/create)、[Messages 流式合同](https://platform.claude.com/docs/en/build-with-claude/streaming)及 [Gemini GenerateContent 合同](https://ai.google.dev/api/generate-content)实现各自边界。

每次请求至多尝试一个上游。目录、凭据、当前组织权限和模型限制在接纳时检查。已有 HTTP DNS/IP 固定、私网精确审批、无重定向和受信代理规则不变；内网不经外网代理。平台生成的请求 ID 通过 `X-Client-Request-Id` 供供应商对账关联，不接受客户端替换。

请求上限 32 KiB，回复上限 1 MiB，输出上限由模型目录约束且不超过 4096。输入数组/消息为有界集合；内联图片也计入请求大小。这是明确的有界协议合同，不能声明完整厂商 API 覆盖。供应商自有 file/conversation/cachedContent 等引用、Responses 存取和后台执行、远程图片 URL、供应商托管工具保持拒绝，防止共享上游账号的跨分支引用。Responses 强制 `store=false`、`background=false`。工具结果由应用执行，Console 只展示。

Console 可选择已发布协议并编辑原生请求 JSON；Responses/Messages/Chat 可流式显示，Gemini 与 Embeddings 的 Console 表单使用非流式调用，公开 Gemini SSE 已验证。0.73 的供应商发现采用 OpenAI-compatible `/v1/models`；后续 0.74 已独立验收[Anthropic/Gemini 原生发现与发布](AI-NATIVE-PROVIDERS.md)。商业账号和其余完整能力仍分别留证。

## 价格与预算

运营方为模型发布显式价格版本，货币固定 USD，使用整数微美元 / 百万 Token。输入、输出、缓存读取、缓存写入四项费率都必须显式提供；遗漏费率不会默认为免费。免费模型必须显式发布四项零费率。价格随不可变目录快照保存，不因后续编辑改变在途请求。

项目管理员通过 `PUT /api/v1/projects/{p}/branches/{b}/ai-gateway/budget` 设置累计额度，使用 `version` CAS 和 `Idempotency-Key`。金额是整数 `limit_micros`；Console 支持最多六位小数的 USD 输入。额度累计且不自动按月清零；修改、停用或重新启用策略均保留已结算和预留金额。Viewer 可读预算，Editor 不能修改预算。

启用预算时，无价格版本的模型在调用前被拒绝。接纳事务在同一分支锁内保存请求、固定价格并预留输入容量上界和请求输出上界所需金额；输入按四类价格中最高者保守预留。已用、已预留和本次预留之和超过额度时返回 429，不调用供应商。输入容量上界需由运营方据模型上下文配置。

收到完整有效用量后按固定价格计算实际费用，四类 Token 金额相加后向上取整到微美元，原子更新请求、预留和不可变账本。供应商报告超过已配置上界时保存真实确认用量及费用，并拒绝后续超额请求，不能把差额截断成虚假成功的硬上限。这是平台费用控制，不是外部供应商账单准确性、支付或预付 credit 系统的认证。

| 状态 | 资金处理 |
|---|---|
| `reserved` | 已接纳、尚未确认结果；持有预留 |
| `settled` | 已取得完整用量或运营方确认；扣实际费用并释放余量 |
| `released` | 确认发送前失败，或运营方证明供应商未接受；释放预留 |
| `uncertain` | 已可能接受但超时、取消、断流或缺少有效用量；继续持有预留 |

请求过期只结束并发占位，不自动退回未知费用。Messages 初始 `output_tokens=0` 和 Gemini 中间片段不能冒充最终用量。取消请求后，独立结算上下文仍处理账本；结算存储失败保留待处理预留。提示词、回复、供应商密钥不进入请求或预算表。

## 对账与部署

`GET .../ai-gateway/budget` 返回策略、预留和是否待对账。只有独立白名单中的平台运营方可调用 `POST .../ai-gateway/requests/{id}/reconcile`，提交完整一致的 Token 计数，或明确确认供应商未接受，并提供证据 SHA-256。证据原文保存在运营方保护目录；不能凭超时、用户取消或未知结果推断免费。已终结的结算不可覆盖，同一幂等键重放不会重复扣费。

迁移 060 增加协议/缓存计数、预算策略、预留、不可变账本及幂等回执。迁移后执行 [最小权限授予脚本](../tools/provision-ai-budget-grants.sql)：独立 Gateway 只能更新资金计数和结算列，不能改额度、启停策略、改预留价格或删除账本，也不能读取未发布供应商凭据。没有放宽 Kubernetes 权限。

仍保留的独立工作：原生厂商发现/验证注册表、商业账户实际协议调用、完整多模态/托管工具与有归属证明的持久引用、预付 credit 与账单修正、组织 BYOK、TPM 等扩展策略，以及被删除资源的运营对账入口。性能、压力、HA/可靠性仍在其他环境验收。本增量不关闭整个 I02 或 O03。
