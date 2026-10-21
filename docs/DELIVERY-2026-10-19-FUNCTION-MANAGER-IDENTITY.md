# 2026-10-19：Functions 原实例管理证书自动续期

原实例两小时叶证书到期后需要停醒的功能缺口已完成本地基础验收。后台通过原 UID/boot、TLS/HMAC 和旧指纹 CAS 更新证书，随后用新 TLS 握手核对精确指纹；函数代码、SQL 身份、部署与运行进程保持同一身份。T01 的全链路 TLS、根 CA 轮换、公网入口等仍未关闭，性能与可靠性专项按用户要求后置。

## 精确来源

- API/Worker/Web **0.57.0**，migration **046**，Chart **0.9.49**，Helm **127 → 128（关闭）→ 129（功能窗口）→ 130（默认窗口）**。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`；实际本地源码快照 `24140747961a46102c7544cedfd08538a7641f84a0daf21e9da03a7ed9a08a71`，归档 `b8c89f283e915679fce2f0bbbd79944f1c8510797debd3774c252515a12a4f54`。未将父提交宣称为部署源码，未推送公共 registry 或统一公开发行。
- API/Worker `neon.local/control-api@sha256:4eff6809afa43f90b2a4dceb61859b2aca18d3a3bf3cd7c4f5b84dd344797f1b`；Web `neon.local/control-web@sha256:f99d2e2a68bd2b9175c48b5e4f5e5e2cc7005d945421022a09bbfb8a0d17e378`。
- Guest `neon.local/control-functions-vm@sha256:b9c04590c1450b6dbb59c17b5c333949d1bee33908e38fd9645fee28b93f04b7`，supervisor `475db372b47624446080bc0eb85b1e58217c2d7f0b99298c0ec05a03967a826b`。使用 F04 原已验证压缩离线检查点，在新稀疏文件中只替换 supervisor，原系统/Node/依赖保留；未修改接受的原离线源。26 个 Guest 生产输入、320 个 API/Web/Chart 生产输入逐项匹配。
- Guest 基础/Node 外网下载 **0**；每个 peer 仅传输 **159,897,600 字节**缺失 OCI 内容，在三节点验证准确 digest/CRI。API/Web 也只在 Linux 构建一份后内网分发。代理继续仅用于外网，私有管理客户端明确禁用环境代理。
- migration 046 SHA `5774db082eeb8118f81f6de749d3ebd033d50b37d35b74ece5d567016d8aabe3`，已应用并冻结。045 的原 SHA 未改变；后续 schema 修复必须使用新迁移。
- 三阶段升级前完整元数据备份分别为 `51ff91a882d6b71c8d5fc746a5de0dbcf0b18f5b79bc99ccbbff8fe49c20e039`、`bbbedda5ac07b99ba64b4c11502c459d9383793ba5095bab4dcb82629726cb86`、`67372fce62954f0722596c246dc8610612b498a0d8fa97c78be323484544b9d1`。每次都保留 9 个 PVC、当时既有 Secret UID 和其他六个 release；精确 24 对象清单仅允许指定镜像、未来 Guest common 引用和两个管理策略环境变量变化。
- 新 immutable common Secret `neon-functions-renew-v057-20261019112146` / UID `c7ab2a8f-a004-451a-85eb-feadec077fa8`，仅 vm_image 更新；原 common Secret/UID、MAC/加密/签名根私钥均保留。

## 功能与边界

Guest root 维护原实例的内存 TLS 身份，接受同一固定 CA、SAN、scope、boot、旧指纹以及新的独立公钥。新叶限定有效期、必须延长原到期时间；未知提交结果观察原 boot，不盲重试、不创建替代实例。过期、错误 CA/SAN、旧指纹、错 boot、相同密钥、排空及超长有效期被拒绝。新连接使用当前叶，已存在调用不因切换而重启。私钥只存在受信 Worker、TLS/HMAC 管理消息和 Guest root 内存，不进入客户环境或证书观察表。

原 bootstrap 始终 immutable。控制面回读旧回执时验证签发窗口、原 MAC/UID 和完整 scope；Guest 新 boot 及当前管理连接仍检查当前有效期。避免换证后被原 bootstrap 的旧叶到期挡住，未引入过期 TLS 连接例外。元数据保存原 boot、VM/Service/bootstrap UID、指纹、有效期与认证观察历史，拒绝覆盖旧证书记录。

默认提前 **2700 秒**刷新两小时叶证书。实际 UI 为加速触发，临时使用有界 **7140 秒**窗口；完成后已恢复 2700 秒，没有改 VM 时钟或缩短叶有效期。旧 Guest 没有能力标志时不会被推送更新，正常停醒后使用新 Guest。Console 运行观察展示到期、指纹和当前实例续期次数；后台刷新不执行客户代码、不唤醒 SQL、不修改函数版本、公开策略或日志授权。

## 实际回执

私有证据在 `control-plane/production/evidence/20261019-function-manager-identity/`。

| 回执 | 结果 |
| --- | --- |
| `functional-20261019111500` | 三个新增 Root/TLS 顶层测试及九项拒绝子例；真实 TLS/HMAC、新握手、同 boot/child、丢失响应后原身份观察、旧原请求重放、到期边界和过期 bootstrap；两个受影响身份/观察测试、三个 OpenAPI 检查、vet、静态 supervisor 构建通过 |
| `go-20261019112059` | 两个定向实际一次性 PG/策略测试通过；历史、原 boot/VM/Service/bootstrap、版本/计数拒绝、已暂停意图拒绝、不可变旧记录和窗口边界；编译/vet/OpenAPI 通过 |
| `go-20261019114815` | 补充过期原 bootstrap 的控制面完整路径：原链/MAC/UID 可回读、当前 Guest 启动仍拒绝、内容篡改及替代 Secret UID 拒绝；不改变运行代码 |
| `quality-a` | Linux 格式/TypeScript/Vite/API 索引和 65 个受保护套件入口准入通过，115 paths / 164 operations；未重跑 65 套旧业务 UI |
| `guest-20261019112353` | 离线新磁盘读回、运行文件校验、e2fsck/qcow2 校验、稀疏封装和三节点缺失层导入通过；失败的首次工具输入缺失记录保留 |
| `deploy-disabled` / `deploy-enabled-functional-window` / `deploy-default-window` | 先退出全部旧 dispatcher 再启用，最后只恢复默认窗口；镜像/common 引用及其他环境值精确受控，数据身份保留 |
| `ui-a` | 真实 Console → 原保留 SQL 分支 → 新受限 Function；实际 SQL 换证前后均成功，原 Function/DID/instance/version/generation 不变，自动换证 0→1，新到期与指纹可见；原函数正常停止、删除与 Writer 缩零，共三项检查通过 |
| `original-identity-readonly-proof` | 同一 boot/VM/Service/bootstrap、两条证书历史、原 bootstrap 证书/UID/MAC 未改、环境及 SQL UID/代次保留；一个完成部署及五个原操作保留并成功、原时间线 Ready、原保留函数仍为一个、三节点 Ready、VM 零，Helm 130 |
| `runtime-source-match.json` / `owned-terminal-jobs-retirement` | 实际生产输入逐项一致，UI 没有 test override；五个自有一次性 Go Pod 与两个终态 Job 全部按 UID 留档退休，未删除业务数据或 Secret |

原函数 **fnc_5b7ea05a337b1644**、部署 **fdp_24331e84eea645f7**、实例 **fni_1486f219b0f38502**。旧指纹 `01404a11e2ba33a0d3a598d2000e4f4cad0c65a982f2eaa510b76cb549f53724`、旧到期 13:48:03 UTC；新指纹 `14d25fca8f57f31ce2ebf8fb5e73f43de083d6e3be0dd2ed61b5903b8a638ea2`、新到期 13:49:37 UTC，定义/执行代次均为 2。实际 SQL 返回同一受限角色及保留的 `parent-after-fork` 数据。

到期边界使用隔离真实 TLS 客户端的校验时钟越过旧叶期限，并独立构造已过期且原 MAC/UID 完整的回执；现场 UI 使用加速的提前窗口。没有把两分钟原生测试声称为持续两小时或长期可靠性运行。早期失败均为测试/构建夹具：不存在的 ready_at 字段、未做 Kubernetes JSON 归一化的嵌套映射、首次未复制打包工具输入。修正后新 attempt 通过，原失败与离线载荷保留。CI 同一 shell 的 ZIP 路径修正和后补控制面测试分别按非运行输入来源记录。

本片完成短期管理证书续期功能，不关闭整个 T01 或全项目。后续继续业务功能；根 CA 轮换、全链路 TLS 与性能/可靠性专项仍按独立边界推进。
