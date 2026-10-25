# 首次PG持久身份的准确账本读者

沿原codex/dsga-control-plane-v3-20261020，将自有AS准确替换版本更新为
v0.0.0-20261025192811-da2aa56a1efa。依赖先推送，实际Go下载Origin.Hash严格核验
完整SHA `da2aa56a1efaf6241ebf85c7814542e41e757a66`；只新增zip/go.mod两条sum。
readonly vet internal/control通过，不构建镜像、不准备/执行部署、不合main。

新增专用TLS签发器/typed Store/CAS验证1PASS0skip：无HWM时经真实Store写
Model v2/PGInstance，生产TLS listener和签发器读它后可签发原VM/Compute/
epoch的有效签名token；账本不变、凭据零写入，不能把签发成功当成换PG boot
许可。Kube/TokenReview为协议夹具，不证明实际集群RBAC或PG/预算。
首轮用另一测试库引出缺依赖的setup失败，未执行场景；改用已有标准testing
后单项通过，没引入新依赖，初日志/初测试源码保留，不算产品失败或重复通过。

Model v2与此前最终Resume审计v2不同；外层压缩信封仍v1，PG/Neon原生接口
不改。全部账本读者先升级后启用v2生产；旧二进制会拒绝，不能删字段或重造
日志回退。AS去重36顶层/71含子项race通过，完整账本旧合同失败及单项替换、
旧读者三条兼容断言和最终双Linux架构仅交叉编译详见AS同名中文收据。

本项只是首次身份基础，首次预算/开放、完整Prepare/Finalize/SystemOutsidePG、
安全降normal/后台私有上界、Host最终VM/公开worker/受控跨boot仍缺代码。
Linux内核/物理/集群/性能另验，其他PG暂缓，Goal active/incomplete、默认关闭。
证据见[本次收据](DSGA-首次PG绑定账本读者-Mac收据-20261026.json)。
