# Functions SQL 身份与授权合同

## 1. 目标与状态

`api/internal/functionsql` 提供独立 Go 权限边界，供后续 leased Worker 调用。
guest supervisor 包不依赖管理 SQL 客户端，客户 VM 不得到 provisioning 凭据。
该包已在真实、专用 PostgreSQL 中验证身份和权限；**尚未接入 Functions 部署 Driver，
尚未证明 Neon VM 内真实函数 SQL 或 UI 函数部署可用**。

官网 Functions 可以通过分支 DATABASE_URL 使用数据库；首版自托管实现采用明确授权的
单函数 DML role。不是把控制面 owner、cloud_admin 或 control_probe 密码交给客户代码。

## 2. 输入与数据关系

| 输入 | 约束 |
| --- | --- |
| FunctionID | fnc_ + 16 hex，来自已授权持久定义 |
| ProjectID / BranchID | prj_/br_ + 16 hex，原生创建身份 |
| Database | PostgreSQL identifier ≤63；必须精确等于 current_database() |
| Schema | PostgreSQL identifier ≤63；拒绝 pg_/neon_/control_ 和 information_schema |
| ProvisioningRole | 可选、显式持久化的可信控制角色；非客户输入；单函数权限标记同时绑定此身份 |
| Verifier | 固定 SCRAM-SHA-256 / 4096 格式，来自独立 immutable SQL Secret |

role 固定为 `fn_{FunctionID hex}`。数据库角色 comment 绑定 project、branch 和 function。
数据库本身必须等于 Scope.Database；caller 必须先依据已保存 Writer selector、租户授权、
metadata version 和 Operation lease 建立连接。此库不接受 DSN，不自行选择 writer、
读写 Kubernetes、改 Proxy 路由或声明服务就绪。

## 3. 事务执行和权限

`ProvisionTx(context.Context, pgx.Tx, Scope, verifier)` 由 caller 控制事务、超时和提交。
先检查数据库和 schema，再对 function 获取事务级 advisory lock；不存在角色才创建。
已存在但 comment 不匹配或权限超出预期时拒绝，不收编也不偷偷降权修补。
同一 scope/verifier 的重试保持幂等；失败事务不会留下新 login。

授予：当前 database CONNECT、指定 schema USAGE、现存普通表 SELECT/INSERT/UPDATE/DELETE、
现存 sequence USAGE/SELECT。设置 connection limit=4、idle_session_timeout=5s、
statement_timeout=30s、指定 search_path。RLS 保持有效。

不授予 schema CREATE、TEMP、对象 ownership、运行角色的角色 membership、管理权限、BYPASSRLS、
视图/外部表或 SECURITY DEFINER 执行。不会代表所有客户 owners 安装 default privileges；
新表需要 owner 显式授权并重新调谐。

这里核验的是**当前连接 database 内**的权限。PostgreSQL role 在整个实例内存在，
其它 database 的 PUBLIC CONNECT 和对象权限不能从当前连接自动证明安全。未来
Driver 必须另实现精确 database 路由授权，或由分支 owner 明确建立并验证整个实例
的 CONNECT/PUBLIC 策略，再做实际跨 database 负例。当前库不能据此宣称客户代码
被隔离到唯一 database；这个门槛不能通过静默撤销其它用户 database 权限来绕过。

### 3.1 数据库 owner 的必要检查

PostgreSQL 默认把 TEMP 授予 PUBLIC。预览权限边界会拒绝这样的 database；由 database
owner 明确决定策略，Driver 不会自动撤销共享 PUBLIC 权限。专用测试 database 示例：

```sql
-- 只在明确选择的 Function database 中，由 owner 判断并执行。
REVOKE TEMPORARY ON DATABASE my_function_database FROM PUBLIC;
-- 为 Functions 创建 app schema / 表后，按其明确业务范围提供 grant option。
-- 不得把这些命令批量施加到所有用户 database。
```

可访问的 PUBLIC schema CREATE、范围外 table/sequence 权限、非系统 SECURITY DEFINER
也会导致拒绝。普通系统 catalog 可见性仍遵循 PostgreSQL 本身；完整扩展/function
allowlist、安全审计与通用 SQL 网络访问限制仍有独立门槛。

后续权限补强同时检查列级授权、授权转授选项、TRUNCATE/REFERENCES/TRIGGER、
sequence UPDATE，以及同一 schema 内可访问的 view、materialized view、partitioned
table 和 foreign table。不能把“没有表级 SELECT”当作没有列级 SELECT，也不能把
owner view 的作用域视为底层数据的作用域。预览实现拒绝授予 DML 的普通表上所有
enabled 非内部 trigger；它们有独立执行语义，需要后续专门的执行 allowlist 门槛。
系统 schema 的过滤使用字面 `pg_` 前缀；不使用带下划线通配符的 `LIKE 'pg_%'`，
避免把 `pgx_private` 这类普通客户 schema 错当成系统 catalog 而跳过权限检查。
不会为通过检查自动撤销已有 PUBLIC 或客户授权。参照 PostgreSQL 16
[GRANT](https://www.postgresql.org/docs/16/sql-grant.html) 和
[权限检查函数](https://www.postgresql.org/docs/16/functions-info.html)。

### 3.2 实际运行连接核验

`VerifyRuntimeSession(ctx, *pgx.Conn, Scope)` 检查已经完成认证的专用连接。
Caller 必须先查证原 Writer selector、TLS hostname/CA、immutable credential receipt
以及当前租约和父资源；函数不接受客户 DSN，不把 SQL comment 当作物理分支证明。

方法以最多 10 秒的只读 repeatable-read 事务检查 current database、session_user、
current_user、原 scope comment、LOGIN/connection limit、角色权限、search_path、
30 秒 statement timeout 和 5 秒 idle timeout。新普通表/sequence 未获得完整预期
授权时拒绝，等待 owner 明确重新调谐。已有 caller transaction 会被拒绝且保持原状；
失败清理使用单独有界 context，不返回服务器错误或凭据，不执行客户 SQL/DML。

这是一份当前 catalog/连接状态观察，不是数据库 owner 后续 DDL 的外部栅栏；owner
仍可在观察后改变授权或启用 trigger。Driver 需要在实际 guest SQL 和发布前使用此
观察，但不能据此放行跨实例 fencing、跨 database 隔离或永久安全声明。
`ALTER ROLE ... SET` 提供连接默认值；客户可在后续 SQL 中修改可设置的 GUC。
本次探针不是不可修改的 statement deadline。真实 Driver/guest 仍需请求超时、
取消查询、排空连接和 VM 退休的独立验证，不能借默认 30 秒 timeout 声称所有
后续客户 SQL 都受强制时间栅栏保护。

PostgreSQL 16 中非 superuser 的 CREATEROLE 创建者会得到新角色的管理 membership。
已发布的 `f0b63f5` 保守合同拒绝与运行角色有关的所有 membership，因此其专用
superuser 测试不能证明 Neon `control_probe` 的真实创建路径可用。

下一阶段源代码增加可选 `Scope.ProvisioningRole`，空值保留旧的严格合同。非空值
必须来自不可变、已授权的控制元数据；它写入 role marker，不能通过修改 caller
输入收编旧身份。执行 provisioning 时 session_user/current_user 都必须等于该角色，
该角色必须为非 superuser CREATEROLE，且 `createrole_self_grant` 必须为空；不会为
通过检查自行 SET ROLE 或修改 GUC。运行角色不能是其它角色的 member。

只允许一个精确的 creator 管理关系：grantee 是已记录的 provisioning role，
grantor 是 PostgreSQL bootstrap superuser OID 10，ADMIN=true、INHERIT=false、
SET=false。此关系必须存在；任意其它 grantee、grantor 或 SET/INHERIT 授权均拒绝。
这不把 creator 的 ADMIN 权限授予运行函数，也不证明这个 catalogue 关系的历史
创建来源；可信 database/role 管理者仍能修改它。对应真实 PG16 测试已在 Linux
`dataapi-quality-20261011180043` 通过；全量 666 pass / 0 fail / 0 skip。Neon 实际 Writer/Proxy
路径、权限委托和跨 database 负例仍需独立验证。

SQL role 与密码不能成为 compute 普通高权限 role spec。未来 Driver 必须把服务
SCRAM verifier 只发布到该 branch 的 Proxy registry，并在冷启动时保留已授权服务身份。
删除/轮换须先排空原 Function VM、停止 route admission，再按原 scope 禁用 login；
不得删除用户表或把密码轮换等同于 compute 创建。

### 3.3 独立的可信运行连接

`functionsql.ConnectRuntime(ctx, ConnectionPolicy, password)` 的代码已加入下一阶段。
发行、Linux 结果和部署状态单独记录；它不是现有 Console SQL helper 的别名，
也没有启用 Functions。Policy 只由 trusted Driver 构造，完整包含已持久 Scope、
原 Writer ID、精确私有 IPv4/端口、TLS hostname、显式 CA 和最多 120 秒预算。
不接受用户 DSN、URL、系统 CA、HTTP proxy、DNS 重定向或 plaintext fallback。

Caller 先核验原 credential UID/MAC、当前 Operation/父 intent 和原 Writer route
所有权；本方法再以 Fn role 建立实际连接、检查 authenticated session 和 catalog。
方法本身不查询 metadata/K8s，不把 role comment 当作物理租户/分支证明，不实现
跨 database 或多 Worker 外部栅栏。返回连接由 caller 有界使用并关闭，不入池、
不自动重连，也不重放结果未知的 SQL。

TLS 采用显式 trust roots、hostname 验证、最低 TLS 1.3。固定地址和逻辑 hostname
分开，保留 SNI/证书名称核验；`PGHOST`、`PGOPTIONS`、ambient 证书/密钥路径不能
改写目标或连接 GUC。所有相关 pgx Host/Port/TLS/Fallback 字段同时设置，fallback
为空；使用从不拨号的 Unix socket 解析模板，避免 pgx 在配置 TLS 前读取 ambient
证书路径。`statement_timeout=30s` / `idle_session_timeout=5s` 保持 role 默认值，
不会用 Console helper 覆盖为 120 秒。Policy JSON 序列化为 `{}`，错误固定且不
含 DSN、密码、verifier、远端 SQL 错误或内部地址。

固定版本 [pgx 5.9.2](https://github.com/jackc/pgx/blob/v5.9.2/pgconn/config.go)
没有 `require_auth` 开关。本包只在交给 pgx 之前核验有界的 startup authentication
帧：要求 SASL/continue/final 后才能接受 AuthenticationOk，拒绝 trust、明文、MD5、
错误顺序和超大帧。SCRAM 机制和 server proof 仍完全由 pgx 实现；没有自己实现
SCRAM 或把密码/SASL 帧写进 trace/log。保留真实 tls.Conn，使 pgx 能协商 SCRAM-PLUS。

专用 Linux CI PostgreSQL 必须使用 `POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256`。
新增测试明确验证错误密码拒绝，不再把默认回环 trust 登录当作 SCRAM 认证证明。
TLS/真实 PostgreSQL 测试采用专用 TLS routing fixture，只在该 fixture 移除 Neon
endpoint startup 参数；真正的 PG16 执行认证、权限、默认 GUC 和 DML。这个 fixture
不是 Neon Proxy，实际 Neon/guest/跨 database/冷醒仍需另验。

## 4. 实际测试与复测

测试必须在 Linux、专用 `control_ci` PostgreSQL 中运行；测试创建自己的 database/role，
结束只回收自己的临时资源。没有 PostgreSQL 时测试明确 skip，CI 把 skip 当失败。

```bash
# 专用可丢弃 PostgreSQL，不能传入业务 metadata 或用户分支 DSN。
export NEON_V2_TEST_DATABASE_URL='postgres://ci:ci-disposable@127.0.0.1:5432/control_ci?sslmode=disable'
cd api
go test -race -count=1 ./internal/functionsql
```

已验证真实 SCRAM 登录、表/sequence DML、私有数据读写拒绝、CREATE/TEMP/角色管理拒绝、
角色切换和 pg_authid 读取拒绝、跨 branch marker/不同 database 拒绝、PUBLIC 权限和
SECURITY DEFINER 负例、已升权角色拒绝以及失败事务无残留 login。
补强测试还执行真实 RLS：隐藏行不能读取/删除，WITH CHECK 拒绝隐藏写入，
`SET row_security=off` 不能绕过策略；明确允许的行写入由 owner 再读确认。

2026-10-11 全量 Linux Job `dataapi-quality-20261011015411` 507 pass / 0 fail / 0 skip，
包含上述 SQL tests、Functions 元数据授权、分页和四类 API 的真实 bigint ETag 检查。
初次只读 API 测试揭露 ETag=0 的失败记录保留；修复后重新运行全部 Go/PG 检查。

## 5. 后续闭环

### 5.1 下一阶段 SQL 凭据与持久身份合同

下一阶段新增 `functions_sql_secret.go` 和 Migration 022。真实专用 PostgreSQL
的回执、租约、不可变 trigger、重启恢复与拒绝变更测试已通过，Linux 全量 666/0/0。
真实 Kubernetes 验证和发行分别记录；当前运行的 `f0b63f5` 仍是 Migration 021，
不含这些新代码，也未启用 Functions 执行。

真实 Linux Job `functions-foundation-20261011180347` 4 pass / 0 fail / 0 skip，
使用相同归档源码。SQL Secret 原 UID 为 `5c4b9b4e-e515-4966-8aec-b2b2cdd0efe6`，
通过实际 immutable admission、已提交 POST 回复丢失、独立 client 重启恢复、
原 UID 重读与替换 UID/跨分支/不同密码拒绝。原 Secret 保留；其它三项是实际
MinIO artifact、env Secret 和 Service/VM admission dry-run，不是函数 VM 执行。

复测须使用专用投射 ServiceAccount，Secret GET 仅放行预先分配的原测试名称，
不授予 list/patch/delete；原函数/部署身份由 operator 明确传入，不能复用随机
scope 覆盖既有 Secret。下例只验证 SQL backing，不启用 Functions 或操作业务数据库：

```bash
export NEON_FUNCTIONS_LIVE_SQL_SECRET_TEST=true
export NEON_FUNCTIONS_LIVE_FUNCTION_ID=fnc_<预先分配的16位hex>
export NEON_KUBE_NAMESPACE=<专用测试namespace>
cd api
go test -mod=readonly -count=1 -tags=live \
  -run '^TestFunctionSQLSecretLiveKubernetes$' ./internal/control
```

凭据属于 Function definition，首版 generation 固定为 1；不同部署重用原身份，
不通过滚动部署重设仍在运行的函数密码。轮换需要原 VM/连接排空和路由停用，
尚无公共轮换 API。Secret 名称为 `fn-{function hex}-sql-1`，类型 Opaque、
immutable=true，只有 `sql.json`，内含独立 Password/SCRAM Verifier。

HMAC 绑定 namespace/name、Function/Project/Branch、database/schema、可信
provisioning role、原 Writer、credential generation、MAC key version 和精确
canonical 内容。验证真实 UID、全部作用域标签、annotation、不可删除状态、
唯一数据键和 canonical JSON；错误只返回固定描述。私有 record/reference 默认
JSON 序列化为 `{}`，不返回 API 或写入日志。

创建前 GET；首次明确 404 后只发一次 POST；未知结果只 GET 核验原对象。
同名不同内容拒绝，不 PATCH、替换或删除。原 proposal/回复丢失时，显式
`findFunctionSQLSecret` 只读找回原可信 scope/key 对应的签名内容；随后必须持久化
原 UID/MAC。已有持久回执必须使用 `readFunctionSQLSecret` 核验原 UID，不得退回
“同名即收编”的恢复方法。

`function_sql_credentials` 不含密码、verifier 或 DSN，只存作用域、原 Secret UID、
内容 MAC、key version 和原创建 Operation。Function/Project/Branch/Writer 具备
外键，数据库/Schema/role 与 definition 精确一致；真实数据库 trigger 拒绝改写。
首版可信 provisioning role 固定为 `control_probe`。保留候选使用的同一 Operation
租约和父 intent 行锁，提交前再次用 wall clock 核验；租约失效、旧 worker、父删除、
不同 UID/MAC/key、数据库/Schema 不匹配时拒绝。事务不调用 K8s/guest/业务 SQL。

Secret 回执、角色 provisioning、实际登录探针、Writer/Proxy/TLS 核验必须逐步接入
同一 leased Driver。回执代码存在不能放行函数调用、跨 database 隔离、凭据轮换
或跨实例 fencing。

```mermaid
sequenceDiagram
  participant W as Leased Worker（待接通）
  participant M as Metadata PostgreSQL
  participant K as Kubernetes API
  participant P as 原分支 Writer / Proxy
  W->>M: 锁原 Operation / definition / 父 intent，读取已授权 scope
  W->>K: GET 原 SQL Secret，核验签名；明确缺失时 POST 一次
  Note over W,K: 结果未知只 GET 原对象；不轮换、替换或删除
  W->>M: 同一租约记录真实 UID/MAC/key/Writer，wall clock 再核验
  W->>K: 按持久原 UID/MAC 重读凭据
  W->>P: trusted control_probe / 原 Writer，ProvisionTx
  Note over W,P: 仅允许 bootstrap ADMIN、INHERIT=false、SET=false；不改变 PUBLIC 策略
  W->>P: 独立 Fn role / CA / hostname / selector 登录探针（待接通）
  W->>M: 再核验父 intent / 租约，再继续候选部署（待接通）
```

图中的外部调用不能组成跨系统原子事务。每步都必须恢复原结果、保留失败并继续
检查 authoritative intent。Metadata 租约不是 SQL/Kubernetes 的外部 fencing；即使
单 Worker 串行正反例通过，也不能放行多实例准入。

必须继续接通 immutable SQL Secret、原结果未知恢复、Neon Proxy route、leased Worker、
真正 Node/pg 读写、跨分支凭据拒绝、冷醒与正常退休、UI ZIP/config-only 部署和返回结果。
本次 PostgreSQL 库测试不替代这些验收，也不放行 HA 或外部缩零 fencing。

运行连接核验和补强负例已在 Linux Job `dataapi-quality-20261011162836` 通过；
全量 **610 pass / 0 fail / 0 skip** 包含本包的实际 PG/SCRAM/RLS 正负例。
与历史 507 项和运行版 593 项验收分开记录。当前部署版本以镜像锁为准，
本包测试仍不等同于 Functions guest 内真实 SQL、Proxy database 隔离或产品部署验收。
