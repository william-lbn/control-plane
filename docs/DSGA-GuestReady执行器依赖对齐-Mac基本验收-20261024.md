# GuestReady执行器依赖对齐

沿既有 `codex/dsga-control-plane-v3-20261020` 演进，fresh fetch 的自有 main
`7899ec0e8f1aab75598ec37e566d7260568a1c9d` 已完整包含，不改主分支。

Autoscaling 两笔独立中文提交新增 Prepared 合同和实际 Agent GuestReady 执行器。
自有功能分支 `d3bd69d246cb40f6116fd1b7c874a9a319004a81` 已先推送并 ls-remote
核验，再将本服务 replace 绑定为 `v0.0.0-20261024152806-d3bd69d246cb`。实际 Go
下载的 Origin.Hash 与完整 SHA 一致，新增准确模块校验和，不用本地 replace
冒充远程候选。Neon 发行锁同步同一 AS 来源；PG17 源码未变。

本次本服务没有路由/业务/账本格式改动，仅依赖对齐。准确依赖下 Mac
`GOTOOLCHAIN=go1.26.5 go vet ./internal/control` 退出 0，包含实际消费者编译；
未重复既有 TLS/PG/鉴权业务测试，静态检查也不计作业务测试通过。来源证据
`build/dsga-guest-ready-module.json`，检查日志 `dsga-guest-ready-cp-vet.log`。

AS 新增模块去重 4 顶层/21 含子项 race 通过；实际 Agent/root guard 双架构编译
通过，内核及前驱域仍是明确夹具，见 AS 的 GuestReady 验收记录。Prepared
合同不等于完整预算生产者；可信预算/系统外部上界、首次准入/全排空、Host/
最终 Controller-Runner-QMP、公开 worker/生命周期及受控跨 boot/owner 关闭
仍须编码。真实 Linux K8s/NeonVM/HugeTLB/物理/性能后验，不称整体已完成。
默认关闭，Goal active；不构建镜像、不准备/执行部署、不合 main。
