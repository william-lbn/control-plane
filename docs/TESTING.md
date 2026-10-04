# 测试与交付标准

## 1. Linux CI

普通 GitHub CI 不访问真实集群或用户凭据。

| Gate | 命令/环境 | 判定 |
| --- | --- | --- |
| Go | 专用 PG16 + 固定 PostgREST 16.4，tools/ci-go.sh | gofmt、vet、race、真实 PG/RLS 集成，零 skipped |
| API contract | Go AST + bundled OpenAPI | implemented/documented route 双向一致、所有引用可解析、Cookie 合同一致 |
| Web | npm ci / format:check / test / build | CSPRNG fallback 回归、strict TS、Vite 静态产物 |
| Workflows | 固定 actionlint 1.7.12 / Linux ShellCheck | 校验全部 CI/live workflow；拒绝非法上下文和 shell 问题 |
| Helm | tools/ci-helm.sh | lint/template；拒绝多 API 副本、缺 CA、浮动 tag；gateway 无 hostPath |
| Image | protected main/tag after gates | 固定 builder、SHA tag、API/Web/Gateway/DataAPI 四镜像、SBOM/provenance、digest receipt |

```bash
export NEON_V2_TEST_DATABASE_URL='postgres://ci:ci-disposable@127.0.0.1:5432/control_ci?sslmode=disable'
export NEON_CI_ATTEMPT="manual_$(date -u +%Y%m%d%H%M%S)"
bash tools/install-postgrest.sh /tmp/neon-postgrest
export NEON_DATA_API_TEST_POSTGREST=/tmp/neon-postgrest/postgrest
make test web
PATH="/tmp/neon-helm:$PATH" make helm
```

每次测试使用新的 schema/attempt；失败 evidence 保留。
真实 PG tests 使用独立测试 schema。运行人对传入 DSN 负责，不得用生产库。
Data API 集成另要求数据库名 control_ci 或 dataapi_ci，使用实际独立 NOINHERIT/NOBYPASSRLS
角色和 PostgREST 子进程；配置/数据库只在 disposable CI 使用。具体边界见 DATA-API-FOUNDATION.md。

## 2. 真实 UI：项目到数据库

TypeScript Playwright 替代本仓库之外的 Python 验证入口，retries=0、workers=1。
这是 opt-in 的真实集群测试，会创建持久项目和数据，必须有授权。
Trace/video/自动截图关闭；手动截图 mask 密码；秘密单独保存，不上传 GitHub artifact。

```bash
cd web
npm ci --no-audit --fund=false
npx playwright install --with-deps chromium
export NEON_E2E_BASE_URL='https://console.example.org'
export NEON_E2E_ADMIN_PASSWORD_FILE='/secure/e2e/admin-password'
export NEON_E2E_PRIVATE_DIR='/secure/e2e/attempt-001'
export NEON_E2E_ARTIFACTS='/var/lib/neon-evidence/attempt-001'
export NEON_E2E_ATTEMPT='attempt-001'
npm run test:e2e
```

每次 attempt 使用新名称；不能覆盖数据库密码文件。Private fixture 包含资源身份与
数据库密码文件，保留在安全目录中，供人工复测和失败恢复使用。

真实断言顺序：

1. 浏览器登录与创建项目，max CPU=1、max RAM=1Gi，等待 UI Operation 成功。
2. 从 UI 经 Proxy 创建表、插入并查询实际 PostgreSQL 数据。
3. UI 创建 data-only 子分支，确认没有 Endpoint。
4. UI suspend 父 Writer，减少并发 Compute 占用。
5. UI 单独创建子 Writer，验证父数据继承，再写入子分支。
6. UI suspend 子 Writer，查询父 Writer 触发冷醒，验证子写入不影响父数据。
7. UI suspend 父 Writer，查看监控和 Operation history；保留截图/JSON。
   监控必须显示当前“已休眠”，当前 CPU/RAM 用量为缺测符号，不能把旧 active 样本当作当前资源。
   使用 NEON_E2E_EXPECT_SPLIT=true 时，另检查独立 API/Worker 和 Worker 心跳；
   受控 Worker 故障模式见 [拆分手册](WORKER-SPLIT.md)。
8. 失败时保留 fixture 和 Job/日志，使用其中 Endpoint ID 正常 suspend，
   不能删除数据、Secrets、WAL 或证据。

这些断言覆盖原生资源与分支/连接闭环，**不等于完整权限、自动 idle、
Reader、热伸缩、目录、HA/DR 的全面现场回归**。扩展每项测试时须分别列明结果。

### 2.1 监控状态与采样边界

监控页面同时只读获取 Endpoint 的 Kubernetes runtime 和历史 metrics，均不调用 SQL 唤醒。
当前状态来自 runtime，历史图表保留原样；数据库大小明确标识最后采样时间。
当前资源用量仅使用新鲜、active、同一 Endpoint、晚于当前 VM 创建时间的样本。
休眠、未知、过期、未来时间、缺 VM 身份、旧代次和读取失败均不能显示旧用量为当前值。
运行时读取失败会清除当前观察，保留历史曲线及错误提示。
Node 回归覆盖这些边界，真实浏览器在最终 suspend 后检查状态和资源卡片。

## 3. GitHub trusted live workflow

live-e2e.yml 仅 workflow_dispatch，在 dedicated Linux `neon-e2e` runner 运行。
需配置 protected environment `neon-live-e2e`：
variable `NEON_E2E_BASE_URL` 与 Secret `NEON_E2E_ADMIN_PASSWORD`。
未注册 runner 或未配置凭据时不会自动完成测试。普通 PR 不得获得这类 Secrets。

CI image 构建通过不意味着 live UI 已通过；现场 Job/截图/JSON 是独立证据。

### 3.1 从操作历史恢复真实故障

`web/recovery/recover-operation.spec.ts` 使用显式既有 fixture，
不重新创建失败项目。先修复基础设施，确认原操作为 failed 且 retryable。
有修改权限的用户在“操作记录”打开该 Operation，点击“重试原操作”；
后端仍执行项目权限和状态检查，非 retryable 操作返回 409。

安全目录中的 fixture 合同（密码只存文件，不能填进 JSON）：

```json
{
  "project_id": "<existing-project>",
  "operation_id": "<failed-retryable-operation>",
  "writer_id": "<parent-writer>",
  "child_endpoint_id": "<failed-child-endpoint>",
  "database_password_file": "/secure/e2e/database-password"
}
```

```bash
export NEON_E2E_RECOVERY_FIXTURE=/secure/e2e/recovery-fixture.json
# BASE_URL、ADMIN_PASSWORD_FILE、PRIVATE_DIR 同前；ARTIFACTS 必须是新目录。
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/recovery-001
npm run test:e2e -- --config=playwright.recovery.config.ts
```

此切片要求故障发生在 child Endpoint 创建阶段，父分支已有
`public.ui_release_probe` 表和 `(1, 'parent')` 行、原操作 attempts=1。
断言浏览器刷新后仍能重试、相同 Operation/Endpoint、attempts=2、
子分支继承与写入、父分支冷醒且隔离、两台 Compute 回到 0、监控可见。
复测时根据已发生的写入和 attempts 制定后续断言，不能覆盖原始证据或盲目重跑。

## 4. 扩展验收矩阵

Reader：至少两实例，WAL 可见、拒绝写、独立 Selector/UID、两轮自动 idle/冷醒。
Scaling：运行态 Guest CPU quota/online 与 RAM、负载/SLO、SQL/UID、冷醒边界一致。
Catalog：Writer + 两 Reader，密码轮换正反例、目录删除保护、分支继承、冷醒一致。
Authorization：跨 org/project 404、Viewer/Collaborator、Key scope/expiry/revoke、
成员变更/撤销与并发；不能只测 UI 按钮隐藏。
Idle：长查询/事务/复制活动、新连接竞争、代次计时、休眠监控不唤醒。
Recovery：Worker lease loss、外部阶段故障、幂等重放、节点/API 重启、补偿与恢复。
Production：独立故障域、TLS、DR/PITR、并发 fencing 与持续 SLO，见 PRODUCTION-GATES.md。

## 5. 交付证据

记录 commit、image digest、Helm revision、schema version、测试环境与时间、
资源身份、通过/失败/未测列表及脱敏错误。私密凭据与公开总结分开。
历史报告按其版本保留；新报告不能把旧场景自动登记为本版通过。
