# 2026-10-17 当前交付：Functions UTC 定时触发器

## 1. 交付结论

**F02 分支内 UTC 定时触发器的本地基础闭环已通过。整体仍为预览产品，未达到
官网全部功能或生产准入。** 本页保留既有索引文件名；日期按 Asia/Shanghai。
上一个完整切片是[个人历史名称与 AI 命名](DELIVERY-2026-10-16-HISTORY-NAMES.md)。

| 范围 | 实际结果 | 边界 |
|---|---|---|
| API/模型/UI | 持久计划、UTC 五字段 Cron、启用/暂停/删除、分页调用记录 | 分支内；未实现继承、对象事件、任意时区 |
| 幂等/并发 | 已提交响应丢失后原 key/输入恢复同回执；旧 UI 412 | 数据库事务/租约不等于外部 fencing |
| 真正冷启动 | 同一 Function、同一计划，Function/Compute 均为 0；下一分钟唤醒后 HTTP200，真实受限 SQL 仅一条回执 | Compute 原策略 scale_to_zero=false；实验前/收尾通过 UI 手动归零，不声称这个 Compute 自动休眠 |
| 授权与追踪 | 普通请求伪造平台头为 403，无新增 SQL 行；schedule 请求账本对应原 invocation_id | 依赖可信 Guest TLS/HMAC；公网 TLS、完整 HA 另验 |
| 正常退休 | 正确示例调用后 Function 自动休眠；UI 删除计划/Function，Compute 正常归零 | 源版本、SQL、Secret、所有旧失败/成功回执保留；未物理 GC |

详细架构、接口/模型、错误和手工步骤见[定时触发器合同](FUNCTION-SCHEDULES.md)。
[需求账本](REMAINING-REQUIREMENTS-2026-10-16.md)仍有 29 组代码或独立验收缺口，
已验收基本组的扩展边界也没有自动关闭。下一阶段继续 C01 数据库/角色重命名与
后续 Functions、Storage、Auth、生命周期和分布式基础设施工作。

## 2. 精确运行版本与来源

- API/Worker **0.47.2**；Web **0.47.0**；migration **039**；独立 Chart **0.9.39**；Helm revision **111**。
- OpenAPI **107 paths / 155 operations**。
- 构建父提交 `ac1701ea66fbc3c3a4a80b8a2f3a29e64b05f686`，不是本次部署源码的完整标识。
- API 源快照 SHA-256 `0cea1f5e70513a386434fc300bbc91249371bd5e76de8d29d3e2302addf40fd1`。
- API/Worker `neon.local/control-api@sha256:759c763f1d8ad10f37746856898ea2a77b84823f2474e00708073128272dc99c`。
- Web `neon.local/control-web@sha256:a8525568a87e430a765d283ff63175c38368bc15da10a01fae99297d079f9d8d`，源快照 `77efe38c5a01dd7e8cedefd85737061ad72fcdb6305f9283ca85c34378d10e83`。
- Guest `neon.local/control-functions-vm@sha256:24e348fc125f644cfb00ba3335f8a291d3ce5a974e388b17495057ce94ed13c7`。
- Guest supervisor 源输入 SHA-256 `908d4306a07a42bed5a7fd70836860ed63e4b7dde09a6c8c60ab8c7b0241cb6d`，binary `ae4edc467d3a9d6e47d04ddc150e18b2c8081f4980e9b362ffba9d1143d6b1c7`。
- Guest 是 supervisor 派生版本；父 rootfs/Node/包/内核保留自 `docker.io/williamluckyli/control-functions-vm@sha256:c4b5d20b248e0d3d0951f2c3ae04b917092b58c5ba2f146047e99a3828fbbc32`，**不是完整 Guest rootfs 从最新源重建**。
- 元数据备份 SHA-256 `96551ec850331b257b2c9c2f93c48fb02d00007a4c1e4a199073e947098f326a`。

Linux 构建、三节点按 digest 串行导入、Helm 升级前备份；9 个 PVC 与既有 Secret
UID 保留，其他六个 release 的 manifest 未变。新不可变 Functions 配置仅替换
vm_image；原配置/根密钥/代理 CA 保留。API/合约与运行 Web 输入逐文件核对一致，
测试后续修复按 test-only override hash 留证。Neon Rust/Autoscaling 源码没有修改。
公开统一 Helm 0.1.31 仍锁 0.23.0 控制镜像，最新本地镜像尚未统一公开发行；
不能把本地 digest 当作外部可拉取发行。

## 3. 实际验收与原证据

1. Linux Go race：Cron/日期/输入/稳定 ID，可信 TLS/HMAC、平台头与能力协商。
2. 独立 PostgreSQL control_ci：当前权限、幂等/版本、唯一入队、有限重试、租约过期
   unknown、不可变终态、审计与单连接池场景；受影响 HTTP admission 检查通过。
3. 认证元数据库异常正确为 503，失效凭据仍为 401；artifact 元数据不可用为 503，
   确定的无效身份为 404。下载只对同一安全 GET 做三次新签名尝试，拒绝错误 CA、
   401/404、重定向、篡改和越界；取消/次数上限通过。Guest 启动有界，未降级 TLS。
4. 真实 UI 的业务部分在 `publication-ui-20261016165154` 通过：原 Function
   `fnc_29918034cfee3d14`，原计划 `ftr_9380a86340211ba5f9cd9726`，新调用
   `fti_e96c3f9834c2a5b93b93e393ed2500018e30dc2691c5ced181f095f12f6336ff`。
   实际 SQL 回执只有该计划/调用的一行；普通伪造请求 403，原 schedule 日志一条。
5. 该 Job 后因表中两条 succeeded 的测试选择器歧义失败，**原结果保留 failed**。
   修正为原调用 ID 定位后，以独立 finalize 套件在 `publication-ui-20261016165656` 完成 UI 观察、
   历史保留和正常退休；没有重新派发客户调用，没有重建定义/版本。三条原终态
   调用记录删除前后完全相同。两批证据共同组成闭环，不重标旧失败为成功。

未重复其他旧全量功能套件，也没有等待 GitHub CI。本批仅运行新增和受影响检查，
运行 Web 未变化时复用之前 Vite 构建结果；新增测试仍做格式/TypeScript/准入核对。

## 4. 失败、纠正与明确限制

- 早期冷启动只保留 artifact 阶段失败；失败后 404 是已降级身份的正常拒绝，不能
  证明原下载原因。修复加入安全错误分类与有界 GET 重试，原失败原样保留。
- 一次恢复期间三台 Kubernetes API 同时拒绝连接，原操作在 SQL 前置步骤失败；
  没有新 Function VM。之后只读 TLS1.3/hostname/CA、SQL provisioner 身份验证通过。
  日志有 RKE2 重启及共同 I/O 压力，内存/磁盘容量充足；不声称已证明唯一根因或
  完成硬件稳定性验收，没有重置 etcd/删除数据。当前功能在恢复后串行验证。
- 私有执行器的通用启动命令覆盖了示例打包命令，早期实际部署旧 PostgreSQL 示例，
  因而 transport HTTP200 没有要求的业务 SQL 回执。这是执行器缺陷；修正后通过 UI
  为同一个 Function 明确部署正确示例，保留所有旧版本。新增 ZIP entry 与封存源码
  的逐字节预检、部署 bundle digest 核对和实际执行命令回执，防止重现。
- 旧示例 Node 不就绪时自动回收条件不成立，已通过 UI 明确停止原 UID 后再部署。
  正确示例的健康调用后自动回收通过；**异常 Node 的有界自动回收仍需继续完善**，
  不用正常路径覆盖这个生命周期缺口。
- 分支继承、对象事件、stdout/stderr、WS、长期保留/GC、外部 fence、HA/DR、
  全链路可信 TLS 和统一公开发行仍按各自门槛推进。

所有 fixture、失败/成功、ZIP、源/OCI、业务数据及记录保留；只退休明确自有的终态
测试 Job/Pod/VM，保存 UID 和日志。完整维护者证据位于工作区
`control-plane/production/evidence/20261016-function-triggers`；口令、Cookie、DOM、
私密脚本和引导材料不上传公共 Git。公开产品依旧为 Go/PostgreSQL + React/TypeScript。

## 5. 本批收尾

15 个明确自有的终态质量/UI Job 已先归档日志和 UID，再按 UID/resourceVersion 退休；
临时 PostgreSQL 测试 Pod 也已正常退休。最终 3/3 节点 Ready、无 Memory/Disk/PID
Pressure，87 个工作负载与期望一致（包含有意为 0 的服务），错误 Pod 0、托管 VM 0。
55 个可信 live suite 的声明/准入对齐，新增测试格式/TypeScript 与公开源码门禁通过；
305 个 API/合约文件及 67 个 Web 运行输入与实际镜像源快照逐文件一致。
