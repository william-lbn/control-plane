# 2026-10-18：对象上传触发 Functions

F03 基础闭环通过：原生事务 outbox、桶/前缀过滤、分支签名、持久回执、自动重试、显式重投递、原 SQL 去重和 Console。见[功能合同](FUNCTION-OBJECT-EVENTS.md)。当前九组有基础交付，其余 26 组仍有实现或独立验收缺口；性能、可靠性和 HA/DR 专项按用户要求后置，继续 F04 客户 stdout/stderr。

## 来源与部署

- API/Worker/Web **0.54.0**；migration **043** 已应用并冻结；Chart **0.9.46**；Helm **118 → 119 → 120 → 121**。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`；最终 API 快照 `f2ae662d046e095c6b01d57097280571b701f2d540183e96b6c60913e42ae9d3`，归档 `040bab4cb687dde9205768549d636c8944fa8c98a0da6ec81c1ab7bec1bcbaf3`。未提交本地源码由独立快照识别。
- API/Worker：`neon.local/control-api@sha256:841a35a82a94ceb01d2f23e49da1c72d435d48c41a459cbecd3faa3e5ccd4cd2`。
- Web：`neon.local/control-web@sha256:79a02660d38e4aaa9a18f1265edd0717dd4b66a2950edba8c69bfbc69cadcc0f`，独立快照 `c4086889956e12766502e81403a662d34010cd17ebab48756284d79c440586d5`。其他八组件沿已验证 provenance 复用；Guest 继续使用 `neon.local/control-functions-vm@sha256:26d35375e519f3258ffe848e18eb263cd485972e02bfc89ec3dbc111b1b2182d`，未重新下载或生成 Guest。
- 最终升级前控制库完整逻辑备份 `36064943dddc9bb8677e959a948d4ad144ee4ee5f543dc54f9ccf5afa1364a5b`；每个升级阶段均独立保全控制库。最终语义约束覆盖 24 个资源，9 个 neon PVC、828 个原 Secret UID、其他六个 release 保留。

先部署新版本并保持事件开关 false，确认新 API/Worker 就绪且旧派发器已退出，再独立开启开关。最终补丁只刷新 API/Worker，复用已验收 Web。API/Web 新镜像在一台构建，再内网导入三台，没有公共 registry 推送。

## 功能证据

原生对象发布与 HMAC outbox 在同一 PostgreSQL 事务，原目录锁保护脏标记和导出次序。标记使用 generation CAS，导出先提交确定性调用，再确认原生事件。目标函数、原操作者、过滤命中和版本在发布时固定；后续编辑不重路由已接收事件。伪造事件被保留为 rejected，复制到子分支的父签名不会派发。

普通对象发布、覆盖、COPY 和 multipart 完成接入同一发布钩子。自动尝试使用原逻辑 ID；完整 429/5xx 最多三次，未知结果保留而不自动推断未执行。手动重投递追加独立 delivery，保留原失败，继续使用原 logical invocation ID，要求明确确认、精确版本、持久请求键和当前权限。

私有证据根 `control-plane/production/evidence/20261018-object-function-events/`：

| 回执 | 实际结果 |
| --- | --- |
| `go-20261018092912` | 两个新增 Go 顶层测试和一个受影响定时触发器测试通过；真实且相互独立的元数据/原生 PG 数据库，提交/回滚、过滤、重复导出、实际 COPY 钩子、分支隔离、签名拒绝、三次尝试、权限、跨分支、重投递及原回执不变；compile/vet/OpenAPI 通过，无 race/性能专项 |
| `quality-a/b` | Linux 格式、TypeScript、Vite、62 个受保护套件入口准入和 API 索引通过；111 paths / 159 operations。入口检查不等于重跑 62 套旧 UI |
| `images-b` / `deploy-a/b` | 新版及分两阶段开关部署通过；早期 `images-a` 的合同 const 枚举尚未同步，未上线即更正 |
| `ui-a` | 七个真实 UI 检查全部通过，实际对象字节与原生 SQL 副作用、过滤、自动重试、伪造头拒绝、三次失败、明确重投递、202 丢失后的同键重放和正常清理 |
| `original-receipt-readonly-proof-c` | 两个逻辑对象、三个 delivery、六个独立请求账本引用，原失败/首个引用/路由快照不变；一次手动准入、触发器/函数已删除，原存储禁用状态、原 Writer 缩零和脏标记清除均核实 |
| `go-20261018094933` | 新增旧原生目录首次发布回滚场景：表创建随发布回滚时，导出可在验证原项目 schema 后补建空 outbox 并清除脏标记；同一受影响测试重新通过 |
| `images-c` / `deploy-c` | 仅 API/Worker 更新上述回滚补丁，Web/Guest/其他组件复用 |
| `runtime-source-match.json` | 442 个最终运行输入与本地源码一致；实际 UI 基于 images-b，之后唯一运行文件差异是 `storage_function_export.go`，由新增 Linux 原生 PG 回滚场景单独覆盖，未声称完整 UI 重跑 |
| `owned-terminal-jobs-retirement` | 两个质量 Job 和一个 UI Job 全部留档后按原 UID/RV 退休；四个 Go 一次性 Pod 在各自回执中已按 UID 归档退休 |
| `functional-postcheck.json` | 实际 0.54.0 / migration 043 / Helm 121，API/Worker/Web Ready、事件开关 true、服务的合同接受两类触发器，VM 零 |

真实 UI Job **publication-ui-20261018094743**；复用原项目/分支 `prj_6f3c51bd15b772e3` / `br_6f3c51bd15b772e3`，函数 **fnc_ef2db35bd618a07e / eventprobe54**，触发器 **ftr_8fcdce84e136426af4b1b2ce**。新桶 **ci-object-events-1792316866510** 的三份对象、原 SQL 业务行、全部事件/失败/请求历史保留，没有为清理而删除业务字节。

原失败 **fti_e32f068bd958e4ad666c07b83230ea836bc26f1a3d209d7cd109be30694fc670** 三次 503 后保留；手动 delivery **fti_3a92a64922f297986242859d9656dd84dd20cc7685b0c6eab971749516e31d87** 第一次成功，仍使用原逻辑 ID。业务去重在原 SQL 事务发生，自动两次和失败三次加手动一次分别只有一份 effect。重投递的 202 被测试主动丢弃，UI 保存原键/If-Match/body 并重放，控制面只保留一次手动准入。

初始开发失败均保留：测试夹具路由方法名、迁移约束自动命名和受影响定时 INSERT 的部分唯一索引推断先被一次性 PG 拦截，修正后通过，未对线上修改旧 migration。只读附加检查早期误把 `endpoints.state` 的 provisioned active 定义当成运行态；实际缩零保留定义并移除 VM。最终按原成功 suspend 回执和真实 VM 零核验，没有改写定义来迎合检查。

本片未做压力、性能、故障切换、分区、外部 fencing、HA/DR、无限队列或商业 SDK 全兼容验证。基础 8 MiB 对象限制仍在；F05 分支触发器继承、完整计费及物理保留清理仍是独立扩展。
