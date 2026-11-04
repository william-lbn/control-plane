# Managed SQL 生产路由接线阶段说明（2026-11-04）

状态：**组件回归及正式 Adapter/CLI 构建已实际通过；尚未部署本轮产物、发布生产路由或取得 SQL 验收结果。**
本文只覆盖当前 PG17 数据面所需的最小生产接线，不代表最新完整控制面或官网功能验收。
Root 负责最终提交、单槽调度、真实路由发布及集群变更；本文所记组件验证未执行发布或集群变更。

## 生产入口与身份来源

新增正式 `control-managed-route` CLI 和 Driver 初始化后的自动入口，两者调用同一个 managed
publisher。CLI 输入是 versioned Binding 与固定的 registry/management Service 身份；
不接收任意目标 URL、历史成功标记、预制 Native 成功事实或额外认证能力。
Branch 为空时，在任何 Kube 读取前拒绝。当前 PG17 阶段可引用已授权且沿原冻结 Spec 保持的
Branch 身份，仍须与 fresh immutable Config 的同一字段一致，不能造常数或冒充最新控制面
业务数据库已经部署并验收。

publisher 每轮先读取同一真实 sealed VM、Config、凭据来源及 management Service，再通过原
Native 初始化只读通道取得 fresh 响应，核 tenant/timeline、startup、operation 与实际 instance。
HTTP 503、无效响应、缺失 Branch、UID/内容漂移均不发布。持久化旧 fact 不能代替 fresh Native。
CAS 前再核 VM/Config UID 与内容来源，registry 写回使用原 UID/resourceVersion；只对明确 409
重读全轮，不自动重试未知 PUT 结果。

同 endpoint route 刷新保留完整 RawMap 中的有效策略，包括显式空 CIDR、未知网络策略字段、
原 pageserver node ID 和 state，不能借刷新扩大访问范围或丢策略。首次发布省略原 Adapter
已定义默认的 allowed_ips/pageserver_node_id，不自报未核实的节点常数。其它 route 保持原样。

Adapter managed 分支仅进行实际 VM、immutable Config、SQL Service 的只读核验，不读取
credential Secret，不授予冷创建或配置 POST 能力。Legacy guard 和旧冷创建路径保持原合同。

## canonical Data 旧 guard 根因

v4.1 实际失败定位在 Services 与 Native 初始化 observer 的旧 `len(Data)==1`/只比 config.json
判断。原 canonical renderer 已生成完整多文档 Data；因此合法原创建产物在进入 Service
创建或 Native GET 前即被旧判断拒绝。这不是 Native 或实际 PG SQL 已失败的证明。

v5 两入口统一复用原 `renderDSGABootstrap` 完整期望 Data，通过同一个 helper 核文档总数、
每个 key 存在和各 value 字节完全相同。没有仅删除数量门或接受任意额外文档。
新增两个直接 integration roots 对每个 canonical 文档分别覆盖缺失、追加合法 JSON 尾空白
导致的字节变化、额外文档，以及完整未变输入和恢复。拒绝路径必须保持 Service 写入数/
原绑定事实不变，或保持 Native 请求数不变。

## managed PostgreSQL 端口

现有 managed Helm 数据面使用 PostgreSQL 55432。managed Compute rawSpec settings、
VM guest port、SQL Service 与 Native observer/route 地址沿共享 const 统一为 55432；
SQL Service targetPort 保持命名 postgres。management 3080 不变。
Legacy 的 55433 和 cluster.go 旧 guard 不变，没有扩大所有 route 地址的接受范围。

## 三种 SHA 语义

| 名称 | 精确输入 | 当前阶段用途 |
| --- | --- | --- |
| config_sha256 | Config Secret 的 config.json 原始字节 | 新 Binding 固定实际 Compute 配置 |
| secret_data_sha256 | 原 dataHash 算法：json.Marshal 完整 Secret.Data 后 SHA256 | 新 Binding 固定全部文档输入 |
| 旧 runtime document SHA | 原 bootstrap.Runtime canonical runtime 文档序列化后 SHA256 | 保留原 Root/runtime-prepare 合同，不能冒用 |

本轮原实际对象三个公开摘要分别为：
config `7d3a062c8bdb6f9c28b5da1e21698d8d68ea0091509a0a81725356e7e48dafe1`；
Secret.Data `45596de5e54b90910495ad72ce1b044e5b337301ac388731ebb5232362dd0334`；
旧 runtime document `1fc883a7086638d76b61cea32991d03c4d470bb2e5b1d6aeac824e705dd419b2`。
新 Binding 仅用清楚命名的前两项与原 ConfigUID/VMUID/incarnation/Native 实际来源绑定，
不重建或自报第三项当作新证明，不改旧 Root 的 runtime_sha256 语义。

## 实际 routes reader 两层重复键拒绝

真实 registry reader 保留原 2MiB 上限，整张 map 新增顶层 endpoint key 唯一和尾随 JSON 检查；
这一层不把合法 Legacy opaque 策略字段纳入 managed 递归深度限制。

含 managed_binding 的 record 才进入已有 bounded JSON 递归重复键校验及真实 strict Binding
静态检查；重复 managed_binding、Binding 未知字段、null、嵌套重复字段均拒绝。
这里没有新增 API 探测，fresh live source 检查仍在原 selected/managed validator 路径。
Binding 原由 RawMessage 保留，原 selected 已会拒绝未知 Binding 成员；此次补的是实际
reader/ready 入口，而非声称旧 decoder 会把这些成员全部丢掉。

直接回归通过 s.routes 入口覆盖上述负例、重复 endpoint、尾随数据及上限，并保留合法 Legacy
48 层 opaque 字段兼容样本。单独调用 validator 的测试不能代替真实 reader 测试。

93b646 首次真实回归的 Control 12 根已通过，但新 reader 将普通 ValidateJSON 的 maxBytes
参数传成 2MiB，超过该入口原有的 1MiB 参数上界，导致所有 managed record 被拒。
v6 最小改用原 ValidateObservationJSON(record, 2MiB, 65536)：复用同一重复键、32 层、UTF-8、
尾随数据检查，保留 registry 原 2MiB 上限。新增大于 1MiB 且小于 2MiB 的实际 reader 正例，
并明确 reader 只 strict decode Binding RawMessage + Validate；route 一致性仍由原 selected
检查，不提前扩大静态 Binding 门。93b646 整体 FAIL 保留，不能因 Control 通过改写整票。

## 冻结来源、回归范围与复用边界

源码基线为 CP `bc8b4e7e3d4019b298d3156f6059d91fcc0304a7` 加本轮明确 owned 17 文件。
api/go.mod 仅含 Root 唯一写入的两个已批准原锁定版本 indirect require；go.sum 不变。
本次 v5 相对 v4.1 只改六个 owned 文件。固定 SDK 内 128Mi、0.25 CPU、无网络实际 gofmt
exit 0 后冻结 1433 regular files/0 symlink。格式化成功不是测试成功。

- 最终源票：`/data/neon-build/validation/managed-SQL-route-v5-source-PREPARED-20261104T034317Z-8e0687/proposal.v5.formatted.json`
  SHA256 `55a172d3160be1b8a90dc02cef3ad63f640538709d5736232d9fad5eaa5efe7d`。
- 全源 manifest：同目录 source-manifest.v5.formatted.json，
  SHA256 `b0f1be60de4e9117c286f23b3339b8ac0dff515554ab7a5cf27da6ce2ce6ee13`。
- 原 v5 worker（历史，93b646 整体 FAIL 保留）：`/data/neon-build/validation/managed-SQL-route-v5-Go-20261104T034809Z-93b646/worker.py`，
  SHA256 `d88d2b4ec0926280f68d919a895dba51e70d34d287677c4758cc60f7f77c7ce7`。

实际组件矩阵与来源：

- Control 12 roots：93b646 实际全部 PASS，零 fail、零 skip（原失败 10 根加 canonical Data
  两个新增根）。v6 的全部 Control Go 文件及 managed Adapter validator 文件与 93b646 逐
  SHA 相同；这些结果直接复用，没有重新启动 metadata。
- Adapter 11 roots：v6/c5d8a3 实际全部 PASS，共 63 个含子案例的 pass 事件，零 fail、零 skip，
  使用 race。实际 reader 的重复键、unknown/null、1–2MiB、静态边界及 Legacy 兼容均在内。
  旧 Adapter 11 roots/55 PASS 仅为历史，未替代此次 reader 修改后的实际回归。
- 原 v4.1/4e105b 的 Control 16 roots：逐 root 函数体字节相同，按 Root/独立审查确认的调用边界
  复用。Data helper 只有 Services/初始化 observer 两个 caller，其 history 调用已被上述 12 根
  覆盖；VM/Renderer 不反向进入 helper，Publisher 四根使用独立 fresh Native observer 和未改
  exported validator；JWT/Legacy 不达这些门，也不经过 s.routes。

三组根函数集合无重叠，去重合计 **39 unique roots = 12 + 11 + 16**；这是跨三个真实任务的
准确组件复用，不是一次执行 39 roots，更不是把旧整体 FAIL 改写成 PASS。

v6 任务目录：
`/data/neon-build/validation/managed-SQL-route-v6-Go-20261104T040506Z-c5d8a3`。
source-lock.v6.json SHA256
`7e8ecde944f462a9c173cccb69a506c0a5180deb8e29b126720042722c792106`；
worker.py SHA256
`674396bfc0c58e9e9ec07e319a26a99c0c542f7043a76d0d55c75368f292df13`。

该任务实际 85.671627762 秒 PASS，原 unit inactive/dead、MainPID 0、exit 0，原 reconcile 0。
共享父组实际 memory.max 3221225472、memory.swap.max 0、cpu.max 300000/100000；
峰值 1074814976 字节，OOM/oom_kill 0。没有启动或使用 metadata fixture，也未挂生产凭据。

正式 Adapter ELF：export/adapter，58056916 字节，
SHA256 `862b268006e8af0648f04e9e920aa46fdccbe84785b401083a084232297e414d`。
正式 managed-route CLI：export/managed-route，57890581 字节，
SHA256 `5b39b87ee058c963037f81d1f2e1526600322472da8edcb5492e1bb6008538c0`。
CLI help 实际 exit 0；空 Branch 固定拒绝实际 exit 1/零 API，不使用真实发布参数。

精确合成票为该目录 combined-component-acceptance.json，
SHA256 `e16faa96971e6e8710cea7855ac727ebafc10721c5b9e40eddb3861e96042191`；
它逐组列 root name、原结果 SHA、源码复用边界、两个 ELF 及实际资源信息。
组件通过不授予实际路由发布；生产调用仍须 fresh Native 初始化成功及全部来源门通过。
Native 503 仍必须零发布，不能用本组件票或旧持久化 fact 绕过。

## 原失败保留及实际生产边界

| 原 nonce | 原实际边界 |
| --- | --- |
| 488a1a | 离线依赖缓存缺失，零测试根；不是生产逻辑失败 |
| ef12ac | 原失败/skip 保留，涉及依赖声明、fixture 环境与解析边界，不能改写通过 |
| 978fb5 | Control 两处局部变量引用编译失败；Adapter 11 roots/55 PASS 为历史事实 |
| 4e105b | 整体 FAIL；16 Control roots PASS，10 roots 因 canonical Data 旧 guard 失败，零 skip |
| 93b646 | 整体 FAIL；12 Control roots PASS，Adapter 8 根 PASS/3 根失败，零 skip；原因是 reader 错用普通 JSON 入口限额 |

4e105b 原 terminal-component-summary.json SHA256 为
`2fbf2f0295b671164ab1ef5fdd4b2db34b18a444c60aad90dc6a9166971a6a30`。
原目录、源码及结果均保留。新结果只引用并更新结论适用范围，不能回写旧 FAIL。

当前完成范围为源码组件回归及两个正式产物。真实 Native 初始化、生产 route 发布、
Proxy SQL 身份与 512-row checksum 仍分别取得实际票后记录；不以 PodReady、Compute running、
源码审查或本组件票代替。这是当前 PG17 数据面桥接收口，不冒称最新完整控制面目标完成。
