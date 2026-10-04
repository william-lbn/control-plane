# Linux 部署与回滚

## 1. 前提与边界

本仓库发布 API、Web、Compute Management Gateway。
API 镜像也包含独立 control-worker 二进制；Chart 0.2.0 默认使用 split profile。
旧版本升级必须先执行 WORKER-SPLIT.md 的停止/排空步骤，不能直接覆盖运行中的合并 Worker。
Neon/Autoscaling 完整基础设施须先安装并验收：CRD/NeonVM controller、Agent/Scheduler、
Storage Controller、Pageserver、Safekeeper、Proxy、持久对象存储和兼容 adapter。
当前依赖外部 adapter，不能只安装本仓库的两个 Chart 就获得完整数据库服务。

要求：Linux amd64、Kubernetes >=1.30、NeonVM 所需虚拟化能力、固定 PG16 guest、
可用 metadata PostgreSQL、可信 SQL/管理证书以及明确的 namespace 与内部 DNS。
基础组件资源必须预留；Computes 按需休眠，不能删除 Secrets、PVC、WAL 或测试数据来释放空间。

## 2. 准备凭据

凭据由运维系统生成并存储在 checkout 外的安全目录。
Secret 名可自定义，以下 key 是当前合同：

| Secret / key | 内容 |
| --- | --- |
| control credentials / database-url | 专用 metadata PG DSN；生产 TLS verify-full，预创建独立 DB/角色 |
| control credentials / admin-password | 首次 admin bootstrap 密码；不在 Helm values 内保存 |
| control credentials / idempotency-key | 至少 32 随机字节的 base64；备份恢复必须保持一致 |
| PG CA Secret / ca.crt | SQL Proxy 信任链；与 hostname 验证匹配 |

```bash
umask 077
install -d -m 0700 /secure/neon-control
openssl rand -base64 48 > /secure/neon-control/admin-password
openssl rand -base64 32 > /secure/neon-control/idempotency-key
# 通过秘密管理器创建 /secure/neon-control/database-url，不把 DSN 写入命令历史。
kubectl -n neon create secret generic neon-control-plane-credentials \
  --from-file=admin-password=/secure/neon-control/admin-password \
  --from-file=idempotency-key=/secure/neon-control/idempotency-key \
  --from-file=database-url=/secure/neon-control/database-url
kubectl -n neon create secret generic neon-postgres-ca \
  --from-file=ca.crt=/secure/neon-control/pg-ca.crt
```

不要覆盖已有 bootstrap/幂等密钥。集群 Secrets 加密、备份与 RBAC 由运维负责。
首次启动自动按序迁移并 bootstrap；已存在 admin 时不会靠文件自动重置密码。

## 3. 固定镜像部署

从通过 Linux CI 的 image receipt 获取三个 digest；不要根据 tag 猜 digest。
首次 GHCR package 公开性需要匿名 pull 验证，或显式配置 imagePullSecrets。

安全目录中的 values 示例：

```yaml
api:
  image:
    reference: ghcr.io/william-lbn/control-api@sha256:<verified-digest>
  existingSecret: neon-control-plane-credentials
  pgTLSMode: verify-full
  pgCASecret: neon-postgres-ca
  cookieSecure: true
  proxyHost: proxy.neon.svc.cluster.local
  proxyPort: 4432
  publicProxyHost: db.example.org
  publicProxyPort: 5432
  computeGatewayURL: https://neon-compute-management-gateway.neon.svc:8443
  computeControlHost: neon-compute-management-gateway.neon.svc
web:
  image:
    reference: ghcr.io/william-lbn/control-web@sha256:<verified-digest>
ingress:
  enabled: true
  className: traefik
  host: console.example.org
  tls:
    - hosts: [console.example.org]
      secretName: neon-console-tls
```

`<verified-digest>` 是占位符，不能直接安装。所有 image reference 必须 sha256。
如果 namespace 或内部服务名称不同，显式修改 Proxy 和 Gateway DNS；
当前基础设施名称/adapter 合同仍需匹配 pinned profile。

```bash
helm upgrade --install neon-gateway charts/compute-management-gateway -n neon \
  --set-string image.reference='ghcr.io/william-lbn/control-gateway@sha256:<verified-digest>' \
  --wait --timeout 5m
helm upgrade --install neon-control charts/neon-control-plane -n neon \
  -f /secure/neon-control/control-values.yaml --wait --timeout 10m
kubectl -n neon rollout status deployment/neon-control-api --timeout=300s
kubectl -n neon rollout status deployment/neon-control-worker --timeout=300s
kubectl -n neon rollout status deployment/neon-control-web --timeout=300s
```

Chart 当前 API 单副本、Recreate，滚动升级会有短暂控制面中断。
独立 Worker 同样单副本 Recreate；数据库 session 领导锁不替代连接与外部动作 fencing。
DB、已有数据与 Secrets 不由这个 Chart 创建/销毁。
Public SQL 入口由 Proxy Chart 暴露；Web ingress 不承担 PostgreSQL TCP。
Gateway 默认 ClusterIP、image profile，无 hostPath；host-binary 仅保留显式调试兼容。

## 4. 验收与观察

- /healthz 仅为进程活性；/readyz 校验 metadata PG，不能证明全栈功能。
- 浏览器登录，查看 capabilities；disabled 服务必须保持不可用状态。
- UI 创建低资源测试项目、写入/读取、分支继承/隔离、休眠/冷醒。
- 验证多个 Reader、密码轮换、目录状态、监控不唤醒、Operation 与 request_id。
- 观察控制面/etcd/Neon 基础组件日志及 CPU/RAM/磁盘压力。
- 测试后通过控制 API/UI suspend Compute，保留身份 fixture、证据和数据。

真实步骤见 TESTING.md。未执行过的测试不能写为通过。

## 5. 回滚与恢复

1. 升级前保留 Helm history/values、当前固定 image digest、metadata DB、
   所有 owned Secrets 和幂等密钥以及数据面对象/WAL 的一致恢复点。
2. 先确认没有活动 Operation；暂停新的控制面变更并记录维护窗口。
3. metadata 迁移目前为 forward-only；旧 API 必须先验证新 schema 兼容。
4. 仅 schema 兼容时使用 `helm rollback <release> <revision> --wait`。
   涉及 split/all 或旧无锁版本切换，先停止两组控制器并确认 Pod 退出，详见 WORKER-SPLIT.md。
5. 不兼容迁移按隔离恢复流程恢复 DB + Secrets + 固定镜像；
   不能将数据库降级脚本或重置密钥当作安全回滚。
6. 恢复后再次验证真实 Proxy SQL、目录、分支、冷醒及授权负例。

当前没有完成全栈 HA/DR/PITR 认证；恢复演练与生产切换是独立放行门槛。
