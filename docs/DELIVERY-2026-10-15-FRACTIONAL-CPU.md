# 2026-10-15：小数 CPU 与实际 runner 配额本地交付

## 结论与版本

实际 React UI / Go Worker / NeonVM / agent / runner / Proxy SQL 已完成 **250m→500m→250m**、逻辑内存 **1Gi→2Gi→1Gi**、Reader 冷醒及最终缩零。保留原测试身份与失败记录，未通过修改数据库目标状态伪造成功。详细合同见 [FRACTIONAL-CPU](FRACTIONAL-CPU.md)，实际 fork 缺陷及修复见 [源码核验](RUNNER-CPU-QUOTA-FIX.md)。

- API 0.35.1：85 paths / 124 operations；migration 032；Application Auth 0.25.1。
- Control Plane Chart 0.9.20，Helm revision 87；neonvm release revision 53。
- API 源 ca498140c1423c9136df2f409a8c960a0d97763a0a50f331b7e85a9839f19e25，镜像 sha256:646feec64e853628ccdefb3197597dd6ac6c645ded42830063ce1262a21bbb9e。
- Web 源 f910c17dce241d232d3f8556c7e01ad5150a75a7d00ff81575f3e1ecb9e15b49，镜像 sha256:db24af9f744c8eb3e711b00bd55654d405c89627f688cd2ec75a343c7f6f1ae6。
- Auth 继续沿用已经验收的 967e1b3 源与原 digest；其他组件有各自保留的来源，没有声称全部重建。
- runner 使用自有匹配 fork 修复 1720924，digest b636612…，完整值见源码核验文档。当前均为 Linux 本地发行；尚未发布远端统一 Helm 或镜像。

## 新增检查与真实边界

定向 Go/临时 PostgreSQL：15 项受影响 bounds 测试通过；另一个新测试验证 fractional runner 的 owner、command 与 RW cgroup 挂载准入。runner 单个定向 race 测试、vet/模块校验/静态二进制构建通过。TypeScript、Vite、Helm 和新 observer 语法通过；未重跑已验收的旧全量 Go/UI。

Linux UI Job publication-ui-20261015135047 接续原 Endpoint ep_0c0e38aedc1d423c，原创建 operation op_b1cdbccaeca325b8a6e3c012。它在修复前正确暴露 dummy cgroup，不因失败另外换一套身份。原创建和新操作 op_7ea93c1e0f34688123fe7793、op_3b389c6027aae70533c07e52 均保留。

| 观察阶段 | 实际逻辑 CPU | VM 内存 status | runner CPU | QEMU cpu.max | QEMU memory.current |
| --- | --- | --- | --- | --- | --- |
| initial | 250m | 1024MiB | 250m | 100000 / 100000 | 503824384 bytes |
| raised | 500m | 2048MiB | 500m | 200000 / 100000 | 521064448 bytes |
| returned | 250m | 1024MiB | 250m | 100000 / 100000 | 523792384 bytes |

同一 VM UID aef4ac4d-801d-48cf-9d26-f370bf140e54、Pod UID dad0f49b-a3b6-4e93-a83a-4631d2f31aad 的三阶段采样通过。UI suite 的六项检查包括原生创建、实际 quota/只读 SQL、升配、空闲缩回/SQL、保留边界和身份的冷醒、最终正常停止。冷醒后再停止，最终托管 Compute Pod/VM 均为空。角色、SQL 数据、元数据、PVC 和观察记录未清除。

**这不是严格 Guest 0.25 核隔离证明。** 当前 runner 保留四倍预算。内存 status 缩回也不代表物理 RSS 归还；表中的 RSS 没有证明该项。严格隔离/计量/争用、完整物理 RAM 回收、跨实例栅栏、HA/DR/TLS 继续单独验收。

## 历史失败与部署保留

首次 UI 按钮名称不匹配、后续 label 精确匹配错误均为测试定位问题，保留失败回执并做 test-only 修复，没有重建运行组件。CRD 字段外观曾被怀疑不支持小数，实际整核/250m 服务端 dry-run 均通过；真正缺口为运行开关与动态 setter，按匹配源码与运行证据修复。

控制面升级前备份 SHA256 3d05aa10de739e66b3bc3c5110a369b0b07173063bce59d0fa79707545576044；9 个 PVC、777 个既有非 Helm Secret UID 及其他 6 个 neon release 保持。neonvm 独立升级保留证据见源码核验文档。无公网凭据、无远程 CI、无硬件压力测试；所有功能验证串行执行。

维护者原证据：control-plane/production/evidence/20261014-auth-mail/fractional-* 和 runner-quota-*。公开仓库只收录无敏感值的合同、代码与总结，不上传私有 fixture、密码或 Python 编排器。

## 新建 Worker 准入补充验收

Linux UI Job publication-ui-20261015140122 另外创建 Reader ep_9328b569c5e6ae0b，operation op_5a309362ae4e4ea4de2dbd49。通过新 Go Worker 的实际 owner/command/RW cgroup 检查后 Operation 成功，再用同一原数据库/角色通过真实 Proxy 只读 SQL，最后从 UI 正常缩零。三项新检查通过，未重复配额升降循环；结束时托管 VM/Pod 为空，数据与回执保留。对应新 suite 为 fractional-admission.spec.ts。
