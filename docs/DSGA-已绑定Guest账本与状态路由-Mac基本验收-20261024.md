# 已绑定Guest账本与资源状态路由

新增固定GET `/dsg/v1/resource/state`，沿原管理TLS/SNI、原VM/Service和current
owner链转发；拒绝POST。状态包含独立资源floor/Original/Held与实际限额，不是
完整预算、原命令NoEffect或VM许可。控制面不生成root事实，也不接受任意代理。

AS b9e393f8fa846b19d14c0246c1da46cf0d82a70f已先推送并核对，再绑定本仓
v0.0.0-20261024145012-b9e393f8fa84和校验和，下载Origin Hash匹配。同一版本
Model增加optional原Guest身份；旧未绑定数据可读，写入新字段后不能无条件退回
旧reader。保持既有服务版本功能分支，不合主分支。

Mac仅运行新增路由用例1项race通过，0fail/skip。用例通过实际生产Store的UID/RV
条件写入新ResourceIdentity，再经实际TLS网关、凭据核验和新解码器读取，验证
不是只对旧空字段数据通过。原生/root目标和Kube API明确为协议fixture，不是K8s
或物理端到端。准确依赖下vet通过；未重复旧路由、PG或SDK。

证据在工作区build/dsga-resource-owner-cp.jsonl、cp-vet.log及module.json。
完整资源GuestReady执行器、预算/首次准入/全排空、Host/最终VM和受控恢复仍需
编码；真实Linux/K8s/物理性能后验。不构建镜像、不准备或执行部署、不合main。
