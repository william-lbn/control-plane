# C1 自动供应与通用 Guest 接线方案

状态：2026-11-07 现场及源码审查后的执行设计；实现尚未全部完成。本文是[平台总方案](平台总方案与演进规划.md)的 C1 细化，不另设总计划，不代替真实验收。

## 1. 当前已经证明什么

PG17/PG18 各四项内存动作及应用/WAL 已通过。控制面本轮已完成 PG17/PG18 共同创建合同、冻结计划、原 Worker 步骤、原 VM/配置/初始化及普通 SQL Ready 检验；定向测试、Mac/Linux 编译和经118访问现有PG18只读观察通过。

**尚未证明：通过公开控制面 API 自动创建一个全新项目，自动获得真实资格，修改策略后完整执行内存动作。** 已有实例的只读成功与人工发布后的核心通过，不能当作自动供应已实现。现有创建入口保持关闭，下一次验收必须使用独立项目/tenant/timeline。

## 2. 本次核到的真实缺口

| 缺口 | 现场/源码依据 | 对产品的影响 |
|---|---|---|
| Guest含固定测试Compute身份 | `vm-builder/normal_split.go:126`将build plan argv封入vmstart；`normalTrustedComputeScript`复用；当前PG18构建计划中compute-id等于现有测试Compute | 同一候选镜像不能直接供应任意新端点 |
| CP旧VM投影与已验收启动方式不同 | CP renderer设置command/args/config卷/TLS；现场已验收VM没有这些override、没有ClusterIssuer | 不能仅靠纯构造测试就启用新VM，须统一启动合同 |
| 存储管理处于开发部署模式 | storage-controller参数为HTTP1234及--dev；原Neon源码已有HTTPS/public-key/JWT配置 | 新Worker有正式HTTPS/JWT要求，当前部署不能直接满足 |
| 资格与策略是静态逐VM配置 | Agent从prepareQualifications/automaticMemory映射在Owner启动时取值；ConfigureAutomaticMemory仅允许Run前 | 新项目/在线策略不能依赖人工改Helm并重启Agent |
| 发行标签继承历史信息 | PG18 Guest实际digest与PG18 SQL/功能已确认，但label含early PG17 scope与早期revision | 标签不能作为最终来源锁，须从构建原件核实 |

固定身份构建计划来源：`/data/neon-build/validation/PG18-固定Compute身份启动器增量Guest-940b25fb/production-build-plan.json`；只记录其字段结构及“固定ID与既有实例相同”的布尔结果，不输出argv、环境值或连接信息。原始私密配置继续留在保护目录。

这些差异发生在本项目的发行/启动/控制面接线，不是已证明的上游PG内存算法缺陷；也不能据此说官方托管系统具有同样问题。

## 3. 通用启动的决定

每个受验证PG发行使用通用Guest，不为每个租户重新构建镜像。版本选择只决定发行及其能力，控制面和Autoscaling不增加PG17/18业务流程分叉。

- 镜像固定：PG/Neon扩展、compute_ctl、Guest内核/资源守护器、非root UID、可执行路径、允许的启动形状和最低运行配置。
- 实例配置绑定：逻辑endpoint/Compute名称、原VM UID、incarnation、执行实例身份、原immutable配置UID/摘要、tenant/timeline。它们从原可信Controller/Bootstrap读取，不能由租户传任意URL/命令。
- 同一逻辑端点身份、物理VM UID、原生PG boot和Owner epoch含义不同；恢复必须逐项核对，不将其中任意一个当PG大版本号。
- 启动模板只允许明确的身份/配置槽位，不能变成任意command/env执行入口。复用原Runtime/ISO与manifest来源检查；实际argv与受验证展开结果必须一致，不能跳过旧hash校验来使新镜像通过。
- 新Guest入口继续执行原Guest检查、资源初始化及controller-ready等待，再按既有权限降权启动。CP不能以VM command override绕过这些入口。
- 原immutable配置的交付与一次configure幂等性采用同一启动方式，不能一边镜像内旧argv，一边CP另挂配置，形成两个权威。
- 证书复用已有NeonVM/cert-manager支持；先确认Guest实际证书供给/轮换合同，再选择受验证发行配置。无证书/来源不一致仍拒绝，不能靠关闭TLS校验补接线。

旧已验收镜像、旧计划及运行VM保持可识别；新能力用明确的发行能力/启动合同登记。具体wire版本随实现确认后固定，不提前把草案版本号写成现场已支持。

## 4. 资格和在线策略的决定

控制面提交业务意图与产品边界；原Scheduler依旧是唯一Host资源准入方。独立资格签发器继续从真实原VM/节点/硬限收集并签发，CP只引用其结果，不制造预算证明。

优先复用现有资格Collector/Issuer、VM身份、受限平台配置和原Owner执行循环：

1. 原创建Operation产生VM及immutable配置后，正式资格流程使用该原身份；已有资格必须验证来源/UID/hash，不因为文件存在即认为有效。
2. 新实例的具名观察授权由正式供应流程管理，不依赖人工改三个Helm清单；先核原RBAC/来源机制能否复用，Agent不得获得业务凭据或全namespace秘密读取能力。
3. 原Owner在自己的串行执行循环处理资格/策略配置代次；禁止启动第二个执行器。原待决动作先按journal确认，配置更新不得丢失Unknown、取消或旧写者fencing状态。
4. 策略改变仅在已签发能力及租户规格范围内生效；无法获得新预算时继续保持已证实安全的现有资源与SQL。真实安全条件失效仍按原保护处理。
5. 期望代次、已应用代次、等待原因和实际PG/normal/VM/Host状态分别读回。写API成功不等于动作完成；UI不能用期望值冒充实际值。

已有手动资格/策略验收回执仍保留。此处的自动供应实现通过前，不将启动时静态map包装成“已支持在线配置”。

## 5. 存储管理接线

使用Storage Controller原生HTTPS和JWT，避免增加新代理/调度服务。部署变更前先列出实际调用链：控制面→controller、Pageserver→controller upcall、controller→Pageserver/Safekeeper、controller→现有hook，以及维护/观测客户端。

逐接口使用原生支持的scope、CA和轮换方式；配置只持Secret引用，不将私钥/JWT写入Helm values、数据库Operation或Git。先核调用方全部支持所需身份，再切正式配置，保留原数据/PVC；不能单独打开认证后等待业务失败才发现漏接。内部网络隔离与外部管理加密边界要明确，不假定HTTP Service就天然受保护。

## 6. 落地顺序与验收

1. 固定并追溯当前通过的内核/扩展/Runner来源；补通用Guest启动与CP投影，仅测试其新增身份绑定和原启动保护。
2. 原存储管理服务与调用方完成认证/TLS，定向验证拒绝错误身份、正常供应及已存在数据访问。
3. 接入独立资格和原Owner动态策略，覆盖更新与原动作待决的直接交叉；不扩大到整机证明或通用K8s故障矩阵。
4. Mac公开API创建独立PG17项目→实际SQL→四动作→应用数据/WAL；PG18使用同一流程、同一上层代码，只改变发行。
5. 逐阶段中文提交原分支、Linux从GitHub取同SHA、固定digest增量镜像、SSH预载117/118/119、自有Helm部署，再跑同一产品场景。
6. C1完成后接C2/C3：生命周期及现有Auth/Data API/Storage/Functions/AI Gateway真实联合链。C4补发行运营界面，C5做承诺范围内的生产放行。

每条回执都标明真实/夹具、源码SHA、运行digest、原对象/动作、日志/退出状态及未覆盖范围。测试前核日志和磁盘，中途核失败/停滞，不等任务结束再检查。现有核心通过与新控制面尚未通过可以同时成立，不混用百分比。
