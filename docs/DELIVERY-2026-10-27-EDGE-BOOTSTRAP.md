# 2026-10-27 独立 Edge 部署与传输验收

Core 已由 193 升级至 194，再升级至 195；运行 schema 为 073，Native Storage 仍为 7。API/Worker 使用 `b45365f3281e76c755b530861b1fd9eca582cb40` 的已验证 Go 源码及镜像，新增独立 `control-function-edge` 进程。Storage 公开入口仍由原 API 提供，独立 Storage 网关的实机接管尚未完成。

此前的[源码增量报告](DELIVERY-2026-10-27-FUNCTIONAL-SLICES.md)记录部署前状态。现在 070–073 已首次应用并冻结；以后继续恢复、GC 或计量开发必须新增迁移，不能改写这四份已应用文件。069 和原生格式 7 保持原样。

API/Worker、独立 Storage 网关和新 Guest supervisor 均复用 Linux 缓存构建；新 Guest 仅替换 supervisor，保留原 Node、WS、内核和系统包，文件系统及 qcow2 检查通过。镜像在一个节点生成，经内网导入其余节点；Guest 只传缺少层。每个节点均核验精确 digest/CRI 身份，临时原始盘和全部传输包已删除。旧 Guest 和业务恢复材料保留。

| 项目 | 本次实机结果 |
|---|---|
| 业务保全 | Core 194 保留原 1099 个 Secret UID、9 PVC 和其他六套发行；Edge 切换保留当时全部 1100 个旧 Secret UID、9 PVC，另外新增三个不可变输入 |
| 独立进程 | 公共 Service 指向独立 Edge，原 NodePort 保留；进程无 ServiceAccount token，只挂载专用客户端叶身份和公开 Guest CA |
| 私有 broker | 专用客户端证书可以到达准入校验；缺少客户端证书得到精确 `TLSV13_ALERT_CERTIFICATE_REQUIRED`，不能用其他 TLS 错误冒充通过 |
| 作用域拒绝 | 规范地址下合法函数名与不存在的分支被拒绝，没有派发或唤醒 Guest |
| 验证夹具 | 新增 A/B 的 HTTP、显式释放 SSE、文本/二进制 WS 和轻量资源采样夹具；3 组 Linux 自检通过；端口声明修正后仅复查 1 组受影响 Helm，通过 |

环境原有 Functions 规范地址为 `.101:30892`，Storage 为 `.100:30893`，均保持原值和显式实验室 HTTP 配置。初次传输探针误用 `.100`，正确触发 authority 拒绝；另一个探针使用了非法函数名，在入口本地返回 404。这两次未作为传输通过回执。改用实际地址与合法函数名后，结合直接 mTLS 正反例完成验收。API 模板同时移除了 Edge 模式下已关闭的旧 Functions container port 声明；原 API/Guest binary 不因此重复构建。

新 CA 私钥保留在 Linux 运维私有边界；Pod 只有各自叶私钥，未复用 Guest 签发根。原公共地址仍是已有实验室 HTTP，不能据此关闭 T01 的全链路 TLS 工作。

以上完成的是部署、传输和隔离验收。原 C02 专用 Function 的发布、环境及公开策略已保存，真实 A → B → 回滚 A、可信资源计量和原代码恢复继续逐步验收；本报告没有将尚未完成的客户执行检查写成通过。F06 及其他四个业务组、O01/T01/O02 仍开放，基础计数仍为 22 组。性能、可靠性、压力、HA/DR 和完整大对象专项后置。

`b45365f` 的 [GitHub 运行 40367868454](https://github.com/william-lbn/control-plane/actions/runs/40367868454) 已完成，15 项检查和镜像发行全部成功；不再重复轮询该运行。结构化范围见[回执](acceptance/2026-10-27-edge-bootstrap.json)。
