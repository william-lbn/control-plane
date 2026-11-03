# PG17 normal 完整命令的 Gateway 依赖对齐

当前阶段只修复既有 Gateway 的内存扩缩容接线，不进入重新获取最新控制面并补齐全部功能的后续阶段。

旧镜像 `7402ec06` 的实际 ELF buildinfo 表明，它通过 Go replace 编入 autoscaling `d74e02db0da6`。当前生产 normal PrepareBudget 包含双 PG 快照和物理释放事实；原完整 typed 合同允许 8–16KiB 命令，旧 journal 读者只有默认 8KiB 上限。Gateway 的 scoped 请求在转发前经过 `journal.Store.Load`，因此合法大命令落账后，旧读者会拒绝请求。这不是此前 Runner 首次 Owner epoch/LeaseTransitions 错误的原因。

本次只把 `api/go.mod` 的 autoscaling replace 固定为 `v0.0.0-20261103115459-fff0a385ee57`，对应原功能分支提交 `fff0a385ee5735068139bd33d9b1727ec6f5ed37`；go.sum 只增加该模块经 Go 官方校验获得的两条摘要，保留已有依赖及历史条目。模块 h1 为 `h1:6znoEG2nxWB0VsvBTaECK/PcYLsbLrxNlGE8sXu5ei4=`，go.mod h1 为 `h1:5qW3EK3EXET9Tg0MQLNpUnvqE42xJ7iXytCBbD3BOHI=`；下载结果的 Origin.Hash 与完整 fff0 提交相同。不使用本地 replace，不执行 go mod tidy，不修改 Gateway 生产鉴权、SNI、Token 或路由源码。

新增测试复用原 Store 的 BindResource/BindPG/BindInitialBudget/InitialAdmission/Begin/Start、UID/RV CAS 和 codec，生成 8193B、16384B 完整 normal PrepareBudget 后读回原命令与摘要，再经过原 TLS scoped 代理正控。Owner、过期 Lease、普通 compute JWT 继续拒绝；缺 normal fence、legacy 大命令和超过 16KiB 同样拒绝。只在本次 normal 路径检查必要鉴权负例，不扩泛用鉴权矩阵。

测试数据 `api/internal/control/testdata/dsga-normal-initial-domain.json` 为上述 AS 提交的 `pkg/dsga/journal/testdata/initial-domain.json` 原始 21315B，SHA256 为 `649e028687a3f354333b6e7c4f23985edeed3131a63fa99257ba6afc2aab37f5`。它是明确的单元协议夹具，不是实际平台资格或生产凭据。测试只将 unit incarnation 对齐原 broker API binding，并由原 Initial.Digest 重算初开命令；不手写压缩 WAL，不接触真实 571 VM 或签名私钥。

从 api 模块根可复现直接相关检查：

```sh
go test -mod=readonly -p=2 -race -count=1 -timeout=120s \
  -run '^TestDSGANormal(LargeJournal|Proxy)' ./internal/control
CGO_ENABLED=0 go build -mod=readonly -p=2 -trimpath -buildvcs=false \
  -o /本次独立输出目录/compute-gateway ./cmd/compute-gateway
```

Linux 原 Go1.27.1、模块/构建/C 缓存和 /data GOTMPDIR 均复用；重 Go 作业限定 3CPU/3Gi 串行。构建显式使用原 127.0.0.1:7890 代理并先检查；测试和编译阶段使用已有模块缓存。首次新模块下载在官方 sum.golang.org TLS 握手超时，尚未执行测试，原失败票保留；后继有界重试维持官方签名 key 验证，不关闭 checksum 校验。

本轮实际定向验证已完成：`pg17-gateway-normal-as-container-go-20261103T121336Z-d0a0af` 中5个相关 race 根、19个 pass 事件、0 fail/skip；单 Gateway ELF 编译0。随后只加强负例，在缺fence/legacy变更后先重算原 CanonicalOutsideName，排除“陈旧摘要本身导致拒绝”的干扰；`pg17-gateway-normal-negative-refine-20261103T122020Z-5d7de5` 对该1根3子项复验通过，不重编生产字节未变的 ELF。

实际 ELF 为 `/data/neon-build/validation/pg17-gateway-normal-as-container-go-20261103T121336Z-d0a0af/export/compute-gateway`，SHA256 `b54b20f22f39b5970bfe1b60db2b0f03f24cb581abba61c94df71f2852e185fd`；Go buildinfo 明确编入上述 fff0 模块与已验证 Sum。此前除了 checksum 网络失败，还保留一次误用宿主不存在 gcc 的构建前失败；最终沿原 dev-tools 固定镜像的 C 编译器运行，Go1.27.1、模块/构建/C 缓存和 /data 临时目录原样复用。

上述是明确单元协议 peer 的读回/scoped链验证，不是实际 Guest/PG 或 Kubernetes 内存端到端通过。镜像/Helm 部署、完整 PG17 Grow/Shrink/Regrow、自动策略和故障恢复仍需另记真实 nonce 验收。
