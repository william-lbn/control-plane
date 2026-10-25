# 持续聚合Quiesce的准确依赖

沿既有 `codex/dsga-control-plane-v3-20261020` 演进；再次fresh fetch的main
`af5e7044167c96faa71d74548d9ae87034cc6833` 全部包含，无空维护或分支重建。
本批仅更新依赖和中文记录，不改控制面业务入口，不重复未改路由/数据库测试。

AS合同568e5645、执行9740ec96及后续来源恢复修复，最终
`2c9a26abac1df37207e32525fde6c94ae4b4c402` 已先推自有既有分支并核验远程。
显式下载 `github.com/william-lbn/autoscaling@v0.0.0-20261025172233-2c9a26abac1d`，
Origin.Hash为同一完整SHA；go.sum相对本仓HEAD仅加最终模块2条记录。
中间9740依赖已下载核验但未提交；确认本地内容恰好是自己这份未提交差异后
恢复它，再锁最终2c9；未夹带中间2条sum，不覆盖其他依赖或用户改动。

Go1.26.5 `go vet -mod=readonly -p=2 ./internal/control`退出0，最终日志为
`build/dsga-quiesce-source-binding-cp-module.json` / `...-cp-vet.log`。AS最终
Mac race5顶层/33含子项、来源替换/旧格式只读保留及真实native关闭消费1项通过；
root/外部预算明确为模型，不是Linux认证。完整命令写前持久原聚合来源，旧仅
关闭格式保持Unconfirmed，只读查询、不补源、不结案、不写重放。

完整Prepare/Finalize/SystemOutsidePG、安全降normal及后台私有上界、首次准入、
Host最终VM/公开worker/受控跨boot仍需代码。Linux内核/HugeTLB/物理/集群性能
后验，不把缺代码写成待测试。本轮不构建镜像、不准备/执行K8s部署、不合main、
不发布；默认关闭、无VM许可，所有服务沿既有分支。
