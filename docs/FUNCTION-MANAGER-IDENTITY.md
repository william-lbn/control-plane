# Functions 原实例管理证书续期

0.57.0 / Chart 0.9.49 / migration 046 / Helm 130 的原实例自动续期功能已完成本地基础验收，见[精确交付](DELIVERY-2026-10-19-FUNCTION-MANAGER-IDENTITY.md)。migration 046 已应用并冻结。全链路 TLS 等 T01 其他边界仍独立。

Worker 使用原 UID/Service/bootstrap MAC、原实例/boot、TLS 1.3 和 HMAC 验证管理端，在到期前申请新的两小时叶证书和独立密钥。Guest root 只允许固定 CA、原 SAN/scope/boot、旧指纹 CAS、有效期受限及不同公钥的更新，原客户进程和定义/部署均不变化。排空/退休时拒绝更新。每次应用结果用新的 TLS 握手核对精确证书指纹；未知 POST 结果观察原 boot，不盲目重复写入或换实例。

管理私钥只进入受信管理通道和 root 内存；客户 Node 没有新密钥或 CA 签名权限。immutable bootstrap 始终保留原 UID、内容、密钥与 MAC。受信控制面回读原回执时验证原叶在签发窗口的链，Guest 新启动仍严格检查当前有效期，访问原运行实例仍必须通过当前有效 TLS。不存在过期证书连接例外。

`functions.managerCertificateRenewal.enabled` 默认关闭，需要新 Worker、migration 046 和支持身份更新的新 Guest。旧 Guest 缺少能力时不会被推送更新；正常停醒可升级 Guest。`renewBeforeSeconds` 默认 2700，范围 600–7140；本地功能验证可短暂使用 7140，观察自动控制器后恢复 2700，不缩短叶证书有效期或改变时钟。

管理端 `/internal/v1/manager-identity` 为私有 TLS/HMAC 协议，不是客户 API。Console 显式运行观察展示原实例证书到期、指纹和续期次数。后台刷新不执行函数、不查询或唤醒 SQL、不修改公开入口、输出授权或函数版本。元数据只保留原 UID/boot/指纹/有效期和认证观察历史，无密钥。

本片针对两小时后实际管理不可达的业务缺口。全链路 TLS、根 CA 轮换、公网入口、性能、可靠性、HA/DR 和外部 fencing 仍独立。
