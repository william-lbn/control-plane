# DSM1固定观察路由与准确模块

沿既有 `codex/dsga-control-plane-v3-20261020`，本批主线af5e704先经独立维护
`eb6829b5f66fa3ad4b81bc42de3952a8e4825a4b` 完整合入并推送核验，再接DSM业务。
新增固定GET `/dsg/v1/work/dsm`，使用原owner/实例/私有Service/凭据检查、无缓存
原字节转发和独立观察通道，不接受用户自报预算、重发resize或清未知Pending。

AS已先推送并核验 `d8c1f2ea2eb2d9db30469f2d279090a2ce6c03b4`。显式下载自有fork
`v0.0.0-20261025133115-d8c1f2ea2eb2`，Origin.Hash为完整同一SHA；go.sum只追加
新模块两条记录，保持其他依赖。准确依赖后readonly vet通过。

Mac受影响代理race两项通过，0skip：固定路径/凭据/原字节及被阻塞写入期间独立
观察。首轮1过，新增路径实际成功后旧夹具仍把总调用数写死15而非16；改为显式
测试路径表长度后只替换失败1项通过，未重跑已过项。没有改变产品转发实现来
迎合计数，原失败保留在 `build/dsga-dsm-formal-cp-proxy.jsonl`，替换记录
`...-cp-proxy-replacement.jsonl`，准确依赖收据 `...-cp-module.json`、vet `...-cp-final-vet.log`。

跨仓DSM真实Go race/TLS/生产Router/JWT/正式SQL/准确PG46已由AS/Neon门禁执行；
这里两项是实际本地代理transport检查，不冒充额外整集群E2E。DSM只是本boot普通页
payload观察，完整预算生产者、后台/私有与最终VM闭环仍须代码；Linux内核/物理/
性能另验。不构建镜像、不准备/执行K8s部署、不合main，默认无VM许可。
