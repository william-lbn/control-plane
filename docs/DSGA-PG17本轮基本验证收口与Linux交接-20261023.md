# PG17 本轮基本验证收口与 Linux 交接

> 最新澄清（2026-10-23）：此前将“节约重复测试”理解为可以停止功能开发，是执行理解错误。
> 暂停结论撤回；当前Goal active，必须继续完成PG17全部功能代码及Mac必要验证，
> 再交Linux作目标环境验收。下面测试事实保留历史证据，不作为完整功能完成依据。

日期：2026-10-23。本文件仅保留凭据模块当时的基本验证记录；此前错误理解为
可以停止开发的结论已撤回。当前继续PG17完整功能开发，不重复未变用例；其他PG
暂缓，每仓保留原汇总分支。**本记录不表示完整V3完成。** 默认关闭，不构建镜像、
准备/执行部署、合入main或发布。本段原暂停结论已撤回，按上方澄清继续完整功能开发。

## 提交与范围

| 仓库 | 统一功能分支 | 本批代码/既有依赖 |
|---|---|---|
| control-plane | `codex/dsga-control-plane-v3-20261020` | `1a7b51f42719880879977e28cca5f113319857be` |
| neon | `codex/dsga-neon-v3-20261019` | 本批只补交接文档；生产Rust未变 |
| postgres | `codex/dsga-pg17-v3-20261019` | `fcada44b67d239aea8e699a721b99c470fc24d65`，未改 |
| autoscaling | `codex/dsga-autoscaling-20261019` | `8e1e0ea93d5f2775376622a38ed10f39d9d1fc8c`，未改 |

控制面完整包含自有main024与原DSGA模块；Neon/Autoscaling/Neon PG17/社区PG17
官方维护关系沿用本日已完成的fresh fetch/祖先证明，不因基本验证再制造空维护提交。
Postgres实际源码仍在Neon vendor/postgres-v17；gitlink/两份版本锁/实际检出一致，
本批发行绑定静态检查通过（18 binaries、4 pinned PG versions、39 extension stages；
这是配置一致性检查，不表示四个PG都实现了本功能）。

本批补齐凭据生产核心：接受前通过中心typed客户端写入、读回immutable Secret，
把实际UID和完整Data域摘要放入原创建计划。来源另核对原Operation/incarnation/
Compute ID；worker第一次读取前即有不可变引用，拒绝首次收养同名替换对象。
重试先GET，不改盐或seed；一次POST关闭429透明重试，409读取赢家，未知写入
下次按同原意图对账。已冻结UID丢失不重建。只读/复制实例读取可信冻结来源并
复制两个SQL verifier，管理seed独立。明文不进入Secret/Operation，seed不进入Guest。

口令按12—256输入字节限制，拒绝NUL/非法UTF-8；SASLprep与固定pgx PRECIS
归一化必须一致。部分Unicode（例如全角兼容字符、软连字符）会在接受前拒绝，
避免两个客户端产生不同认证结果。只影响新DSGA路径，不更改旧口令路径。
固定新增stringprep v1.0.4，保留当前x/text版本，未升级其他依赖。

## 本轮实际 Mac 验证

准确PG17二进制：`postgres (PostgreSQL) 17.11 (fcada44b67d239aea8e699a721b99c470fc24d65)`。
一次性独占数据库、DSGA关闭/local heap用于凭据与元数据验证；测试后PG均正常
停止。Kube侧为真实TLS与typed客户端协议夹具，**不是实际K8s认证/etcd/准入验证**。

| 记录 | 实际结果 | 解释 |
|---|---|---|
| `build/dsga-credential-producer-basic1` | 8顶层选择，21pass事件/6fail事件，0skip；8.119秒 | 6个顶层通过；两组恢复夹具失败保留，不称全绿 |
| `build/dsga-credential-producer-recovery2` | 仅失败两组，2顶层/6pass事件，0fail/skip；6.360秒 | 修正夹具collection分流后定向补验 |
| 最终按用例合并结果 | 8顶层、27唯一pass事件，0最终fail/skip | 分两次定向证据，**不是最终源码单次全套运行** |
| 受影响control包 `go vet` | exit 0 | 日志为空，未扩大到未变全仓 |
| `git diff --check` | 通过 | 中文关键注释与提交说明随代码提交 |

首轮失败原因：凭据与Config共用Secret collection，新增测试夹具把Config POST也
当成凭据POST，恢复时误报409。只改`dsga_creation_credentials_test.go`按实际对象
分流并保留请求原字节；两轮生产源码逐文件相同。最终489个源文件清单与补跑一致。
已通过且不受该分流修正影响的生产者/认证/首次来源/锁等待用例不重复运行。

新增/受影响覆盖：正常创建与无写重试、未知结果原意图对账、409赢家、429不重写、
成功响应后Data漂移、拒绝不同Operation/口令/丢失的冻结UID、复制来源UID核对、
独立seed、随机源短读、完整SCRAM StoredKey/ServerKey、首次worker前UID/seed/
三项来源标记变更拒绝、无ref历史计划拒绝启动、真实SQL来源不可变与lease恢复。
ASCII、中文、非ASCII空格、组合字符四类verifier分别完成真实pgx和libpq/psql
登录及current_user核对。不是只检查哈希字符串或模拟认证。

原PG内存内核、Compute/SDK/Agent及其他未变功能测试不重跑；其证据保留原提交SHA
和原边界，不改标成这次全量通过。未做镜像/部署准备。机器清单与日志SHA256见
`docs/acceptance/dsga-pg17-basic-close-20261023.json`。

复验仅在修复或Linux需要时执行；应提供独占可清理PG、明确psql路径及专用schema：

```sh
# NEON_V2_TEST_DATABASE_URL指向测试专用PG；不得指向业务库。
# NEON_V2_TEST_SCHEMA使用v2_migration_*专用schema；DSGA_TEST_PSQL为配套psql绝对路径。
cd api
go test -race -count=1 -timeout=120s ./internal/control \
  -run '^TestDSGACredential(Producer|Source(Frozen|Actual|Lost|Lease))'
```

## Linux 验证与尚未完成业务分开记录

Mac可验证业务逻辑不意味着Linux内核免验。下一阶段必须验证实际Linux/NeonVM/
HugeTLB环境下的共享映射、预留/提交与真实物理回收，内存压力下增长/收缩，数据库
并发、长事务/缓冲区竞争、取消/超时/进程与VM故障/重启，以及Neon存储读写、WAL/
复制与性能长稳。还须实际K8s的CRD/admission/RBAC、投影令牌/TokenReview、etcd
持久性、owner/boot/lease切换和端到端回执；Mac协议夹具不能替代。

**以下仍是业务代码接线缺口，不能写成仅待Linux验证：**公开DSGA接受/worker与
幂等孤立来源处理、可信catalog源选择、完整VM管理TLS模板/原实例初始化回执、
完整预算/quiesce/最终Runner闭环及完整Neon存储E2E。当前可在Linux验证已提交
PG17内核与组件链；不能把公开入口完整自动伸缩当成已交付或直接启用生产。
上述代码缺口继续实施；节约流程仅指不重复未变功能测试，不能提前结束业务开发。
