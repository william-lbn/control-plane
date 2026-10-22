# 原生供应商发现与发布

0.74.0 增量在已有[供应商管理](AI-PROVIDERS.md)与[原生推理/预算](AI-NATIVE-PROTOCOLS-BUDGETS.md)之上增加供应商类型。源码、数据库检查和[八项现场验收](DELIVERY-2026-10-23-AI-NATIVE-PROVIDERS.md)分别留证，页面或模型目录存在不等于真实商业账号已验收。

| 类型 | 模型发现 | 发布协议 |
|---|---|---|
| `openai_compatible` | 原有 Bearer `GET /v1/models` | 运营方显式选择受支持协议；保留既有 relay 行为 |
| `anthropic` | `GET /v1/models`，`x-api-key`、固定 `anthropic-version: 2023-06-01`，`after_id` 分页 | 明确的 `messages` |
| `gemini` | `GET /v1beta/models`，`x-goog-api-key`、`pageToken` 分页 | 明确的 `gemini` |

运营方仍填写规范的供应商 API 根地址 `/v1`；Gemini 适配器在同一已批准 origin 内使用固定原生路径。没有扩大私网审批到其他主机或端口，也不接受客户指定发现路径、任意请求头、重定向或密钥查询参数。内网请求保持直连，公网沿用现有固定 IP/TLS 和受信 CONNECT 代理规则。

分页依据 [Anthropic Models API](https://platform.claude.com/docs/en/api/models/list) 和 [Gemini Models API](https://ai.google.dev/api/models)。分页只读取元数据，不调用模型。整次发现共用一个有界超时，最多 8 页、256 个唯一模型、每页 1 MiB、累计 4 MiB；越界、重复游标、无效模型资源或不完整目录整体失败，不将截断结果发布为成功。模型 ID 排序去重；Gemini 的 `models/` 资源前缀在发布前转换为原生模型 ID。

类型是不可变修订输入，迁移 061 为历史修订赋予原有 `openai_compatible` 含义。省略新字段的旧请求继续使用原幂等哈希。更换类型必须新建修订，不能修改待测试或已测试的原修订；复用已测试凭据要求供应商、类型、规范地址和端点证明全部一致。原生类型不接受“无需认证”。现有运营方 Secret 引用、固定 UID、密文封装、当前权限和新修订验证流程继续生效。

发布只接受当前已测试修订、实际发现的模型以及该类型支持的显式协议；不会依据模型名称猜测能力或价格。Console 提供类型选择、匹配的协议默认值及发布入口，原活动版本在失败修订之后保留。独立 AI Gateway 从受限只读 view 获取固定类型，不取得供应商配置写权限。

真实厂商账户、供应商配额、账单一致性、完整多模态和持久文件引用、credit/BYOK 等仍有单独功能边界。合成服务用于验证原生请求、目录、发布、权限和账本，不能代替这些外部验收。
