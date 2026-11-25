# Neon 自托管控制面

控制面提供 Go API、后台 Worker、Web 控制台和业务运行时管理能力，与数据面、资源安全协议及 Helm 部署入口共同组成产品。内网部署不要求公网 IP 或公网域名。许可证为 Apache-2.0；本项目独立于 Neon 托管服务。

## 微服务职责

| 服务 | 职责 |
| --- | --- |
| API | 身份与授权、项目/分支/Compute 接口、幂等请求和公开 Operation |
| Worker | 推进原 Operation，协调存储、Compute、资格、暂停恢复和退役 |
| Web | 面向用户的控制台，不直接持有集群管理员身份 |
| 元数据库 | 保存控制面目录、业务对象及工作流状态，不承载租户数据库 |
| Compute 管理网关 | 隔离管理凭据，验证当前原实例并调用运行时协议 |
| Proxy/Storage 适配器 | 编译型数据面认证、唤醒及存储状态观察 |

Compute、Runner、租户和时间线由业务 Operation 及原生 Controller 创建，不能预先写入 Helm 为固定实例。Agent、Scheduler、Root 和 Host 预算属于 Autoscaling 仓库，数据面属于 Neon 仓库。架构与开发详见[平台总方案](docs/平台总方案与演进规划.md)和 [DSGA 控制面集成](docs/DSGA-CONTROL-PLANE-INTEGRATION.md)。

## 标准部署

完整产品统一使用 [Helm 中文入口](https://github.com/william-lbn/neon-helm/blob/codex/dsga-neon-helm-v3-20261019/README.md)。本仓库的 `charts/neon-control-plane` 是控制面 Chart 的权威来源；不能用另一个仓库中的历史同名 Chart 替代。

发行包固定源码和镜像摘要；站点声明仓库、存储、区域、命名空间、节点筛选及集群 DNS 后缀。运行身份在当次安装生成或读取，不复用旧 Node/Lease/Service/Secret UID，不要求特定主机地址。管理与 SQL 验收保留内部信任校验；完整 TLS、生产 HA 和灾备仍需独立建设与验收。

## 当前验收与限制

PG17/PG18 的公开创建、真实 SQL、同一 Compute 自动物理内存增长与回缩、WAL 结清、暂停恢复及正式退役已在标准部署阶段验收。原证据的源码、镜像和环境边界见 [2026-11-24 阶段报告](https://github.com/william-lbn/neon-helm/blob/codex/dsga-neon-helm-v3-20261019/docs/standard/acceptance-20261124.zh-CN.md)。后续文档或工具改动要记录自己的验证，不能借旧报告宣称已实测。

Root 状态未知时保留关闭准入；暂停排空与已有授权恢复必须维护原预算、Owner 和实例身份。自动可信恢复状态机及首次 HTTP 异常细分归因仍待研发。新 Node 持续登记暂缓。可选 Auth、Functions、Data API、AI、对象存储产品能力各有独立合同，不由核心数据库验收自动认证。

## 维护入口

[中文标准边界](docs/标准部署与维护.zh-CN.md)说明发行、失败接续和资料管理；[Linux 开发](docs/LOCAL-LINUX-DEVELOPMENT.md)及[生产门槛](docs/PRODUCTION-GATES.md)提供各自范围内的要求。

维护分支为 `codex/dsga-control-plane-v3-20261020`。以中文记录变更原因、兼容性、实际验证及待办；正常快进推送。旧部署日志保持原始证据，在历史目录按日期查阅，不作为当前版本号或现场部署状态。
