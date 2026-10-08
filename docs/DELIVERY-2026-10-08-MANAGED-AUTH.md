# Managed Auth、正式镜像与环境恢复交付 — 2026-10-08

## 1. 本次实际结果

运行源码固定为 **`2dcd7d2dbac6edac9d7f2190ebf8c67db90ebb97`**，
文档和恢复测试的后续提交不改变这个已验收镜像锁；测试源码 archive SHA
另存于每个 attempt 的报告，镜像、测试与文档版本分别追踪。基础数据面仍为维护者 fork 的
Neon `1f30cd02092dc151f5d00aef97e7c629105b454b`、
autoscaling `c0052f5f2d38fce6c70e448f3f1ee2ee239a0a93`、
PG16 `a42351fcd41ea01edede1daed65f651e838988fc`；本增量未修改这些源码。

独立 Go API、Go Worker、React/TypeScript Console、新增 Better Auth
TypeScript 服务及受限 SQL 身份已集成。应用账号保存在用户分支的
`neon_auth`，与 Console 管理账号分离。父分支身份可被 Timeline 克隆，
子分支重新建立会话和密钥域。标准接口和模型见
[Auth 合同](MANAGED-AUTH.md)、[API](API.md)及 OpenAPI 0.9.0（52 paths / 73 operations）。

| 门槛 | 当前结果 |
| --- | --- |
| 源码 Linux CI | [37781056916](https://github.com/william-lbn/control-plane/actions/runs/37781056916)，五类质量门槛及七镜像发布全通过 |
| Go/真实 PG | 354 pass，0 fail，0 skip；gofmt、vet、race、RLS、接口与迁移合同 |
| Auth | 5 个库/PG/TLS 测试、1 个原生 TLS 测试，锁定依赖审计零漏洞 |
| Web | 13 tests、严格 TypeScript、格式与生产构建 |
| 发布镜像 | API/Web/Gateway/Data API/PostgREST/Adapter/Auth，统一源码；Docker Hub 与公开 GHCR 镜像同 OCI digest |
| Linux 匿名校验 | GHCR 七镜像的 manifest、config、源码标签、许可证和 amd64 平台通过；三节点 21 次真实 containerd 拉取及 CRI digest 检查通过 |
| Helm 本地质量 | 0.1.5，10 Chart lint/render、13 拒绝案例、13 Node 合同测试、10 包生成通过 |
| 现场部署 | 八阶段升级与 manifest/Secret/PVC 审计通过；原完整 values、受保护备份及旧 revision 保留 |
| 新 Auth 真实 UI | **23 checks 全通过**，Job `publication-ui-20261008135401`；项目 `prj_08b7a08145d1c9a5` |
| 历史恢复 UI | 11 checks，`publication-ui-20261008135909`，真实 timestamp/LSN、目录隔离、冷醒与两次 GET 503 |
| 原生 UI | 14 checks，`publication-ui-20261008141717`，独立 Worker epoch 67→68、真实 SQL/分支/目录/监控 |
| Data API UI | 21 checks，`publication-ui-20261008141843`，RLS、密钥轮换、自动缩零/冷醒与请求观察故障 |
| Console 邀请 | 6 checks，`publication-ui-20261008142302`，邀请注册、角色和权限 |
| 应用凭据 | 8 checks，`publication-ui-20261008142320`，复用原生测试项目；不代表真实 AI 推理 |
| 原 Reader 故障恢复 | 5 checks，`publication-ui-20261008144145`，从 UI 重试同 Operation，attempts 2→3，数据/只读/冷醒/缩零 |
| 保留删除与双 Reader | 23 checks，`publication-ui-20261008144311`，双 Reader/WAL/缩零、两轮保留删除/恢复、同 Operation 重试、Data API 与已撤销凭据 |

本次七套主 UI 合计 **106 checks**，另有原 Reader 故障恢复 **5 checks**；
各 suite 结束均观察到 managed VM/Runner 为零。受控恢复注入后 Worker epoch
71→72，原恢复 Operation 在弹窗中重试成功。此注入不是存储灾难或 HA 验收。

仅实验室预览能力通过，不宣称已实现官网全部 Backend 或获得生产认证。

## 2. 从 UI 验证的 Auth 闭环

1. Console 原生创建真实 Neon 项目和有界 1 CPU / 1 GiB Writer。
2. 所有者通过 SQL 工作台委托数据库 CREATE/CONNECT，UI 启用 Auth。
3. 精确 CORS 预检允许，未知来源拒绝；注册后用户及会话真实持久化。
4. 五分钟分支 JWT/JWKS 与 PostgREST/FORCE RLS 联动，其他用户数据不可见。
5. 有活动 Data API 依赖时拒绝停用 Auth；退出撤销会话，密码可重新登录。
6. UI 创建子 Timeline；用户和密码继承，父 cookie、父 JWT 不授权子分支。
   子用户修改不影响父库，反方向 JWT 也被拒绝。负例使用新签发且未过期 JWT。
7. 手动 1→0；CORS 预检不唤醒；应用登录经 Proxy 真实 0→1。
8. Auth 和 Data API 同时启用时，将 idle 策略设为 60 秒，观察自动 1→0，
   再通过应用密码登录冷唤醒。健康检查不访问用户 SQL。
9. 先停 Data API，再停 Auth，公共入口关闭；重新启用新 generation 保留用户。
10. 最终停用所有本次服务和 Compute，确认 VM 与 Runner 均消失；
    SQL、身份数据、Secret、WAL、Timeline 和私有复测凭据保留。

此测试的父/子 Branch 为 `br_08b7a08145d1c9a5`、`br_a013ea4be0b0bfc8`。
管理员用户列表、截图和报告不包含密码、cookie、JWT 或签名私钥。
原始 Playwright、Operation 和资源 UID 回执由运营方存于受保护证据目录。

## 3. 失败诊断与修复依据

* 候选 `f207860` 的真实 UI 注册暴露自签名 Proxy CA 未被 Node 驱动信任。
  引入公开 CA 投影和显式证书身份；没有关闭 TLS 证书链检查。
* 候选 `64517ab` 的实际 `pg.Client` 仍因名称不匹配失败：`pg` 将
  `ssl.servername` 改写为 TCP Service DNS。新适配器调用 Node 标准
  `checkServerIdentity` 验证明确配置的证书名称，保留 `rejectUnauthorized: true`。
  实际 PostgreSQL STARTTLS/启动协议测试同时拒绝错误名称与未知 CA。
* Better Auth 的标准 Ed25519 JWK 不包含可选 `use`。Data API 修复为允许省略；
  存在时仍须为 `sig`，拒绝 null、空值、错误用途、私钥和越界算法。
* 失败的 UI 测试有有界清理：依赖顺序、ETag/CAS、原幂等键及 Operation
  状态全部记录，只停自身服务和 Endpoint；失败记录及数据不覆盖。

两次失败产品 fixture 已正常停用；不通过清空账号表、修改 Neon 数据面或
跳过实际 UI 断言掩盖失败。修复源码已 DCO 提交并发布上述正式镜像。

## 4. 环境稳定性与清理边界

本次启动后活动基础服务正常。先前事件中 CNI/就绪错误集中于主机重启时段，
后续全命名空间快照没有活动服务故障；这不构成 CNI 长期故障已根治的证明。
三节点测试前各有约 9–10 GiB 可用内存、I/O PSI avg10 为 0；
主节点系统盘约 490 GiB、可用 404 GiB，不能据此把网络拉取失败归为磁盘耗尽。

镜像拉取另有确证的出口问题：Linux 7890→Windows 11121→11119 链路中，
11119 实际 HTTPS 请求成功而 11121 超时。containerd 默认 daemon transfer
也继承原代理，仅清 CLI 环境不能修复它。本次使用已知主机校验的有时限 SSH
转发，串行 `ctr images pull --local` 并核对 CRI；没有跳过 TLS、改全局 hosts、
重启 RKE2、删除缓存或修改基础网络。临时转发结束后关闭；新生产节点仍需可靠出口。

已归档并按 UID/resourceVersion 清理 49 个终态测试 Job 和 5 个独立终态探测 Pod；
新增验收资源按相同原则退休。最终全命名空间快照无活动故障，
没有 Pending/Error/CrashLoopBackOff；只保留 RKE2 的一个已成功 CNI 安装 Job。
三节点 Ready、API/Worker/Web Ready、managed VM/Runner 为零，当前 I/O PSI avg10 为零。终态 Pod 本身不再运行容器，清理主要避免状态噪声；
释放运行容量通过 UI/API 停用自有 Compute/Auth/Data API。没有删除 PVC、WAL、对象、
数据库、角色、Secrets、fixture 或任何测试记录。

两套 PostgreSQL 分别服务 Storage Controller 与控制元数据，不是重复服务。
三个 Safekeeper、节点 Agent 与 controller 副本有各自职责。保留 node-1 Pageserver
与 managed node-2 的不同身份和数据，不能仅按相同镜像去重。旧 proxy-api/
compute-resizer 已在确认无消费者和 static Compute 为零后由 Helm 退役。

### 回归期间再次出现的环境故障

首个 native 回归在创建前因 `local` 组织 50/50 项目配额返回 429，
Endpoint 为 102/200。停止 Compute 不释放逻辑配额。四个已有通过回执、
已经未保护的历史 lifecycle 项目，通过确名、ETag、幂等键的保留删除正常退休，
记录七天恢复窗口，物理 GC 仍为 held；没有提高配额或删除元数据行。

第二个 native 尝试项目 `prj_e219c832676791d0` 在父 Writer 冷醒 SELECT
返回 503。2026-10-08 14:03–14:17 UTC 的三节点日志各有 41/41/44 条
慢 fdatasync，最大值分别为 **21.875 / 30.589 / 26.358 秒**，不是 p99。
Linux I/O PSI some avg10 一度约 70%，三节点均有明显等待；每五秒进程采样
仅有少量 MiB 写入，并无编译/镜像构建并发。同期 Windows 有约 22 GiB
可用内存，所见 VMware 进程写入量不高；这不支持内存耗尽，也不能据此确认
具体 SSD/驱动/虚拟化根因。三个 RKE2 进程各自动重启一次，操作员没有重启它们。

暂停后续 suite、保留失败、采样，再在压力降为零后正常停用失败 fixture 的
Compute。原生全流程后续新 attempt 已通过，Worker epoch 67→68；不是改断言、
自动重放未知 SQL 或抹掉故障。宿主机缺少普通磁盘性能计数器，当前进程仍没有
Windows 管理员令牌，未强启 ETW/UAC或改持久化。**共同存储稳定性根因尚未
修复或认证**，属于明确阻止生产准入的门槛，成功复测不能取消此结论。

### 生命周期 Reader 的原操作恢复

Job `publication-ui-20261008142337` 已通过前六个生命周期检查，在创建首个
Reader 时遇到另一段 I/O 等待（主节点 PSI some avg10 38.66%，仍约 10 GiB
可用内存）。Operation `op_60bb08075764392ec964c906` attempts=2 后终态失败，
资源 `ep_03fd898fb0f2e678` 保留；没有重新创建 Reader 或修改数据库状态。
待三节点压力归零后，通过专用 Linux UI recovery fixture 验证原失败身份和
Writer/Reader 类型，再点击原 Operation 的重试按钮。它在 attempts=3 成功，
原数据和真实只读 SQL 通过，Writer 冷醒后两台 Compute 从 UI 缩零，监控可见。

该恢复新增五项检查，保留原失败和新成功两份记录。首次恢复测试在登录完成前
读取 Operation 导致 401，未发重试请求；测试已增加 UI 登录完成的明确等待。
恢复后的测试项目经正常保留删除腾出逻辑配额，SQL、数据、证据和七天恢复窗口
保留。此后完整生命周期用新 fixture 串行复测并通过 23 项；最终正常保留删除该
已通过的测试项目，继续腾出逻辑配额，原失败不能被后续结果覆盖。

## 5. 其他维护者复测

固定本报告源码和 unified Helm 镜像锁，先按统一仓库的 DEPLOYMENT/UPGRADE
准备外部 Secret、完整 values、备份和公开 CA。现有实验室证书身份是
`lab.neon.local`；新隔离实验室生成证书的身份由部署合同决定，不能猜测或混用。

在 Linux 取得 Chromium/Playwright 依赖，使用受保护的管理员密码文件和新目录：

```bash
export NEON_E2E_BASE_URL=http://192.168.146.100:30788
export NEON_E2E_ADMIN_PASSWORD_FILE=/secure/e2e/admin-password
export NEON_E2E_PRIVATE_DIR=/secure/e2e/auth-attempt-001
export NEON_E2E_ARTIFACTS=/var/lib/neon-evidence/auth-attempt-001
export NEON_E2E_EXPECT_SPLIT=true
npm --prefix web run test:e2e -- managed-auth.spec.ts
```

URL 必须等于配置的 publicOrigin。密码文件由运营方安全注入，不能复制凭据到
命令、Git、artifact 或截图。逐条人工操作、SQL 授权与 API 合同见
[MANAGED-AUTH.md](MANAGED-AUTH.md)；后续六套测试和故障注入见
[TESTING.md](TESTING.md)。串行运行，每套结束等待 VM/Runner 真正为零。
读失败可继续观察原 Operation，未知写结果不得盲目重新创建资源。

## 6. 仍然独立的实现与准入

**尚未实现** Functions、客户产品 Object Storage 和真实 AI 推理服务；
底层 MinIO、应用凭据验证不能代替这些产品。模型凭据、SMTP、OAuth 接入后置。
Auth 邮件验证/找回、SSO/MFA、完整用户管理、动态 JWKS 轮换仍需实现。

原地 PITR/完整 Backend 一致性恢复、物理 GC/TTL/失败创建和独立 Endpoint 删除、
小数 CPU、完整内存归还、长事务/短连接竞争矩阵、外部跨实例栅栏、完整多租户
对抗、HA/DR、长期 SLO/告警及全链路可信 TLS 仍未获得独立准入。
当前依赖 singleton/local-path、HTTP Console 和实验室信任配置。
按照 [逐项实施计划](PRODUCT-COMPLETION-PLAN.md)继续逐个闭环，不虚标可用。


## 7. 不可变发布与最终核验回执

* 恢复测试和本报告已推送源码 `599afb1b76e1c304a1ba370aee396a329002c003`；
  [Linux CI 37796055203](https://github.com/william-lbn/control-plane/actions/runs/37796055203)
  五类门槛及七镜像发布全部成功。此提交只增加恢复测试/文档，当前已验收的
  运行镜像继续使用 `2dcd7d2` 的原 digest。
* 统一 Helm 的不可变 `v0.1.5` 对应
  `7a6c8ef5f8b9effec16af3a5d23dd83b520db5cd`，
  [发布 CI 37796404838](https://github.com/william-lbn/neon-helm/actions/runs/37796404838)
  成功；[发布资产](https://github.com/william-lbn/neon-helm/releases/tag/v0.1.5)
  包含十个 `.tgz`、index、SHA256SUMS 和来源锁。
* Linux 使用无凭据 Git clone 该精确 Helm 提交，源合同和八个安装 release 的
  manifest/image/PVC/Secret 审计全部通过；再次确认 managed VM/Runner 为零。
* Linux 从标准 Helm repo 匿名拉取十包及 index，所有 SHA256SUMS 校验通过。
  用户不需要 GitHub 或镜像发布 Token 才能下载此版本。
* 三节点临时 loopback 18790 转发已关闭；两个本次生命周期故障控制器已退出，
  Worker 正常。未遗留后台 operator、测试浏览器或本地 8787/8788 旧服务。

上述后续回执不移动 `v0.1.5` 标签，也不改变已验证的运行镜像锁。
原始证据、失败、备份和私有凭据由部署运营方保留；公开文档只登记脱敏结论。


## 8. CI 交接测试的确定性修正

最终纯文档提交触发的 [37797382865](https://github.com/william-lbn/control-plane/actions/runs/37797382865)
在 `TestControllerLeadershipIntegration` 的即时 takeover 断言失败。
`pgx v5.9.2` close 返回和服务端 advisory lock 释放之间存在异步窗口，
旧测试把客户端关闭当作服务端锁已同步释放。生产 Worker 已对未取得锁的情况
保持 standby 并重试；此次仅修改测试同步，没有改变租约、超时或运行业务代码。

测试记录前任 PID，并有界观察 `pg_locks` 中原会话锁的释放，再执行原来的
successor epoch、旧心跳/旧代次拒绝及失联接管断言。Linux 全量 354 Go 检查
再次通过，十个独立 schema 连续交接的 60 检查通过，均零失败/跳过；见
[交接复测步骤](WORKER-SPLIT.md#52-postgresql-会话锁释放的测试同步)。
该失败日志、GitHub artifact SHA256 与新成功报告均保留；新增临时 CI Job
也已归档并按 UID/resourceVersion 退休，累计 49 Job、5 独立探测 Pod。
不可变 Helm 0.1.5 和已验收运行源码 `2dcd7d2` 继续保留，不因测试修正重部署。
