# 2026-10-18：Functions WebSocket 基础闭环

公开 Functions 入口、认证 Guest 管理器、Node 运行时及 Console 连接调试已贯通标准握手、文本/二进制、子协议、双向关闭、活动计数、空闲退休和保留代码冷醒。始终验收同一个原生 Function，没有用新函数替代失败现场。基础 F01 已通过，[合同](FUNCTION-WEBSOCKETS.md)列出 SDK 和运行限制；完整产品仍有 27 组代码或独立验收缺口。

## 运行来源

- API/Worker、Web **0.53.0**；migration **042** 保持不变；Chart **0.9.45**；Helm **116 → 117 → 118**。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`；最终 API/Web 快照 `217f12e9039b569ce6300e6ee205842107dac17e05cf331971d75e8f5c44dd79`，源码归档 SHA-256 `1be417a39c9b42b8e8417aa7b6cc489d2f77ff38f06a4444fa3b14dd94b272c2`。未提交源码以独立快照识别。
- API/Worker：`neon.local/control-api@sha256:910d5d44c10105bcc15df17a9c576f5da7859c1a8568df7c59a2325bdbacd848`。
- Web：`neon.local/control-web@sha256:cf12d2e8d34ecada3c99e25db8e9a75759b91fbaebac324c750ace45190e7c5c`。其余八组件沿 0.52 独立 provenance 复用。
- 新 Guest：`neon.local/control-functions-vm@sha256:26d35375e519f3258ffe848e18eb263cd485972e02bfc89ec3dbc111b1b2182d`；supervisor SHA-256 `7d920a230262d35cb29ed65106d5c6219b3e655491d36ed67e1b318ae71cf932`；Guest 源归档 `5507185ff6331e54088727fe1e1d6181b752394bdcead6ca1319351c7a8c08f4`。
- Guest 从 `neon.local/control-functions-vm@sha256:24e348fc125f644cfb00ba3335f8a291d3ce5a974e388b17495057ce94ed13c7` 的已校验本地内容派生，仅替换 supervisor、运行时代码和锁定的 `ws` 8.22.0；原 Node 二进制、内核和系统包保留，新增文件逐个从新离线磁盘读回校验，再检查 ext4/qcow2。不是从上游最新 rootfs 全量重建。
- 最终控制库逻辑备份 SHA-256 `b8b336494d193eeacdb8dde33e7e3de3e6b14a7f2b60594685e9aa06d65c8078`。初次 Guest 配置切换前另有完整备份，见 `deploy-a`。

Guest 基础 13 个内容块本地复用，外网新增只有固定版本的小型 JS 依赖。三台均已导入；另外两台各仅通过内网接收 **159,897,600 字节**缺失 OCI 内容，未重复传输已有基础层。API/Web 也通过内网导入三台，未推送公共 registry。

原 common Secret `neon-functions-artifact-v0472-20261016133058` 及其 UID 保留。新不可变 `neon-functions-websocket-v053-20261018050616` 只改变未来实例的 Guest 镜像，未轮换原密钥。切换前确认所有旧 Function 实例正常退休。首次 24 对象部署约束允许三个镜像及此配置引用；CSP 补丁只允许三个镜像、确切 Functions 连接来源及其 Web checksum。最终 9 个 neon PVC、820 个既有 Secret UID 和另外六个 release 保留。

## 功能与验证

新握手先验证后冷醒；以原 Function/instance/boot/generation、TLS 和 HMAC 认证到固定 loopback Node。Guest capability 显式区分旧版本。控制层流式转发帧，Node `ws` 验证掩码/UTF-8/大小和子协议；无压缩，无客户消息重放。打开连接由受信任 supervisor 计数，沿既有共享准入锁保留实例。应用可拒绝握手，平台不记录消息、URL 查询凭据或客户请求体。

Console CSP 只增加运营配置中确切的 HTTP(S) Functions origin 及其 WS(S) 对应值；脚本、样式和其他来源限制保留，非法 authority 在渲染前拒绝。独立 Go Console 中间件采用同样的受限策略。

私有证据根 `control-plane/production/evidence/20261018-functions-websocket/`：

| 回执 | 实际结果 |
| --- | --- |
| `functional-20261018050534` | Go 两个真实 TLS/HMAC/原帧通道测试及 public 握手前置测试通过；Linux Node 12 项通过，其中三项新增 WebSocket、九项受影响 Fetch/SSE；无跳过，无 race/性能专项 |
| `functional-20261018051950` | 增加确切 CSP 来源及非法来源负例；四个顶层 Go 功能检查、vet、supervisor 编译通过，未变的 Node 层复用 BuildKit 检查缓存 |
| `web-quality-a` / `web-quality-c` | TypeScript、Vite、格式和 61 套入口准入通过；110 paths / 158 operations。最终 `c` 核对修正后的原资源续验测试，未执行 61 套旧 UI |
| `guest-20261018050616` / `images-b` | 精确独立 Guest 与 API/Web 构建及三节点导入通过 |
| `deploy-a` / `deploy-d` | common 配置、CSP 语义约束、逻辑备份、实际部署通过 |
| `ui-b` | 原 Function 创建/部署通过；浏览器 CSP 拒绝独立 origin 的连接，原现场保留 |
| `ui-c` | 同一 Function 续验，实际消息、子协议、应用拒绝、34 秒活动连接、正常关闭与自动退休通过；真实冷醒也已发生，测试误以为冷醒保持原 deployment ID 而停止 |
| `original-coldwake-readonly-proof-b` | 只读核对原/冷醒 instance、两个已完成部署、同一代码/不可变 artifact、空自定义环境，以及原自动 `resume:` 幂等意图成功；没有更改状态或业务数据 |
| `ui-d` | 修正验收断言后从同一原检查点继续，比较保留代码/环境并验证新实例、服务端 4001 关闭、101 请求记录、正常停止/删除及原 Writer 缩零通过；没有重复已通过的消息/空闲阶段 |
| `runtime-source-match.json` | 435 个运行输入与实际镜像快照一致；六个 Guest 自有 runtime/包锁输入逐字节匹配；唯一 UI 测试修正 SHA-256 `df163b75e2845b23f5b3bae355683ac48fbf62b9dde02e4850e1958a303831d2` 独立披露，没有重建运行时 |
| `owned-terminal-jobs-retirement` | 七个终态自有 Job 完整留档后按 UID/RV 退休 |
| `functional-postcheck.json` | 实际 0.53.0 / Helm 118，三个业务 Deployment Ready、VM 零、浏览器 CSP 仅许可确切配置来源 |

Function 始终为 **`fnc_b6dab10c860073d2` / `wsprobe18`**，位于原项目/分支 `prj_6f3c51bd15b772e3` / `br_6f3c51bd15b772e3`。创建 `op_37f873c5df7d9d3c`，部署 `op_677daff9e0406b28`。原续验基线 instance `fni_d463a7626a1e28dc` / deployment `fdp_1d2af0040813cd89` 正常退休，冷醒为 `fni_8b1c21747d25e790` / `fdp_f3a159123a976570`；正常 resume 创建新部署回执，`source_deployment_id` 只用于显式历史回滚，不能强求与旧 ID 相同或伪造直接父指针。

最终 UI Job `publication-ui-20261018053721`；此前 `ui-b` / `ui-c` 为 `publication-ui-20261018051432` / `publication-ui-20261018052821`。七个检查点来自同一资源的完整链：`b` 创建部署一项、`c` 新执行四项、`d` 新执行最后两项；不称为七项全部重新运行。最终 Function 停止/删除 `op_f9e901893d11fb83` / `op_9bc1645ec840bc2c`，原 Writer 缩零 `op_679ffd475d4d075e8c21a917`。

开发失败同样留证：初始 Go 客户端超时包装丢失双向 body，改用受 context 约束的固定 transport；Node paused socket 补齐 resume；新 Blob 发送沿库原生队列保序、事件保留 invocation context。早期 Guest 工具把 image config 当 descriptor、一次 CRI 查询漏显式 image endpoint，均在最终候选前修正。`ui-a` 缺少二次归档的例子文件，在客户资源创建前停止。`deploy-b/c` 分别在语义 guard 的资源名和更早的既有 authority 校验匹配处停止，均未执行 Helm 升级。第一次只读冷醒证明也因相同的错误父回执假设而拒绝，最终 `proof-b` 按真实 resume 合同通过。

性能、可靠性、HA/DR、跨实例外部 fencing 和完整计费后置。该自托管 helper 不声称商业 SDK/Hono 专用适配完全兼容；连接最长 15 分钟、单消息 1 MiB、实例并发 16。WS 请求账本的字节字段为双向转发帧总量，未作为收费计量。环境自定义值的非空冷醒保留沿用此前 HTTP 的独立回执，本次 WS 示例环境为空。继续对象上传触发 Functions。
