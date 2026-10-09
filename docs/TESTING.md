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
| Auth | 独立 auth_ci PostgreSQL + Better Auth；原生 Node TLS 测试 | 注册/密码/会话/JWT/克隆身份，严格配置与 cookie、CA/名称负例；零 skipped |
| Image | protected main/tag after gates | 固定 builder、SHA tag、API/Web/Gateway/DataAPI/PostgREST/Adapter/Auth 七镜像、SBOM/provenance、digest receipt |

```bash
export NEON_V2_TEST_DATABASE_URL='postgres://ci:ci-disposable@127.0.0.1:5432/control_ci?sslmode=disable'
export NEON_CI_ATTEMPT="manual_$(date -u +%Y%m%d%H%M%S)"
bash tools/install-postgrest.sh /tmp/neon-postgrest
export NEON_DATA_API_TEST_POSTGREST=/tmp/neon-postgrest/postgrest
make test web
PATH="/tmp/neon-helm:$PATH" make helm
# Auth 使用另一个独立、只供 CI 使用的本机 PostgreSQL：auth_ci 用户/数据库。
export AUTH_TEST_DATABASE_URL='postgres://auth_ci:ci-disposable-password@127.0.0.1:55438/auth_ci'
make auth
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
npm run test:e2e -- native-product.spec.ts
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

## 分支 Managed Auth 真实 UI

完成 [Managed Auth 部署](MANAGED-AUTH.md) 和公开 CA/证书名称设置后，使用相同受保护输入执行 `npm run test:e2e -- managed-auth.spec.ts`。`NEON_E2E_BASE_URL` 必须为部署 `managedAuth.publicOrigin`，不能改为另一个未声明的 Service Origin。该测试从 React 创建项目和 Auth，验证真实 Neon 中的注册、会话撤销、JWT/JWKS、用户继承、子分支隔离、Data API RLS、手动/自动缩零和 Auth 冷唤醒，再停用自己创建的服务和 Compute。重测必须使用新 attempt，失败资源可按保存的 ID/Operation 观察和正常停用；不清除账户或数据。

### 2.0 保护、删除与恢复

项目/分支保护与保留删除：相同 Linux 参数下运行
`npm run test:e2e -- lifecycle.spec.ts`，只删除新建的 `ci-lifecycle-*` 测试资源。
测试确认 root/child 依赖保护、活跃 VM 回收、幂等 replay、七天项目恢复、原 Proxy
凭据冷醒和原数据保留、先前已删除的分支不复活。另创建两个只读 Compute，
验证真实 Replica、写入拒绝、各自 1→0→1、WAL 可见及项目恢复后的原 Selector；
详见 RETAINED-DELETION.md。手动缩零通过不能代替多个 Reader 自动 idle 竞争验收。
所有历史证据和底层数据库/对象/WAL/Secrets 保留，最终 Compute 缩到 0。

### 2.1 Console 邀请与注册

同一组 Linux 浏览器环境变量下，可单独运行
`npm run test:e2e -- console-invitations.spec.ts`。该测试从实际 UI 登录开始，
验证一次性邀请、幂等 replay 不返密钥、受邀注册/登录、跨组织 404、Viewer 无管理权、
已有账号本人接受且不能重置密码、撤销邀请与移除成员立即失权。
只创建元数据账号/组织/邀请，不启动 Compute；资料与凭据 fixture 保留在私有目录。
完整手动步骤、失败语义与生产边界见 CONSOLE-INVITATIONS.md。

这些断言覆盖原生资源与分支/连接闭环，**不等于完整权限、自动 idle、
Reader、热伸缩、目录、HA/DR 的全面现场回归**。扩展每项测试时须分别列明结果。

### 2.2 监控状态与采样边界

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
workflow_dispatch 的 suite 分别选择 native-product、data-api 或 backend-credentials。
凭据切片另需显式指定已有授权测试项目 credential_project，不创建 Compute。

### 3.0 Backend 独立产品切片

Data API：按 [Native Driver](DATA-API-NATIVE-DRIVER.md) 从 UI 创建项目、准备真实 RLS、
启用服务，检查双主体隔离、无效 JWT、伪造/合法写入、幂等、浏览器来源配置保留、停用/重新启用，
以及 Data API 保持运行时的手动缩零、自动空闲缩零和两次首请求冷醒。
运行 `npm run test:e2e -- data-api.spec.ts`；失败记录必须保留，按 fixture 正常停用服务和 suspend Compute。

应用凭据：按 [Credentials](BACKEND-CREDENTIALS.md) 指定 NEON_E2E_CREDENTIAL_PROJECT，
运行 `npm run test:e2e -- backend-credentials.spec.ts`。测试 UI 创建 data-only 子/兄弟分支、
一次性 Token、重放不返回秘密、模型/分支正反例、轮换和撤销、刷新无法找回明文。
Token 只在进程内存，截图遮罩全部秘密输入，完成后仅撤销本测试凭据并保留分支/审计。
凭据授权通过不是真实供应商调用通过；推理服务仍需独立验收。

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

### 已失败的生命周期 Reader 恢复

`recovery/recover-operation.spec.ts` 还支持明确的 `purpose: "lifecycle-reader"`。
这是对已经存在的失败 Operation 的复测，不创建新项目或 Endpoint。
运营方先保留原失败报告、Operation ID、Endpoint ID 和数据库密码私有文件，
确认项目与 Reader 属于该测试、基础服务恢复健康后提供以下 fixture：

```json
{
  "purpose": "lifecycle-reader",
  "project_id": "prj_REPLACE_WITH_RETAINED_ID",
  "operation_id": "op_REPLACE_WITH_FAILED_ID",
  "writer_id": "ep_REPLACE_WITH_WRITER_ID",
  "child_endpoint_id": "ep_REPLACE_WITH_READER_ID",
  "previous_attempts": 2,
  "database_password_file": "/secure/e2e/original-database-password"
}
```

示例 ID 是占位符，必须替换为原报告中的真实 ID；`previous_attempts` 必须与当前
失败记录完全相符。测试先核验 failed/retryable/create_endpoint、原资源和同分支
Writer/Reader 类型，再从 UI 点击“重试原操作”，确认原 ID 和 attempts 增加一。
该 fixture 的原数据库必须含 `public.lifecycle_probe` 的 `(1, 'retained-parent')`。
真实 SQL 确认 Reader 处于 recovery/read-only、Writer 冷醒保留数据，两者从 UI
缩零后监控仍可查看。数据已被后续修改时应设计新的显式断言，不能套用此 fixture。

```bash
export NEON_E2E_RECOVERY_FIXTURE=/secure/e2e/reader-recovery.json
npm --prefix web run test:e2e -- --config=playwright.recovery.config.ts
```

登录完成和受信管理员会话必须先确认，再查询 Operation；恢复测试没有登录 API
绕过。新报告与原失败报告分别保留，已通过恢复不把原失败改成成功。

## 4. 扩展验收矩阵

### 历史分支恢复

运行 `npm run test:e2e -- restore.spec.ts`，保护配置与其他 live UI suite 相同。
先写入并记录 UTC 时间戳/提交 LSN，再写后续数据、角色和数据库，通过“历史恢复”
创建新分支。验证历史内容、当前目录不被投射、分支隔离、SQL 冷醒、幂等重放和
保留窗口外拒绝。`NEON_E2E_POLL_FAULT=true` 验证两次 503 GET 只产生一次恢复 POST。
最终观察所有本次 VM 与 Runner 为 0，保留数据库、凭据、操作与失败记录。
完整步骤和能力边界见 [HISTORICAL-BRANCH-RESTORE.md](HISTORICAL-BRANCH-RESTORE.md)。

### 操作查询故障恢复

设置 `NEON_E2E_POLL_FAULT=true` 后运行 native-product.spec.ts 和 data-api.spec.ts。
两组 Linux UI 分别注入已受理操作的两次 503 读取失败，仍须只有一次 UI 创建/启用请求，
同一个 Operation 完成。只对 GET 观察重试；操作、SQL 或 Data API 应用请求不自动重放。
读超时、权限失效、取消、Retry-After 和卸载语义见 [OPERATION-OBSERVATION.md](OPERATION-OBSERVATION.md)。

Reader：至少两实例，WAL 可见、拒绝写、独立 Selector/UID、两轮自动 idle/冷醒。
Scaling：运行态 Guest CPU quota/online 与 RAM、负载/SLO、SQL/UID、冷醒边界一致。
Catalog：Writer + 两 Reader，密码轮换正反例、目录删除保护、分支继承、冷醒一致。
Authorization：跨 org/project 404、Viewer/Collaborator、Key scope/expiry/revoke、
成员变更/撤销与并发；不能只测 UI 按钮隐藏。
Idle：长查询/事务/复制活动、新连接竞争、代次计时、休眠监控不唤醒。
Recovery：Worker lease loss、外部阶段故障、幂等重放、节点/API 重启、补偿与恢复。
Production：独立故障域、TLS、DR/PITR、并发 fencing 与持续 SLO，见 PRODUCTION-GATES.md。

## 5. 交付证据

### 保留删除的受控恢复重试

正常 `lifecycle.spec.ts` 覆盖项目/叶分支保护和删除、两个 Reader、七天恢复、
Data API 与已撤销凭据。需要在恢复弹窗验证同 Operation 重试时，显式设置
`NEON_E2E_LIFECYCLE_RECOVERY_FAULT=true`。此场景另须受信 operator，不能
将 Kubernetes 或数据库管理凭据提供给浏览器。两进程在 Linux 使用同一私有
fixture 目录（0700，浏览器用户可写）与各自新的 evidence 目录。

```bash
# 受信 operator 终端，运行于已具备授权 KUBECONFIG 的 Linux 主机。
# 明确核对正在使用的集群；不要与其他 live suite 并发。
export KUBECTL_BIN=/path/to/verified/kubectl
export NEON_E2E_KUBE_SERVER=https://YOUR_KUBERNETES_API:6443
node tools/lifecycle-recovery-fault.mjs \
  --fixture-dir /secure/e2e/attempt/private \
  --evidence-dir /secure/e2e/attempt/operator-evidence

# 浏览器终端：BASE_URL、ADMIN_PASSWORD_FILE、PRIVATE_DIR、ARTIFACTS 同前。
export NEON_E2E_LIFECYCLE_RECOVERY_FAULT=true
npm --prefix web run test:e2e -- lifecycle.spec.ts
```

operator 仅接受本次 `ci-lifecycle-*`、已完成删除的 managed 项目；要求队列清空、
单个 Ready Worker，通过 UID/resourceVersion CAS 暂停，确认原 recover Operation
仍 queued 且未持有 lease 后注入 `controlled_test_failure`，再恢复 Worker 并核验
leader epoch 增长。异常/信号退出会恢复原 UID 的 Worker；若无法恢复必须先处理
operator report，不能继续其他 suite。服务/项目名目前明确绑定该实验室部署，
其他命名环境须先调整工具配置并重新验收。SQL 数据、WAL、Secret 和证据不删除。
这是受控 terminal-status 注入，不能标记为存储服务故障、跨实例栅栏或 HA 认证。

记录 commit、image digest、Helm revision、schema version、测试环境与时间、
资源身份、通过/失败/未测列表及脱敏错误。私密凭据与公开总结分开。
历史报告按其版本保留；新报告不能把旧场景自动登记为本版通过。


## Branch Object Storage

Run `web/e2e/object-storage.spec.ts` on the trusted Linux runner with unique protected fixture/evidence directories. Its UI steps and API negative probes are documented in [OBJECT-STORAGE.md](OBJECT-STORAGE.md). Source-only checks do not replace the real Neon+S3 byte/clone/cold-wake receipt.
