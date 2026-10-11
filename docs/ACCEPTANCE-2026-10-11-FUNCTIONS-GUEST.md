# Functions 默认启动与实例隔离验收

## 结论

2026-10-11，源码 `86a9e07a0620b11568ab453effc7d5f3a23a3650` 的自有 Functions
镜像在三台 Linux 节点核验摘要后，默认 entry 在真实 NeonVM 内通过 32 项检查。
没有注入 sleep 或替换启动程序。机器资源有限，测试串行执行。
[机器可读回执](acceptance/2026-10-11-functions-guest.json)不含凭据。

这完成了 guest 默认启动与执行隔离切片；**没有完成 Functions 控制面产品闭环或生产准入**。
scope 为独立测试身份；SQL 项只是受允许端口的 TCP 连通，未验证数据库登录或数据读写。
当前 deployed API/Worker/Web 仍是 `0ba1732`，没有自动部署本次新 migration 或启用服务。

## 验证内容

| 类别 | 真实检查 |
| --- | --- |
| 启动与通信 | DHCP/default route 就绪后获取 TLS artifact、精确 scope、Node 24 Fetch、HTTP、连续 SSE |
| 请求身份 | 未签名、旧 boot、旧 generation、错误 branch、重复 nonce 均拒绝 |
| 进程权限 | UID 65532、no-new-privileges、零 capabilities、不可迁移 cgroup、不可写 bundle |
| 凭据边界 | 不可读 bootstrap/root log/platform key/raw block；公共 CA 与 scratch 可用 |
| 网络 | 精确 SQL/DNS 正例，artifact peer、Kubernetes、宿主 SSH、metadata、IPv6 负例 |
| 回收 | detached child 实际创建；shutdown 结束整棵进程树；普通 VM 删除后 owned Runner 为 0 |

镜像：`docker.io/williamluckyli/control-functions-vm@sha256:c4b5d20b248e0d3d0951f2c3ae04b917092b58c5ba2f146047e99a3828fbbc32`。
公开 Linux quality run `38101501916` 全部 13 作业成功，Go/PG 474 pass / 0 fail / 0 skip；
镜像 run `38101889726` 成功，固定源码、输入 digest、SBOM/provenance 和 loader 检查保留。
carrier SBOM 不能单独证明整个 guest rootfs 漏洞或供应链准入。

## 已修复的问题和记录

1. loader 缺少 `sysctl`：补齐 carrier procps，并在 Linux Docker CI 验证实际 loader。
2. 固定自有 kernel 没有 REJECT target：改用内核内置 DROP；继续逐包阻止私网/平台访问。
3. DHCP 与 supervisor 并行启动：在 root guest 中有界观察 eth0 和 default route，成功后立即继续。
   没有改变主机时间、放宽签名窗口或为客户 POST 增加自动重放。

原失败 VM、root-only 诊断、带延迟 entry 的隔离检查回执保留，均与本次默认启动验收分开。
这些改动只在 control-plane Functions guest；没有修改 Neon/autoscaling/Postgres fork。
测试 VM 按 UID/resourceVersion 正常回收；Secret、artifact、日志与数据未删除。

## 手动复测

使用 [guest 合同](FUNCTIONS-GUEST.md)的固定镜像和独立测试 PKI/Secret，在 Linux
按顺序创建 root-only artifact peer、独立 1 CPU / 2048 MiB / 2Gi rootDisk / no SSH VM，
执行上述 manager/HTTP/SSE/权限/网络/关闭正负例。请求不得打印 manager key 或 bootstrap。
保留 source/image/VM/Pod UID、scope、boot、每项结果及正常退休回执；失败时先观察原实例。
不得通过 force-delete、复用他人 UID、关闭 TLS 或删除原日志使检查通过。

## 下一阶段

Functions SQL 最小权限、持久 artifact/Secret 恢复、leased Driver、OpenAPI、React 部署/调用、
版本失败保留原 active、分支克隆/PITR、关闭/冷醒和独立公网 origin 仍需逐项实现验收。
HA/DR、外部缩零 fencing、全链路可信 TLS 与硬件 SLO 继续是独立门槛。
