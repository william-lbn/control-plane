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

不授予 schema CREATE、TEMP、对象 ownership、角色 membership、管理权限、BYPASSRLS、
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
当前保守合同拒绝与运行角色有关的所有 membership，因此专用 superuser 测试通过
不能证明 Neon `control_probe` 的真实创建路径可用。后续 Driver 必须定义并核验
专用 provisioning principal 的 ADMIN/INHERIT/SET 委托，不能仅放宽 membership
查询或给客户函数 superuser 以绕过这项准入。

SQL role 与密码不能成为 compute 普通高权限 role spec。未来 Driver 必须把服务
SCRAM verifier 只发布到该 branch 的 Proxy registry，并在冷启动时保留已授权服务身份。
删除/轮换须先排空原 Function VM、停止 route admission，再按原 scope 禁用 login；
不得删除用户表或把密码轮换等同于 compute 创建。

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

必须继续完成 immutable SQL Secret、原结果未知恢复、Neon Proxy route、leased Worker、
真正 Node/pg 读写、跨分支凭据拒绝、冷醒与正常退休、UI ZIP/config-only 部署和返回结果。
本次 PostgreSQL 库测试不替代这些验收，也不放行 HA 或外部缩零 fencing。

运行连接核验和补强负例已在 Linux Job `dataapi-quality-20261011162836` 通过；
全量 **610 pass / 0 fail / 0 skip** 包含本包的实际 PG/SCRAM/RLS 正负例。
与历史 507 项和运行版 593 项验收分开记录。当前部署版本以镜像锁为准，
本包测试仍不等同于 Functions guest 内真实 SQL、Proxy database 隔离或产品部署验收。
