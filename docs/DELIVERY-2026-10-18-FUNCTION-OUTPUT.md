# 2026-10-18：Functions 客户 stdout/stderr

F04 本地基础闭环已通过：明确授权、已知值替换、应用与平台 scope、实例/部署/boot 归属、密文存储、有界分页和保留、停止尾日志及真实 Console。见[功能合同](FUNCTION-OUTPUT.md)。当前十一组有基础回执，其余 24 组仍有实现或独立验收缺口，继续 F05 分支复制与恢复；性能、可靠性、HA/DR 与外部 fencing 专项按用户要求后置。

## 实际来源

- API/Worker/Web **0.55.0**，migration **044** 已应用并冻结，Chart **0.9.47**，Helm **121 → 122 → 123 → 124**。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`。最终 API 本地源码快照 `fca71f6942ad0929df8ab1a058a972042b0ec8f9869553d079d9de5856bbb275`，归档 `08552db6ff7105a2d2224e4387affbd38669278e9c7cb36fed261d26bd06f209`；本地变更尚未统一公开发行。
- API/Worker `neon.local/control-api@sha256:9d982cb49470291e06b15b99e71a63a968482e4279f45ffc391ab42726a816c3`；Web `neon.local/control-web@sha256:6e6c0cbd5c27257f7b2ac0a537b741b8f0ab8f6106756ea16674b9f676a28558`，沿用独立已构建快照 `26f1a3d213e730d6900c38c6bc73bbcf4703ffed7ddc39199da4653c9c3d9e71`，所有 Web 运行输入逐文件匹配。
- 新 Guest `neon.local/control-functions-vm@sha256:4cc946dd4782d1ba95bb1837d47d5385da2a4c970c2601e86e8b8aae6e25ecd4`，直接复用原已接受 Guest 的 Node、内核和固定依赖，仅更新 supervisor/runtime。Guest 测试归档 `7a281f3d54ee7655b8ce19311b717ec0e6b91623fe7ca9a08ea6deb09f63afc2`，实际 supervisor `9405c9f7ddd02cec529455c826aacb8726842b7acaa905d6e22d6ea892950ff7`；Linux 编译时 gofmt 后返回的准确输入与 JS 源码另行核对，没有把原上传格式误称为实际编译格式。
- 基础层外网下载 **0**，缺失层经内网分发并在三节点核对 digest/CRI。其余八个组件复用原接受来源。公共 registry 未推送。
- 升级前完整控制库逻辑备份 `2d9059571b51b42c1990da603ad3c0d41849eccfc90c42ffd4c5e99550becaad`。清单约束允许 API/Worker/Web 镜像、未来 Guest common Secret 引用和明确的输出开关；保留 9 个 PVC、835 个原 Secret UID 及其他 6 个 release。原 immutable common Secret 保留，仅新配置的 vm_image 改变，签名/加密密钥未轮换。

## 功能证据

私有证据在 `control-plane/production/evidence/20261018-function-output/`。

| 回执 | 范围与结果 |
| --- | --- |
| `functional-20261018111600` | 三个新增 Root 输出/认证/最终停止尾页测试与两个受影响 WebSocket 测试通过，真实 TLS/HMAC、boot 拒绝、级别/秘密替换/长度/缓冲/未换行；Node console 定向测试、vet、三个 OpenAPI 检查及静态 supervisor 构建通过 |
| `go-20261018112623` | 新控制面真实一次性 PG 顶层测试通过：默认关闭、必填 enabled、viewer/跨分支拒绝、确认/版本/持久同键重放、授权窗口、AES-GCM 密文/篡改拒绝、筛选/分页、过期不可见、停采禁止写入及后台采集与最终尾页重叠合并；编译/vet/OpenAPI 通过，测试 Pod 留档后按 UID 退休 |
| `quality-a` | Linux 格式、TypeScript、Vite、API 索引和 63 个受保护套件入口准入通过；113 paths / 162 operations。没有重跑 63 套历史 UI |
| `functional-20261018111600` / `guest-20261018112802` | 复用原离线检查点，替换新 Root 后回读比对；稀疏 gzip helper 的独立 Linux 内容/CRC/边界/拒绝覆盖测试复用通过记录；文件系统和 qcow2 检查通过，完整新 Guest 三节点导入通过 |
| `images-c` / `deploy-a` | 最终 API 与新 Guest 首次部署通过，控制库先备份、原身份保留、精确清单检查通过 |
| `quality-b/c` / `images-d` / `deploy-b` | 实际 UI 揭示日志/WebSocket 同 key 导致重复面板，修复为各自唯一前缀，新增刷新后只有一个面板检查；仅重建并更新 Web，API/Worker/Guest/config 逐项保持原值 |
| `ui-a` + `ui-d` + `ui-f` | 同一原函数累计六个唯一真实 UI 检查通过，首次创建/部署检查点复用，其余在修复后继续：原定义/部署、默认关闭及丢失 200 后同键/版本/正文/原确认时间重放、真实四级输出/原始流/替换/整行超长占位/伪造平台 scope 拒绝、筛选、受控迟到响应不覆盖新级别及实际 JSON 下载、明确跟随和停采区间排除、同次停止响应最终 SIGINT 输出及退休历史、原 Function/Writer 正常退休 |
| `original-output-readonly-proof` | 56 条保留密文，单一恢复后实例/部署/boot（原 Function 不变，两个原生命周期实例均退休），复合归属、序号唯一、四级/三流/两 scope、三天期限和四次策略准入全部核对；所有 emitted_at 都位于明确授权窗口，原操作均成功，VM 零；没有导出客户明文 |
| `quality-d` / `images-e` / `deploy-c` | 修复旧未筛选响应覆盖新筛选，按请求序号与当前级别拒绝过期响应；Linux 质量检查和 Web-only 精确部署通过，API/Worker/Guest 不变 |
| `runtime-source-match.json` | 456 个最终运行输入匹配，实际 UI 使用最终 API，Web 和 Guest 分别按其准确来源匹配；未声称旧候选 API 获得新验收 |
| `format-e` | 最终有界保护测试的 Linux Prettier 检查通过，没有重跑 TS/Vite 或旧 UI；测试源码与已接受 override SHA 保持一致 |
| `owned-terminal-jobs-retirement-final` / `owned-interrupted-ui-e-retirement` | 最后一个格式 Job 以及已到期且原 Pod 不存在的 ui-e Job 分别按确切 UID/RV 留档退休；本片共 11 个自有 Job 均已退休 |
| `owned-terminal-jobs-retirement` / `functional-postcheck.json` | 9 个本批终态自有 Job 完整归档后按原 UID/RV 退休；实际 0.55.0、migration 044、Helm 124，API/Worker/Web Ready、VM 零 |

首次 UI Job `publication-ui-20261018115537` 保留失败与原创建/部署回执；ui-b 保留两小时证书过期导致的 503；正常停止后唤醒保留源的继续 UI Job **publication-ui-20261018150126**；沿用原项目/分支 **prj_6f3c51bd15b772e3 / br_6f3c51bd15b772e3**，原函数 **fnc_4b05074ff1f722ac / logprobe55**。新演示代码只使用人工测试秘密，没有采集原业务秘密或清空原数据。删除定义后旧 slug 不再提供旧输出；保留密文按既定三天窗口过期。

原实例 `fni_bd96742a6274a101` 的证书于 2026-10-18 13:55:57 UTC 过期，只读证据核对后通过正常 UID 退休与保留版本唤醒恢复同一 Function；原部署/实例历史保留。新部署仍使用相同 bundle/环境名称，正常 resume 的 source_deployment_id 为空。没有放宽证书有效期或跳过 TLS 校验；原实例的短期管理证书自动续期仍列入 T01 功能缺口，不由本片或后置性能专项代替。ui-d 的保护测试恢复步骤以明确 SHA 的 test-only override 运行，产品代码为 images-d；最终 images-e 包含该测试及受控交错响应检查，ui-e 的受控延迟响应夹具等待被精确 UID/RV 的自有 Job 截止时间限定，证据保留；ui-f 将夹具等待改为有界并记录阶段，使用明确 SHA 的 test-only override，运行代码仍为 images-e。

正常停止先采集有界前缀，再通过同一次认证 shutdown 响应保存最终尾页，避免 root 进程退出后再连接导致尾日志丢失。事务内合并并发采集已提交的前缀，不要求已退出 Guest 再次发送。秘密替换只覆盖已知环境变量值、注入数据库 URL/密码及常见 URI 编码，是尽力的已知值匹配；任意、派生、分段秘密仍可能无法识别，因此中央采集默认关闭并要求明确确认。

UI 首次失败定位到相邻 React 组件 key 冲突，修复产品代码后沿用原 Function/部署/实例继续，没有重建替换测试身份。早期构建失败及硬件中断检查点全部保留：旧 Runner 无 Bash，封装改用静态 Go 稀疏 gzip 工具；新 Root 停止协议替代旧候选时保留原压缩载荷，只重新修改本次新离线磁盘。宿主机 NVMe 重置的实际恢复与自有构建隔离记录在环境报告；不把构建完成解释为硬件可靠性验收。未重复执行已接受的全套集群/存储审计或旧 UI。

缓冲为 512 行、单行 4096 字节、每页最多 16 条且序列化有界，中央每函数最多 10,000 行、三天。缓冲溢出有明确掉行计数；突然退出前未采集输出、尚未发布的失败启动、完整外部日志平台和商业计量仍独立，不以本片代替性能、压力、可靠性、HA/DR 或外部尾流验收。
