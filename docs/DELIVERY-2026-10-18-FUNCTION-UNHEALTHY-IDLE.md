# 2026-10-18：Functions 客户进程异常后的空闲回收

L01 中的客户 Node 进程异常回收切片已经通过：认证 supervisor 仍可观察、无活动调用且满足已确认的空闲策略时，客户进程不就绪不再永久阻止实例退休。Keyset 扫描也避免首批不可观察函数饿死后面的候选。合同与手动验收见 [Functions 异常进程回收](FUNCTION-UNHEALTHY-IDLE.md)。

这一步不关闭整个 L01：失败创建依赖清理、分支 TTL、Kubernetes 节点分区和外部 fencing 仍须分别完成。产品继续为预览版本；既有七组基础回执保留，其余 28 组仍有实现或独立验收缺口。

## 精确交付

- API/Worker、Web **0.49.0**；migration **039**；Chart **0.9.41**（appVersion `0.49.0-local-unhealthy-idle-preview`）；Helm **112 → 113**。
- 构建父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`，不代表未提交源码的完整身份。
- 不可变源码 fingerprint `4f0f0b77c2737594e55fab9cd65f25bd6a970060af855cefdfeace999c244db6`；源码归档 SHA-256 `880e9ccbf3628087b724a8fa9fa340a4706f336d349568e1b08a34ea56553f4c`。
- API/Worker：`neon.local/control-api@sha256:553386ffb0f8c6222c94f3ce047dbcbdadbce32bcb1e1c97a80a9e86aaa8b71e`。
- Web：`neon.local/control-web@sha256:95495de2d6854bd39cf4075b6a21a417d0a8fcdb50010721aedf65f3005a10aa`。
- Guest 保持原 `neon.local/control-functions-vm@sha256:24e348fc125f644cfb00ba3335f8a291d3ce5a974e388b17495057ce94ed13c7`；其余组件复用各自原来源和 digest，Neon/Autoscaling 源码未改。
- 升级前完整控制库逻辑备份 SHA-256 `531cbf881f3666ce8952e7ca32f1d8472782b085c34729e653b5a9e3db1c53fb`。

Linux 构建并导入三节点，未推送公共 registry。Helm 变更前对 24 个对象作语义比较，仅允许 API/Worker/Web 镜像和 Chart 标签变化；9 个 neon 命名空间 PVC、800 个既有 Secret 身份保留，其他六个 release manifest 不变。验收后独立核对全集群 106 个控制器 UID/副本与预期模板、10 个 PVC 身份/绑定，76 个活动 Pod 全部 Ready，VM 为零，实际 OpenAPI 0.49.0。

`runtime-source-match.json` 核对 399 个 API/合同/Web/Chart 输入与构建快照一致。随后文档和镜像导入工具的修改单独留证，未把未重建工具描述为已进入 API/Web 镜像。

## 验证与证据

维护者私有证据位于 `control-plane/production/evidence/20261017-functions-unhealthy-idle/`；密码、私有 checkpoint、OCI 和原日志不进入公共 Git。

| 回执 | 结果与范围 |
| --- | --- |
| `go-20261016175023` / `go-evidence-reuse-after-recovery.json` | 两个新增 Go/PG 测试、race/vet/OpenAPI 通过；314 个 API/合同文件按相同 gofmt 规范核对一致，恢复后不重跑这些已通过检查 |
| `quality-after-recovery` | Linux 格式、TypeScript、Vite、57 个 live 套件准入检查及 109 paths / 157 operations 索引通过；未执行旧业务 UI 套件 |
| `images-after-recovery` | 精确不可变快照构建 API/Web，三节点 digest 导入通过 |
| `named-import-after-recovery` | 初始检查失败：native 导入已成功，但 CRI 内存仍缓存旧合成别名；失败回执保留 |
| `named-import-after-runtime-refresh` | 清理已核对的合成别名并串行刷新运行时后，三台 API/Web CRI 引用均准确；没有重建或重新外网下载镜像 |
| `deploy-after-complete-recovery` | Helm 变更约束、备份、原资源身份及其他 release 保留通过 |
| `ui-after-complete-recovery` | 原 fixture 的七项新 UI 检查首次通过，真实 Linux 浏览器执行约 2.3 分钟 |
| `retirement-after-complete-recovery` | 两个终态自有 Job 先保留定义/日志，再按 UID/resourceVersion 退休；未删数据或 Secret |
| `platform-after-acceptance-b` | 全集群控制器/PVC/活动 Pod/实际版本与 VM 零核对通过；前次只读审计因 `.101` 未安装 Helm CLI 中止，原日志保留，改为读取 Kubernetes release 版本元数据 |

原 UI Job `publication-ui-20261017172748`，沿用项目 `prj_6f3c51bd15b772e3`、分支 `br_6f3c51bd15b772e3`、Writer `ep_6f3c51bd15b772e3`，只新增本测试函数 `fnc_cda889901f723571` / `idleprobev1`。

七项检查依次为：创建自有定义；部署 sealed bundle 并记录健康原实例；UI 明确开启 public/idle 策略；一次正常响应后客户 Node 退出，原实例认证观察为未就绪且活动调用为零；唯一原自动停止 Operation 完成且 UI 转为 metadata-only；一次公开请求使同一定义、bundle 和环境冷醒到新实例；正常停止/删除测试函数并让 Writer 归零。原自动停止 Operation 为 `op_ff352cfbfc4eb903`；完整六个 Operation 和七个 checkpoint 在私有回执中保留。最终没有受管 Compute Pod 或 VM。

## 环境与后续

关机后的原文件系统、etcd、containerd 和 CNI 恢复先独立完成，再部署本候选。三台 Safekeeper 与原数据面恢复；四个冷启动镜像在三台均完整解包，采用已有缓存和内网传输，没有为本次业务验收重复拉取外网镜像。原缓存和故障证据保留。

宿主机仍为单块共享 SSD，累计 NVMe MediaErrors 为 18；软件恢复和本次 UI 通过不代表介质错误已修复，也不关闭长期负载、独立故障域 HA/DR、外部 fencing 或生产准入。继续实现失败创建资源的受控清理和分支 TTL，再逐项处理需求账本；历史通过不自动成为新切片的验收。
