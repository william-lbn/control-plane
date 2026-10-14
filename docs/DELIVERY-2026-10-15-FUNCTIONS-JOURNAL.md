# Functions 已调度请求日志本地交付

控制面 0.30.0 / metadata migration 030 / chart 0.9.14 / Helm neon-control-plane revision 80 已部署；API 合同 81 paths / 120 operations。API/Worker `neon.local/control-api@sha256:2eae713d50c9b3e12f8dac60b9b6a62427f507c0a752cce5fb0b31f4b1b57249`，Web `neon.local/control-web@sha256:f4d3e95bae70d44249017c8461c44903d6d9d7247d01ca78c0a0cfe253c9c819`；两者本地不可变源 SHA-256 `3ccd47204fc280168f28aeff8fb9b47e32cb1bc62d9c0e442eef36fe5cb45848`。Auth 与其他组件沿用既有验收摘要。源码父 Git 5449a11 不能替代实际源快照。未触发远程 CI 或发布 local image 到公共 registry。

Linux 新 PostgreSQL/cursor 两项检查通过：真实 admission/once completion/tenant scope/keyset pagination/no sensitive input。前端类型、构建、Helm、31 个串行 suite admission 和 API.md 检查通过。旧 UI/unit 功能未重新执行。

真实 UI 原案例：函数 `fnc_a09ba3b71485f111`，slug `journal1792013286362`。第一次输入用了非法连字符，422 后没有创建资源；修复测试输入，沿用同一原时间戳。实际部署遇到节点 2 的 KVM unhealthy admission，原 runner Failed，原 Operation 超时；当前三节点插件 Ready、KVM allocatable 1k。原失败 VM UID b176bbe0-5fc3-4e07-b37d-8b75d8401632 的完整 manifest 已私有归档 SHA-256 c61b2ff70f07e31cddec41008a01e9f6e46c66db65b4ed23bd715f246c2ae5b9；通过普通停止和部署恢复同一函数，没有覆盖数据库状态或创建新函数。

Console 与独立公开 origin 各实际调用 /health 一次，均 HTTP 200；记录 `req_6abbb412cf4dc33d6867d892`, `fhr_3a641e5d7ac3e82c7b420187`。最后修复请求 ID 断言（24 hex），仅继续读取原结果，没有再 dispatch。UI Job publication-ui-20261014215547 原案例验收 7 项通过：原退休恢复、实际 VM 调用、UI 完成状态、公开原请求、稳定分页和隐私、停止运行。全部失败/恢复日志、输入和 Operations 6 个保留。

最终 managed compute Pods/VM 为空，测试函数正常 suspended，Function journal/元数据和 SQL/原 bundle/证据保留。部署阶段 9 PVC 与 768 原 Secret 身份保留；其他 6 Helm release manifest 不变。

[模型、接口与复测](FUNCTIONS-REQUEST-JOURNAL.md)。该功能只记录调度后结构状态，不是 stdout 捕获、完整访问日志或 billing。未知/响应不完整有实现路径；真实 API crash/SSE disconnect 的独立故障验收、跨实例外部栅栏、HA/DR、全链路 TLS 仍有门槛。本地 KVM 故障恢复结果不等同硬件 SLO 准入。
