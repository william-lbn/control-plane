# 测试与交付标准

当前新功能与原 fixture 续测入口见 [Auth/S3 交付](DELIVERY-2026-10-14-AUTH-S3.md)
和 [Functions HTTP](DELIVERY-2026-10-14-FUNCTIONS-HTTP.md)。所有业务/浏览器测试在 Linux，
单集群串行、retries=0。新套件 selector/action/response 等待须单独有界；
不能以全套 30 分钟冷醒预算等待一个已改名按钮。已通过 checkpoint 保留后只续验未通过步骤。
API.md 由 `node tools/generate-api-docs.mjs` 生成，Linux CI 使用 `--check` 检查同步。


独立 Compute 删除增量使用 `web/e2e/endpoint-deletion.spec.ts`，手动步骤与实际 API
合同见 [ENDPOINT-DELETION.md](ENDPOINT-DELETION.md)。从登录/点击验证丢失 202 后
的原请求重放、服务依赖、Writer/Reader 连续性、完全无 Compute 后重建和项目恢复。

新增局部验收：应用 TOTP `auth-totp.spec.ts`、项目共享 `shared-queries.spec.ts`、个人执行历史 `query-history.spec.ts`。完整运行边界和原检查点续测见对应交付报告；不重复已验收旧套件。

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
| Image | protected main/tag after gates | 固定 builder、SHA tag、API/Web/Gateway/DataAPI/PostgREST/Adapter/Auth/AI 八镜像、SBOM/provenance、digest receipt |

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

若原 Auth 测试在子分支创建后观察失败，但原 Operation 已实际 succeeded，可以恢复原
fixture。先用只读 API 核对原项目名称/managed source、根/子分支、Writer 归属及原
Operation，不重新创建项目或子分支。将以下完整身份写到 mode-0600 私密 JSON：
`project_id`、`project_name`、`branch_id`、`writer_id`、`child_id`、`child_writer_id`、
`child_name`、`branch_operation_id`、`original_failure_job`、`database_password_file`、
`application_password_file`。最后两个字段指向原测试保存的 mode-0600 密码文件，不能
把密码值写到文档、日志或 JSON 回执。原 job 必须为 `publication-ui-{14位数字}`，
项目名 `ci-{原job}-auth`、子分支名 `child-{原job}-auth`；不要手工改变名称掩盖归属。

在 Linux 使用新的私密目录、证据目录和 attempt，保留原失败，然后执行：

```bash
export NEON_E2E_AUTH_RESUME_FIXTURE=/secure/e2e/original-auth-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/auth-recovery-001
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/auth-recovery-001
export NEON_E2E_ATTEMPT=auth_recovery_001
npm --prefix web run test:e2e -- managed-auth.spec.ts
```

该路径从 React 操作详情重新观察原已成功 Operation，再在 UI 按实际 disabled 状态
重新启用原分支 Auth、登录原账号并查询原 RLS 行；随后验证子分支账号隔离、JWT 边界、
手动零/冷醒/自动零、禁用/重新启用和原账号保留。恢复结果明确
`creates_new_project=false`；不能当作本版本新注册/新分支创建验收。原失败回执始终为 fail。
新建路径会在等待创建窗口关闭之前保存 child Writer 身份，异常退出也能正常缩零该
精确自有 Endpoint。逻辑退休仅在完整通过且实际 VM/Runner 为 0 后执行，数据继续保留。

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

### 恢复已失败的对象存储验收

`object-storage-recovery.spec.ts` 专门恢复 2026-10-09 的已保留故障项目。
它要求 `NEON_E2E_STORAGE_RECOVERY_FIXTURE` 指向受保护 JSON，包含原始
`project_id`、`project_name`、`branch_id`、`child_id`、`child_name`、
`writer_id`、`child_writer_id` 和 `original_failure_job`。实际字段以测试开头的
fixture 类型及其严格校验为准，不能用新项目代替原失败身份。
恢复会检查原父/子文件、安全头、保留删除/恢复和再次启用，最后保留数据并释放
测试运行资源与逻辑配额。该用例绑定已发生的原始写入；其他故障必须按各自
私有 fixture 和当前状态另写显式恢复断言，不能盲目套用。

```bash
export NEON_E2E_STORAGE_RECOVERY_FIXTURE=/secure/e2e/original-storage-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/storage-recovery-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/storage-recovery-unique
npm --prefix web run test:e2e -- object-storage-recovery.spec.ts
```

管理员文件和 BASE_URL 沿用前述安全输入；公开 artifact 不包含 fixture、密码或
下载 token。失败和恢复各保留一份回执。受信 Linux 运维人员还须运行统一 Helm
仓库的 `tools/verify-product-storage-access.mjs`，确认产品 bucket 可列举、数据库
bucket 明确 AccessDenied；错误网络连接不能算 IAM 隔离通过。

### 当前发行的完整串行回归

按最新交付报告的源码与镜像锁，依次运行 object-storage、native-product、
backend-credentials、data-api、restore、lifecycle、console-invitations 和
managed-auth。凭据测试指定已验收且未删除的测试项目，不再创建 Compute。
每组完成都核对 VM/runner 为零、停用本组服务，并保存原 Operation。
只有确名、具有通过回执的自有项目才可执行 CAS 保留删除以释放 50 项目配额；
七天 UI 恢复窗口与永久保留物理数据是不同合同。绝不通过增加配额、清库或
删除对象/WAL/Secrets 来使回归通过。测试门槛与生产门槛分别记录。

### Data API 原失败项目恢复

`data-api-recovery.spec.ts` 只接收 `NEON_E2E_DATA_API_RECOVERY_FIXTURE` 指定的
既有失败项目，JSON 字段为 project_id、project_name、branch_id、writer_id、
original_failure_job、database_password_file。名字必须与原失败 Job 匹配，原分支
须只有该 Writer，服务须为 active，原 `app_data.notes` 的三条已验证行必须存在。
数据库密码保存在原私有文件，不能写入 JSON 明文、环境变量或公开 artifact。

此场景原签名私钥只在已退出的测试进程内存中，故恢复**显式从 UI 停用/重新启用
并轮换测试 JWKS**，不是找回旧签名密钥。先用原 SQL 凭据确认数据，再检查真实
RLS 请求、手动/自动零后的冷醒，最后从 UI 停用服务、缩零和保留删除以释放配额。
所有原 SQL/RLS、WAL、Secrets、失败证据仍保留。其他失败阶段不能直接套用。

```bash
export NEON_E2E_DATA_API_RECOVERY_FIXTURE=/secure/e2e/original-data-api-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/data-api-recovery-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/data-api-recovery-unique
npm --prefix web run test:e2e -- data-api-recovery.spec.ts
```

不要并发运行另一 suite；先确认环境压力恢复。恢复五项通过仍不改写原失败，
新的完整 data-api.spec.ts 需要另一个 attempt/项目和独立回执。

### 恢复目录表单阶段失败的历史恢复测试

`restore.spec.ts` 支持受保护的 `NEON_E2E_RESTORE_RECOVERY_FIXTURE`：
project_id、project_name、branch_id、writer_id、original_failure_job 和
database_password_file。它只接受原 `restore-publication-ui-*` 项目仍 ready、
仅一个原分支/Writer 的阶段；已创建恢复分支的失败不能套用这个模式。
使用原密码，创建新唯一表/角色/数据库与**新的历史目标点**，不修改或清空原测试
表、不声称重新取得丢失的历史 LSN。新名字/时间戳/LSN 及时保存在新私有 fixture，
原失败和旧数据完整保留。继续完整历史点与目录隔离、冷醒和幂等断言。

```bash
export NEON_E2E_RESTORE_RECOVERY_FIXTURE=/secure/e2e/original-restore-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/restore-recovery-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/restore-recovery-unique
npm --prefix web run test:e2e -- restore.spec.ts
```

测试显式延迟角色成功后的只读目录刷新，确认用户同时填入的数据库名称仍保留。
该测试暴露的实际 UI 缺陷修复只清理已提交表单，不清理其他表单或较新的编辑。

### 恢复 Object Storage 冷醒/子分支创建阶段的原测试

`object-storage.spec.ts` 支持 `NEON_E2E_STORAGE_RESUME_FIXTURE`。先归档原失败，
确认原 Job/Pod 已停止；不能在它仍写入时另启 suite。该模式只复用下列阶段的明确
身份：原私有 bucket/file 已成功验证，原 Writer suspend 为 failed/retryable；可选
带上已经受理后失败的唯一子分支创建。它不重新创建项目或覆盖父文件。

受保护 JSON 输入包含：project_id、branch_id、writer_id、project_name、
original_failure_job、failed_suspend_operation_id 和 database_password_file。
project_name 必须精确等于 `ci-{original_failure_job}-storage`；密码只能从原私有文件
读取。若已有 child，必须同时提供 child_id、child_writer_id、child_name 和
failed_branch_operation_id。子分支原 parent/name/Writer 和 Operation action/resource
在任何重试前核验；不能省略这些字段来创建第二个 child。已经成功的操作不再次重试。

该路径从 Console 登录开始，在 Operation UI 重试相同 ID，保留新 attempts。
若先前失败清理已禁用 Storage，则从 UI 重新启用，验证原对象仍存在，再缩零/冷醒。
子分支失败沿用原 create_branch Operation；继续验证父子原字节、条件写入、下载
token 隔离、禁用/重启、独立删除和保留项目退休。失败立即保留终态/原身份；受理的
Operation 在等待弹窗前写入结果。清理请求的 500/502/503/504 或超时属于结果未知，
不能标记为“未受理”，必须用记录的幂等键/Operation 只读查证。

```bash
export NEON_E2E_STORAGE_RESUME_FIXTURE=/secure/e2e/original-storage-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/storage-resume-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/storage-resume-unique
npm --prefix web run test:e2e -- object-storage.spec.ts
```

如运营方临时暂停了该失败 fixture 的 Proxy 入口，必须先按保存的 Secret UID 和
resourceVersion 恢复**此 selector 的准确原 entry**并保留其他 routes；不要整体回滚
一份旧 Secret 覆盖其他客户的新配置。原 entry 可能没有 `state` 字段，不能把“恢复”
理解为强制写 active。恢复后的新测试也有独立结果；历史失败不改成通过。

### 已受理并恢复成功的 timestamp 分支继续验收

`restore-resume.spec.ts` 仅接受 `NEON_E2E_RESTORE_RESUME_FIXTURE` 指向的原受保护
fixture。原失败 Job 已终止，原 timestamp Operation 已通过 UI 重试至 succeeded，
原 VM 正常恢复后才能使用。该模式保留原 project/timestamp branch/Operation 与历史点，
验证原真实数据及目录隔离，再串行验证写入隔离、原恢复 Writer 1→0→1、UI 创建新的
LSN 分支、其真实历史数据及冷醒、原 LSN 请求的幂等重放、窗口外拒绝、历史页与监控。
它不重复创建 timestamp 分支，不声称原失败变为首次成功；不是原地 PITR。

JSON 字段：project_id、project_name、operation_id、writer_id、child_branch_id、
child_endpoint_id、original_failure_job、names（receipt/laterTable/laterRole/laterDatabase）、
sourcePoint（timestamp/lsn）、resolved_parent_lsn、database_password_file。项目名须为
`restore-{original_failure_job}`；resolved_parent_lsn 是原 timestamp admission 固定的
精确 LSN，可能与 sourcePoint.lsn 不同。不要重新计算或猜测这些字段。

fixture 密码来自原 Linux 私有文件；JSON 不存密码值。已有 receipt 中原 child 的 id=1
为 before、source id=1 为 after，且 child 尚未插入 id=2。其他失败阶段不能套用。
每次新 attempt 单独保存受保护的 accepted LSN 原请求/幂等键，在等待创建窗口关闭前
保存 branch/endpoint/Operation 身份；不会把包含密码的请求体上传为公开 evidence。

```bash
export NEON_E2E_RESTORE_RESUME_FIXTURE=/secure/e2e/original-restored-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/restore-resume-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/restore-resume-unique
export NEON_E2E_ATTEMPT=publication-ui-20261011000000 # 替换为本次唯一身份
npm --prefix web run test:e2e -- restore-resume.spec.ts
```

每阶段先正常缩零上一 Writer，再进行下一项，不并行启动三个恢复 Compute。结束还需
实际 Kubernetes 确认原 VM/Runner 全部消失，才可正常保留删除该测试项目释放配额。
原失败、恢复与续测分别出具回执；只统计实际通过的检查，不合并不同版本的数字。

### 原 Reader 删除成功后的 Compute 生命周期续测

`endpoint-deletion.spec.ts` 支持受保护的 `NEON_E2E_ENDPOINT_RESUME_FIXTURE`。
仅适用于第一 Reader 已经正常删除、原 Writer 与第二 Reader 仍保留，原 SQL 表
`public.endpoint_retention_probe` 只有 `(1,'retained')` 的阶段。原 Job 必须已终止。
后续阶段失败不能重新使用此前 fixture，也不能创建替代项目冒充原失败恢复。

JSON 字段：project_id、project_name、branch_id、writer_id、deleted_reader_id、
surviving_reader_id、delete_operation_id、original_failure_job、cleanup_operation_ids、
database_password_file。名字必须为 `ci-{original_failure_job}-epdelete`；三个 Endpoint
身份必须不同。密码从原私有文件读取。先确认原清理 Operation 全部 succeeded、
Storage disabled、删除 Reader 的四个步骤 succeeded/404/held tombstone，以及
原 Writer/保留 Reader 的精确数据，再继续实际 UI 服务依赖保护和删除流程。

续测涵盖 Writer 正常退休、原密码重建新 selector、Reader 跟随新 WAL、Storage 绑定
替换 Writer、独立 Reader 退休、无 Compute 界面、重新创建后的数据、项目恢复排除
独立删除的 Endpoint，最后正常保留删除并核对四份 held Endpoint tombstone。
依赖错误校验实际 409/code 和 UI 呈现的服务端 message，不把旧英语提示当作 API 合同。

```bash
export NEON_E2E_ENDPOINT_RESUME_FIXTURE=/secure/e2e/original-endpoint-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/endpoint-resume-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/endpoint-resume-unique
npm --prefix web run test:e2e -- endpoint-deletion.spec.ts
```

这是原 fixture 的续测范围，原首次失败保持失败。通过回执必须额外核对实际 VM 和
运行 Runner 为零；受理清理的 202 不构成已经缩零或已经释放资源的证据。

### 原生命周期测试在 Writer SQL 回复未知后继续

`lifecycle.spec.ts` 的 `NEON_E2E_LIFECYCLE_RESUME_FIXTURE` 默认适用于已删除 parent/leaf、
解除项目/main 保护、创建两个真实 Reader 并分别缩零后，Writer 插入 id=2 的回复失败
阶段。JSON 字段：project_id、project_name、branch_id、writer_id、reader_ids（恰好两个）、
deleted_branch_ids（恰好两个）、original_failure_job、database_password_file。名字必须
为 `ci-lifecycle-{original_failure_job}`。只能使用已停止的原 Job 和保留的原密码。

续测先确认原项目 ready、唯一 live root、两份 held branch tombstone，以及原 Writer
和两个原 Reader 的类型/归属/休眠状态。随后 UI SELECT 原表：只有 id=1 时执行一次
插入；已含准确 id=1/id=2 则继续，不重放 INSERT。其他行/值直接失败，不清表或覆盖。
继续两个 Reader 冷醒/WAL、项目删除/恢复/原身份数据、Data API 真实 RLS、服务关闭、
撤销凭据不复活和显式重新启用。该模式不重新执行先前十项新建/保护检查。

```bash
export NEON_E2E_LIFECYCLE_RESUME_FIXTURE=/secure/e2e/original-lifecycle-fixture.json
export NEON_E2E_PRIVATE_DIR=/secure/e2e/lifecycle-resume-unique
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/lifecycle-resume-unique
npm --prefix web run test:e2e -- lifecycle.spec.ts
```

新回执单独记录恢复范围与原 Job，不将原 503 改成通过。所有服务停用、VM/Runner
实际归零后，才可按原项目名称、实际版本和通过证据进行正常保留删除释放测试配额。

### 原叶 Endpoint 创建已受理，但 UI 等待失败

同一 lifecycle suite 支持显式 `stage=leaf_endpoint_accepted`。原 Job 必须停止，
原项目/main 仍 protected，parent/leaf 仍 ready 且无保护；保留三个 branch 的
精确 parent 关系。JSON 使用上述原项目字段，`reader_ids=[]`、
`deleted_branch_ids=[]`，另加 parent_branch_id、leaf_branch_id、leaf_endpoint_id、
operation_id。原密码文件只放在受保护目录，不上传公共 Git。

先按原 project/name/branch/Endpoint/Operation 核验。原 create_endpoint 失败时
仅从 UI 操作记录重试同一 retryable Operation；已经 succeeded 时仅观察。
不重新创建 project/branch/Endpoint/table，不重放原 INSERT。核验原 Writer
精确 `(1,'retained-parent')` 后继续叶数据继承、叶/父保留删除与幂等回放，再通过
UI 串行创建两个 Reader 并验证只读、缩零、WAL 和后续完整生命周期。

该阶段不可用于 parent/leaf 已删除、已有 Reader、原表已出现其它行或后续步骤
未知的项目。每次使用新 evidence/private directory；失败继续保存原资源和
Operation，不能复用此早期 fixture 跳过对新现场的检查。测试运行时为明确恢复范围，
不等同于原正常 suite 首次通过或 API 故障期间的连续可用性。

如果明确续测已完成叶/父保留删除，失败发生在 Reader 创建阶段，重新核验现场后
使用 `stage=readers_in_progress`。项目和 main 必须已解除保护、唯一 live root，
`deleted_branch_ids` 对应两份 held branch tombstone；`reader_ids` 为一或两个
已经受理的原 Reader（不得遗漏），`reader_operation_ids` 按相同顺序记录精确
create_endpoint Operation，另记 `continuation_failure_job`。项目名称仍绑定最初
original_failure_job；保留整个失败链，不把后续 Job 改成原项目创建 Job。

对已受理的 Reader，续测核验类型/branch/Operation，failed 时仅从 UI 重试原 ID，
succeeded 时仅观察；只有经完整 Endpoint 目录证明不存在的第二个 Reader 才通过
UI 新建。两个 Reader 串行验证只读 SQL、拒写和独立缩零，再继续完整生命周期。
原 Writer 表在此阶段必须只有准确 id=1，禁止清表、重建或重复插入来满足检查。

明确恢复模式可在 503/504 后，最多重做一次固定白名单的只读 UI SQL 观察，并记录
Endpoint 和 HTTP 状态；正常首次验收仍只有一次。白名单只包括此测试的具体
catalog/原表读取，不接受任意 SELECT、调用任意 function 或重放任何 DML。
所有失败和恢复 Job 分别保留，该恢复结果不能证明超时窗口内业务连续可用。

## Auth SMTP verification and recovery

See [the branch email/recovery contract](AUTH-EMAIL-RECOVERY.md). The protected Linux workflow advertises `auth-mail.spec.ts`, requires both `NEON_E2E_SQL_SCRIPT_FIXTURE` and `NEON_E2E_MAILBOX_FIXTURE`, and never uploads private mailbox bodies. Deploy the exact runtime images and operator mail configuration before this suite. The fixture receiver is optional, acknowledged, authenticated, internal and persistent. A mail submission response does not prove delivery or verified identity; acceptance reads the actual message and uses the actual UI account.

## Email OTP candidate

`auth-otp.spec.ts` uses the protected owner/mailbox fixtures and actual SMTP. Deploy the version declaring `ManagedAuthSpec.email_otp_enabled` before running it. It verifies native passwordless signup, incorrect/replayed codes, verification of a password user, and an independently captured server cookie before/after OTP recovery. Normal cleanup retains data/mail and retires owned Auth/Compute. See [the OTP contract](AUTH-EMAIL-OTP.md).

## Bounded S3 multipart

`storage-multipart.spec.ts` verifies the original UI upload across interruption/reload, final bytes, seven real signed MinIO SDK operations, abort, credential revocation and Compute zero. It requires the retained SQL owner fixture, protected admin file/private directory and a Linux `storage-sdk-probe` binary built from the candidate snapshot. Reuse the original checkpoint after any failure; never create replacement fixtures to hide an unknown mutation. See [the full contract and manual steps](OBJECT-STORAGE-MULTIPART.md). New `TestMultipart*` uses disposable `control_ci` only; the actual SDK signature test is `TestSignatureActualSDKMultipartSevenOperations`. Old accepted UI suites are not repeated by this slice.
