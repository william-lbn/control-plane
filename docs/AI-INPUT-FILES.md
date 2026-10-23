# 分支 AI 输入文件

0.77.0 增加分支自有文件和有界多模态输入。模型必须明确批准 `input_modalities` 中的 `pdf`、`audio` 或 `video`；旧模型保留原文本/图片能力。该声明是运营方批准的能力，不代表网关测试过商业模型的识别质量。

控制台在 AI Playground 中上传、下载、选择附加和删除文件。上传不会自动附加；切换分支、模型或协议会清空选择。待提交文件、原幂等键与重试状态只在组件内存中保存。提交响应丢失时，“重试原输入文件提交”复用原文件、保留时间和键，服务端返回原回执；推理请求仍遵守已有未知结果处理规则。

## 文件 API

控制 API：`/api/v1/projects/{project}/branches/{branch}/ai-gateway/files`。需要项目 Editor 权限，支持 Console 会话或已有范围的普通控制 API Key；Console 写操作保留 CSRF 检查。SDK 入口：`/ai/v1/{branch}/v1/files`，需要已有分支应用凭据和 `ai_gateway:invoke` 范围，继续检查当前项目/分支权限及即时撤销。

| 方法 | 路径后缀 | 行为 |
|---|---|---|
| POST | 空 | 上传，201；必须提供 8–128 字符 `Idempotency-Key` |
| GET | 空 | 当前有效文件元数据列表，最多 32 个 |
| GET | `/{inputFile}` | 当前有效文件元数据 |
| GET | `/{inputFile}/content` | 原字节下载，SHA-256 ETag、`no-store`、`nosniff`、attachment |
| DELETE | `/{inputFile}` | 删除密文并保留不可变元数据/原请求引用；必须提供幂等键 |

上传可使用 `multipart/form-data` 的 `file` 和 `purpose`（`user_data` 或图片专用 `vision`），也可提交带确切 MIME 的二进制正文及 `X-Neon-Filename`。文件名不得含路径或控制字符，最多 128 字符。`X-Neon-File-TTL` 为 60–604800 秒，默认 86400 秒；原键重试必须保留原参数。

单文件最多 1 MiB，分支最多 32 个有效文件、16 MiB 密文对应的原字节。支持 PNG/JPEG/GIF/WebP、PDF、WAV/MP3、MP4 的有界输入，并核对 MIME 与文件签名。这里只检查传输格式、大小和边界，不执行内容渲染或恶意文件扫描。文件以现有后端密钥环派生的独立 AES-GCM 密钥加密；认证附加数据绑定组织、项目、分支、创建者、文件名、类型、大小、SHA 和密钥版本。读取接口不返回密文或密钥。

文件 ID 格式为 `nif_` 加 24 位十六进制，仅可在其确切分支使用；父分支、子分支或同组织另一分支都不能借用。已有文件不随分支历史自动复制。删除或过期后读取及新请求引用返回不可用；后台每分钟有界清理过期密文，上传也先清理本分支过期文件。清理不依赖解密密钥或 AI 启用状态。删除前已准入的请求可以完成，其费用和引用历史保留。

## 原生引用和协议

| 协议 | 本地文件引用 | 转发给已批准上游 |
|---|---|---|
| Responses | `input_file.file_id`；`input_image.file_id` | PDF 的 `file_data`/filename；图片 data URI |
| Chat Completions | `file.file_id`；`image_url.url: neon-file:nif_…`；`input_audio.file_id` | PDF `file_data`；图片 data URI；WAV/MP3 `data`/`format` |
| Anthropic Messages | image/document 的 `source: {type: file, file_id: nif_…}` | base64 source，保留原生 media_type |
| Gemini | `fileData: {fileUri: neon-file:nif_…, mimeType: …}` | 原生 inlineData |

Chat 的 `input_audio.file_id`、Anthropic 的 `source.type=file` 和 Gemini 的 `neon-file:` 是本服务解析的本地扩展。它们不会原样发送给供应商。Responses 支持图片/PDF；Messages 支持图片/PDF；Chat 支持图片/PDF/WAV/MP3；Gemini 可按批准能力提交图片/PDF/音频/MP4。Embeddings 保持文本输入。

每次最多 8 个输入片段、解码后总计 1 MiB，原生请求正文最多 2 MiB，文本预算保持 16000 字节。直接内联媒体也执行严格 base64、MIME/签名和模型能力校验。远程文件 URL、供应商账户的 opaque file ID、conversation/previous-response 等其他持久对象引用继续拒绝。

网关先验证调用身份和分支授权，再读取文件及供应商目录。准入事务再次锁定并核对文件仍有效，将 request/file/project/branch 写入不可变引用表，随后按原组织额度、BYOK 和分支预算规则准入。删除竞争不能生成未授权的已计费请求。日志、用量和错误不记录上传字节；当前公开用量 API 不提供附件内容或附件列表。

协议字段依据官方 [OpenAI 文件输入](https://developers.openai.com/api/docs/guides/file-inputs)、[Anthropic PDF](https://platform.claude.com/docs/en/build-with-claude/pdf-support)、[Gemini 文档处理](https://ai.google.dev/gemini-api/docs/document-processing)的内联传输形式。网关限制和本地扩展以上述合同及严格 OpenAPI 为准，不承诺供应商账户文件管理、无限文件大小或商业模型认证。
