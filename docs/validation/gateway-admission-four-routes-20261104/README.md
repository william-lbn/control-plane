# 管理网关补齐原准入租约与延续关闭协议

真实 PG Owner 的 Run 首次必须访问 admission/lease。现网旧网关固定名单遗漏该路由，在认证和转发前拒绝，导致 Agent pg_run 很快退出，Owner 只读入口返回 unavailable。这里只补齐当前原 AS/Rust 合同已实现的四条精确 POST 路由，沿原独立观察连接池转发；执行堵塞时仍能观察、续租或关闭。不增加通配路径，不改 JWT/SNI、实际 VM/ConfigSecret/Service/Owner 身份校验，不放宽请求/响应限制或原 native 严格 DTO。

本轮采用基线0fa6完整源码加本次仅两文件，明确排除同时开发的 managed SQL 路由代码。七个直接 TLS/合同入口实际 race 通过，53 个去重 PASS 事件，失败/跳过/竞态均零；正式单 Gateway ELF 编译通过。测试核原字节、凭据与 epoch/deadline 不被重写，坏 DTO 仍由原 typed native 解析拒绝；错方法、近似路径、query、超限、错 actor、Owner 或 ConfigUID 漂移不得到达后端。测试后端是明确夹具，不是 PG 执行证明。

Linux /data 原始日志和产物按收据归档，3CPU/3GiB/swap0 有界运行已终态并释放自有预算。下一步仅替换 Gateway 镜像、三节点预载和原 Helm 发布，核实际 PG Owner/租约与资格路径。直接 Native initialization 的503另行定位，不能将本修复冒称其已恢复；Grow、安全 normal Shrink、Regrow、自动策略仍待真实链验收。
