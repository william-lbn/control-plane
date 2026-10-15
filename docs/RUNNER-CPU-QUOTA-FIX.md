# NeonVM runner 动态 CPU 配额修复

## 核验结论

运行镜像的 OCI source/revision 标签指向 william-lbn/autoscaling 的 c0052f5f2d38fce6c70e448f3f1ee2ee239a0a93。修复基于该精确 checkout，未修改另一份不同 revision 的上游 checkout，也未修改 Neon Rust。分支 fix/runner-cgroup-cpu-limits，本地提交 **172092424135c565a7e49cb696201daa8e15c41b**；本批尚未推送。

原镜像：williamluckyli/neonvm-runner@sha256:5dc2833a442265b15c9584c05b4c12a1eafdc1a1b0d581c2c3093f551ec9328b。

检查同时覆盖实际 VM/Pod command、cgroup 挂载、runner /cpu_current、QEMU cgroup 文件，以及精确 revision 的 runner/main.go、runner/cgroup.go 和控制器配置。旧部署带有 disable-runner-cgroup，runner 命令包含 skip-cgroup-management；/cpu_current 显示 250m 但没有对应 QEMU cgroup，不能算实际内核配额生效。仅去掉这个开关仍不够：旧动态回调没有调用配额 setter，只更新缓存。

## 修复边界

- CPU 初值取 Guest.CPUs.Use，避免用最小值冒充当前值。
- Sysfs/QMP 更新统一经过串行化预算提交；启用 cgroup 管理时先写内核配额，成功后再更新缓存并确认请求。
- 写内核失败返回错误且保留旧确认值；重试保持幂等。
- 显式关闭 cgroup 管理的兼容路径保留；控制面拒绝将这种 runner 用于新的小数 Endpoint。
- 保留原 overcommit factor **4**、Guest CPU 热插拔与 controller 协议，不把 250m 改成严格 0.25 核隔离。

改动为 runner/main.go、runner/cpu_budget.go、runner/cpu_budget_test.go；新函数注释解释确认与内核应用的顺序。单个定向 race 测试覆盖内核失败、重试和缩回，vet、模块校验和静态构建通过。原 fork 的 ci/test.sh 已执行 go test -race，包含该新测试；未启动远程 CI 或重跑整个旧验收。

## 本地构建、部署与回退

正常发行继续使用 fork 内 Dockerfile/CI。此次为了避免重新下载或编译相同 QEMU/kernel，Linux 构建匹配源码的静态 runner，校验 SHA256 后覆盖到现有自有基础镜像。基础镜像 OCI 层、平台 manifest 和源标签均校验；新镜像层中的二进制再次校验，源文件哈希与提交一致。

| 项 | 值 |
| --- | --- |
| runner 二进制 SHA256 | 2fd3b2cdf8974253d441891679013cfb9c78438384b8a2319bcb2b167cdc1058 |
| 本地 runner digest | sha256:b636612c60251a289240d1a08476dd7a8a423d93924ac3fc35aa9030ebda769b |
| OCI 归档 SHA256 | 9571a6808068a33d1a31c623f6d571ac445c55d1ade0b10b652e36e68d6aff36 |
| 镜像名 | neon.local/neonvm-runner:quota-172092424135 |

三个节点逐一验证归档哈希并导入 containerd，核对 digest 后升级。实际 Helm release 为 default 命名空间的 neonvm，资源位于 neonvm-system；revision 52→53。三个 controller Ready，controller 镜像保持原 digest。只变更 runner 镜像和显式 cgroup 开关，没有更新 CRD、重启 RKE2/etcd 或删除持久数据。升级前后 webhook Secret UID/内容摘要、全局 10 个 PVC UID 及其他 18 个 release 清单保持一致。

统一 Helm neonvm 子 Chart 的 controller.runnerCgroupEnabled 是具类型约束的布尔配置，默认 false。实验覆盖文件设 true，并锁定上述本地 digest；公开配置没有写入只能在本实验集群拉取的 neon.local 地址。正式发行须将 fork 修复推送、公开发布镜像并锁定公开 digest，保留 SBOM/provenance 与回滚镜像，再发布统一 Chart。

回退时先正常缩零本批小数 Endpoint，保留角色/数据库/路由元数据；回退前保存当前 Helm values/manifest 和镜像归档，再恢复已归档的前一版本 values。旧 runner 不满足小数配额验收，回退后不可声称该能力继续可用。不要只回退控制器开关却保留运行中的小数 Endpoint。

## 复测与证据

按 [小数 CPU 手工步骤](FRACTIONAL-CPU.md)从 UI 创建 Reader，以 tools/observe-fractional-cpu.mjs 的只读观察独立核对实际配额，再检查 SQL、冷醒与正常停止。现场回执见 [交付](DELIVERY-2026-10-15-FRACTIONAL-CPU.md)。维护者保留 runner-quota-go-new1、runner-quota-local-image1、runner-quota-neonvm-deploy3 的原日志与归档；误判 Helm release namespace 的两次只读失败也保留。

严格 Guest 隔离、公平计量、节点竞争及物理 RAM 回收仍是独立门槛，此修复不关闭这些门槛。
