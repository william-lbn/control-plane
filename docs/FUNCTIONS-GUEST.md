# Functions guest v1：引导、隔离与镜像构建

## 1. 交付范围和当前门槛

这是 Functions 专用 VM 的 Go supervisor、Node runtime 和 Linux 镜像构建实现。
它是 [Functions 产品计划](FUNCTIONS-IMPLEMENTATION.md) 的 F1 增量。平台
`capabilities.functions` 仍为 false；metadata/leased Driver、UI 和真实 VM
验收未完成前，不允许将这个镜像作为生产 Functions 服务发布。

Linux `functions-foundation-20261010212911` 已编译真实 supervisor 并完成 74 项
Go race/vet 和 9 项 Node HTTP/SSE 检查，0 fail / 0 skip。测试包含实际 TLS
artifact HTTP peer、错误 CA/签名/重放/redirect/hash/overflow 拒绝、严格配置
和并发日志上限、固定 Writer selector、明文/编码 dot segments 和 HTTP backslash
归一化拒绝。它没有执行 guest sysctl、netfilter、cgroup 或 Secret 卸载。
实际 VM 验收须独立记录，镜像 build/CI 成功不能替代这些门槛。

## 2. 实例与引导合同

唯一启动程序为 `api/cmd/function-supervisor`。必须是独立 Linux NeonVM 的 root，
镜像 marker 精确为 `/etc/neon-function-guest` 的
`isolated-neonvm-functions-v1\n`。不可作为 host/共享容器工具执行。

Secret CD-ROM 挂载 `/run/function-bootstrap`，唯一输入 `config.json`，最大
128 KiB，严格 JSON，不允许未知字段。不得通过 VM env、command、Pod annotation
或命令行传密钥。初始化后所有私钥仅保留在 supervisor 内存中。

| JSON 字段 | 类型及限制 | 定义 |
| --- | --- | --- |
| `scope` | `Scope` | instance/deployment/project/branch/slug/generation，见内部 manager v1 |
| `manager_key` | 标准 base64 的 32 bytes | 单实例 manager HMAC key |
| `manager_certificate` / `manager_private_key` | PEM 字符串 | 同一叶证书/私钥；唯一 SAN 为 `{instance_id}.functions.neon.internal`，不能是 CA |
| `manager_ca` | PEM ≤32 KiB | 启动时验证 chain、serverAuth、SAN、期限和私钥匹配；TLS 1.3 |
| `artifact_url` | 单一 HTTPS URL | 精确 path `/internal/v1/function-artifacts/{instance_id}/{deployment_id}/{bundle_digest}`；无 userinfo/query/fragment |
| `artifact_key` | 独立 base64 32 bytes | 不可等于 manager key，只用于本实例/版本的签名获取 |
| `artifact_ca` | PEM ≤32 KiB | 不回退到系统 CA、不使用代理、不跟随 redirect |
| `bundle_digest` | 小写 SHA256 hex | 绑定整个 ZIP 原始 bytes，≤8 MiB，获取后再次 ZIP/CRC 校验 |
| `database_url` | PostgreSQL URL | 用户 `fn_<16hex>`，密码≥24 bytes，精确 Proxy hostname/port；只接受固定 SSL 字段及 Writer options |
| `writer_endpoint_id` | `ep_<16hex>` | 固定 Writer，SQL options 必须精确等于 `endpoint=ep-<16hex>`；Driver 还须校验它属于本 branch |
| `sql_ca` | PEM ≤32 KiB | 公共 SQL trust anchor 写入固定 `/etc/neon-function/sql-ca.crt` |
| `proxy_ip` / `dns_ip` | 显式 IPv4 | Node UID-owner 网络白名单，不接受 loopback/unspecified |
| `proxy_hostname` / `proxy_port` | lowercase DNS / uint16>0 | 固定 `/etc/hosts` 映射；SQL `verify-full` 身份与目标一致 |
| `environment` | 受限 string map | 客户 env；禁止平台保留键，不含 manager/artifact key |
| `allow_public_https` | boolean | 默认 false；启用时仍拒绝私网/metadata/保留地址 |

SQL URL 仅接受 `sslmode=verify-full`、
`sslrootcert=/etc/neon-function/sql-ca.crt` 和一个精确绑定 Writer 的 `options`。
当前 own-fork Proxy 使用该 startup option 路由，不能仅依据一个 TLS 主机名假定
SQL 已绑定分支。这里的 role 命名/URL 校验不能替代
SQL Driver 的实际 NOSUPERUSER/NOCREATEDB/NOCREATEROLE/NOBYPASSRLS/NOINHERIT/
NOREPLICATION、表授权、连接数和超时验收；这些留在 F2/F3。

### 2.1 启动顺序

```mermaid
sequenceDiagram
  participant I as NeonVM init
  participant S as Go root supervisor
  participant A as authorized artifact handler
  participant N as Node UID 65532
  I->>S: immutable entrypoint / Secret CD-ROM
  S->>S: validate scope, keys, manager certificate and SQL URL
  S->>A: TLS 1.3 + signed exact artifact GET
  A-->>S: scoped immutable ZIP
  S->>S: SHA256 + ZIP/CRC + sealed root-owned install
  S->>S: unmount Secret; deny raw block reads; tighten cgroup migration
  S->>S: IPv6 off; UID IPv4 default deny; child cgroup limits
  S->>N: setpriv + cgroup FD; only customer env/SQL identity
  S->>S: serve signed manager over TLS 1.3 :9090
  Note over S,N: Customer stdout bounded to root-owned 1 MiB file
  I->>S: ACPI shutdown hook / SIGINT
  S->>N: SIGINT; <=5s; whole cgroup kill + populated=0
```

`/run/neon-function/supervisor.pid` 必须新建；同一个 guest 不收编旧运行目录或
重新启动客户代码。失败恢复由 Worker 创建新实例/代次，防止在有旧进程、旧
netfilter 或旧 mount 的 guest 中复用状态。控制日志只输出失败阶段，不输出
bootstrap、底层异常、SQL URL、env 值或代码正文。

## 3. 独立 guest 的额外边界

- own-fork `vminit` 给 root `cgroup.procs` group/world write，以支持原 cooperative
  signaling。Functions 在启动 Node 前收回 procs/threads 权限并开启 memory/pids
  controllers；此动作局限于专用 guest，无 upstream fork 修改。
- Node memory.max=1536 MiB、swap=0、pids=64，位于 nominal 2048 MiB VM。UID/GID
  65532、无 supplementary groups、no-new-privileges、所有 capability sets 丢弃。
  `UseCgroupFD` 使它从 fork 起就在受限 cgroup，parent-death SIGKILL 和完整
  cgroup.kill 覆盖 detached 子进程。
- Secret 正常卸载后检查目录为空并封闭为 0700；所有 block devices root:root
  0600，镜像有相应 udev 规则。仅 chmod Secret 文件不足以防止 raw CD-ROM 读。
- 自定义 `inittab` 移除 SSH、自动 root 控制台、vector 和未经此产品授权的
  neonvmd 管理监听；固定 CPU/RAM guest 不依赖这些资源热插拔服务。
- supervisor 截断日志到 1 MiB，0600；不会把客户 stdout 直接发到平台日志。
  这只是 guest-local 日志，持久日志/授权查询尚待 F2/F4，不能声称已交付监控。
- SIGINT 五秒后无论 Node 主进程是否已退出，都检查/结束整个 cgroup。退出后
  抑制 `vmstart` respawn，Worker 才能普通回收该 VM。

这些边界依赖真实 kernel/guest，不依赖普通 CNI policy 宣称额外 VXLAN 网络已
隔离。证书/签名不替代多租户授权、外部栅栏、可信启动或全平台可信 TLS。

## 4. Linux 镜像流水线和输入

`containers/functions.lock.json` 锁定 Node 24.19.0 Debian bookworm-slim、Debian
2026-10-09 snapshot、自有 vm-builder/daemon/kernel digest 和 autoscaling source。
Node base 已通过匿名 registry manifest SHA256 验证。构建只能在 Linux amd64。

`tools/build-functions-vm.sh`：

1. 拒绝与 `FUNCTIONS_SOURCE_COMMIT` 不一致的 checkout 或已有 build receipt。
2. 编译静态 Go supervisor；从固定 Debian snapshot 安装 guest iptables-legacy /
   setpriv / 公共 CA，保留实际 dpkg 版本列表。
3. 通过自有 vm-builder 创建 2G root disk，使用独立 SIGINT shutdown hook。
4. 使用 qemu-img/debugfs 在 CI 离线替换 `/etc/inittab`；逐字比较写入结果并
   执行 e2fsck，拒绝只依据 debugfs 的 exit code 宣称成功。
5. 生成 qcow2 SHA256、guest package/inittab/source receipt，再发布 carrier image。

公开源 `2cd2f804` 的质量 CI `38086954494` 全部通过（454 Go/PostgreSQL，0 fail /
0 skip）。首次 VM pipeline `38087496936` 完成 rootfs、qcow2、inittab compare/
e2fsck，发布 carrier 的 COPY 被默认 artifact 排除规则阻止；失败原日志保留。
已改为显式 `artifacts/functions-vm/carrier` 独立构建上下文，只复制已封存的
disk、公共 package 清单和锁文件，不放宽仓库的 private/artifact 排除规则。
它不是成功发布或真实运行验收。

vm-builder 本身包含自己的 base image/tool 输入；其 digest 与来源锁已保留。
其已有 Vector 下载没有新 checksum gate，最终 Function init 不启动 Vector。
qcow2 内的依赖清单和镜像 SBOM/provenance须共同审阅；carrier SBOM 不能单独
证明整个 guest filesystem 的供应链/漏洞准入已经完成。

```bash
# Linux Docker builder，仅构建，不对 cluster/数据进行变更。
export FUNCTIONS_SOURCE_COMMIT="$(git rev-parse HEAD)"
bash tools/build-functions-vm.sh
```

`.github/workflows/functions-vm.yml` 只接受 main 上**同一 commit 已成功的**
`Linux quality and images` push run，输入其 `quality_run_id`。未满足时拒绝构建/
发布。成功时推送 immutable `sha-{commit}-r{run}-a{attempt}` 到公开 GHCR，配置
已有仓库凭据时同步 Docker Hub `williamluckyli/control-functions-vm`。所有 action
固定 commit；没有在 Windows 或实验室 host 上构建 qcow2。发布 receipt 明确
`microvm_acceptance:false`，真实 VM 测试通过后另出 runtime acceptance lock。

## 5. F1 实际 VM 验收清单（尚未通过）

必须按以下顺序串行执行并保留 Job/VM/Pod UID、精确 image digest、source/boot /
scope、正负检查和正常回收回执：

1. 匿名拉取发布镜像/核验 revision；创建 dedicated 1 CPU / 2048 MiB / no SSH VM，
   一个 bootstrap Secret、一个可信 TLS artifact peer，无 host Docker socket。
2. 同一 instance 的 trusted manager status/invoke、真实 Fetch/连续 SSE；拒绝
   unsigned/replayed/old boot/wrong generation/wrong branch 请求。
3. Node 检查 UID/GID/caps/no-new-privileges、不可写 cgroup ancestor、不可读 Secret
   目录/raw block/platform private keys；可读公共 SQL CA/自身 app env。
4. 实际限定 role 经 TLS Proxy 读写唯一 branch fixture；不能跨 branch/角色升权。
5. Private Kubernetes/metadata/manager/DNS rebinding/IPv6 负例与明确允许的 SQL/DNS
   正例；未校验完不得启用任意公网 egress。
6. detached child/资源超限/崩溃/关闭失败后 retry、ACPI SIGINT 和 cgroup=0；保留
   失败日志、原实例身份和后续恢复。普通 VM 回收后所有 owned Runner absent。

任何项失败都保留原始记录并修正具体问题；仅测试完成的实例/Job 可回收。不要
删除 branch 数据、bootstrap/evidence 文件、artifact/WAL、用户 Secret 或 PVC。
