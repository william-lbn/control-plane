# PG17冻结三节点放置与Sysfs：Linux验收

新VM profile v3冻结完整selector与3节点hostname，固定Sysfs并深拷贝放置结构；旧v2未绑定拒绝新VM POST，已绑定同UID按原计划恢复，不读env补授权。067前向格式守卫保持历史row/xmin。

采用17个root/57个subcase，Go1.27.1 race全部通过，0失败/跳过；保留首轮2个夹具失败，仅重跑失败及改动根。go.mod/go.sum未改。真实隔离metadata PG/TLS Kube peer，不是实际集群、KVM或物理内存E2E。

[详细验收和精确命令](validation/pg17-frozen-placement-linux-20261030/README.md)；[结构化收据](validation/pg17-frozen-placement-linux-20261030/summary.json)；[逐根采用来源](validation/pg17-frozen-placement-linux-20261030/adopted-tests.json)；[本次placement配置片段](validation/pg17-frozen-placement-linux-20261030/placement-fragment.json)。
