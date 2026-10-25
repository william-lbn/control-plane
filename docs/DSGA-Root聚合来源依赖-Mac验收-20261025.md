# Root聚合来源的准确依赖绑定

沿既有 `codex/dsga-control-plane-v3-20261020` 演进。新一轮fresh fetch的main
`af5e7044167c96faa71d74548d9ae87034cc6833` 已完整包含，无空维护或分支重建。
本批不改控制面业务入口，只锁定先推送并核验的自有AS来源生产模块：
`a3d91b8a10bbd7e20b6a3710ae220bf9018d1621`。

显式下载 `github.com/william-lbn/autoscaling@v0.0.0-20261025142910-a3d91b8a10bb`，
返回Origin.Hash与完整SHA相等；go.mod替换准确版本，go.sum只加对应两条记录，
无其他模块升级。Go1.26.5 `go vet -mod=readonly -p=2 ./internal/control` 退出0，
日志 `build/dsga-root-aggregate-cp-module.json` / `...-cp-vet.log`。未改代理业务，
不重复此前通过的DSM/路由/真实PG测试。

AS实际Agent GuestReady新审计v2带原root PG计费域+整Guest巨页池聚合来源，
旧审计v1不回写。来源不是完整预算，也不对自报摘要授信；后续仍须在认证root
重新Match。完整外部成本与Prepare/Finalize/BG私有/首次准入/最终VM等仍须代码，
Linux cgroup/HugeTLB/内核物理和集群性能另验。本轮不构建镜像、不准备或执行
K8s部署、不合main、不发布；所有服务沿既有功能分支，仍默认关闭。
