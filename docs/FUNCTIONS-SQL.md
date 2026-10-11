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
| Database / Schema | PostgreSQL identifier ≤63；拒绝 pg_/neon_/control_ 和 information_schema |
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

2026-10-11 全量 Linux Job `dataapi-quality-20261011015411` 507 pass / 0 fail / 0 skip，
包含上述 SQL tests、Functions 元数据授权、分页和四类 API 的真实 bigint ETag 检查。
初次只读 API 测试揭露 ETag=0 的失败记录保留；修复后重新运行全部 Go/PG 检查。

## 5. 后续闭环

必须继续完成 immutable SQL Secret、原结果未知恢复、Neon Proxy route、leased Worker、
真正 Node/pg 读写、跨分支凭据拒绝、冷醒与正常退休、UI ZIP/config-only 部署和返回结果。
本次 PostgreSQL 库测试不替代这些验收，也不放行 HA 或外部缩零 fencing。
