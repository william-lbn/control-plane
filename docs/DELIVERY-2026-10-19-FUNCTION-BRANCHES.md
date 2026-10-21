# 2026-10-19：Functions 分支复制与恢复

F05 本地基础功能已验收：显式复制、分支创建时冻结来源、无 Writer 延迟准备、默认暂停的触发器继承、历史 SQL 使用当前代码的明确确认、独立 SQL 身份与实际隔离、原失败操作恢复及正常退休。当前十二组有基础回执，其余 23 组仍有代码或独立验收缺口；继续处理 T01 中原实例管理证书自动续期的功能缺口。性能、可靠性、HA/DR 与外部 fencing 专项按用户要求后置。

## 实际运行来源

- API/Worker/Web **0.56.0**，migration **045** 已应用并冻结，Chart **0.9.48**，Helm **124 → 125 → 126 → 127**。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`；实际 API 本地不可变源码快照 `20a1c57992e1168627c5d00b78d6a913f9697e4ea7819848cdb21106b375aacd`，归档 `cc698b42beb487d5588cce4adb146e8a88f7faa85674df9694d1afd749a0b67a`。父提交不是部署源码，本地累计变更尚未统一公开发行。
- API/Worker `neon.local/control-api@sha256:e64011593f2c81420815262e871e22f112c914f2866a779572cba362031ffb27`。
- Web `neon.local/control-web@sha256:21c45c4d8889d9dcfe0e55965639bad64bc27b27c1215af58bbbe3c7a469b3e7`，来自快照 `152cca6d1584e0093dc91a7dd6cd75452334dce3473230a5ef8f78e6474c41cb`；76 个 Web 运行输入与最终源码逐项匹配。
- Guest 沿用 F04 的 `neon.local/control-functions-vm@sha256:4cc946dd4782d1ba95bb1837d47d5385da2a4c970c2601e86e8b8aae6e25ecd4`；41 个实际格式化编译/运行输入未改变。468 个最终来源输入核对通过，唯一差异是明确 SHA 的收尾测试修复；产品运行代码未由测试 override 改写。
- Linux 缓存构建，镜像只构建一份后在内网分发，三节点核对准确 digest/CRI；没有推送公共 registry，没有重新拉取整套基础镜像。
- migration 045 SHA `b6d743aa16ff9e58f5e459c1217e9ba8b288ea7d68a85fe486ae1136036cf6b1`，已应用版本不可再修改。后续数据库变更必须使用新迁移。

## 交付行为

显式复制固定同一项目中的来源分支、函数、完成部署、版本、调用者和持久幂等键。环境复制必须明确确认；环境值和 SQL 密码不进入公开回执。目标得到新的分支作用域环境 Secret/MAC、SQL 角色/凭据和部署，代码内容保持来源摘要；对象键按分支独立，未宣称物理去重。

创建普通子分支时，父函数及完成部署、环境名称、定时或对象触发器配置在原创建事务中冻结。没有 Writer 时仅保存不可变计划；Writer 就绪后按原身份准备，父函数后续更新不改变已接受快照。没有部署的定义只复制定义，不创建 SQL 凭据或执行实例。配额/权限不足保留计划及明确状态，不把已就绪的 SQL 分支改成失败。

全部复制默认暂停，不运行客户代码、不继承公开入口、输出采集、调用历史、实例、boot 或 SQL 密码。继承触发器保持同一分支作用域 ID、来源标记且 disabled/next_run_at=NULL；子分支修改和删除墓碑不会回写父分支或被准备控制器复活。

历史 SQL 分支需要额外确认使用当前冻结的函数代码。实际检查分别展示旧 SQL 数据、当前 v2 代码和环境，未将其解释成跨服务一致时间点恢复；L03 仍有独立缺口。

## 定向功能验证

私有回执位于 `control-plane/production/evidence/20261018-function-branches/`。所有失败、原 ID 和原数据保留，没有以新身份替换失败测试。

| 回执 | 实际范围 |
| --- | --- |
| `go-20261018160041` | 五个新增/受影响顶层 Go/一次性 PG 测试；持久重放、来源/权限/版本、真实对象字节及 Secret UID/MAC、延迟 Writer、配额回滚、冻结触发器及墓碑；编译、vet、三个 OpenAPI 检查通过 |
| `go-20261018164218` | PostgreSQL 无 GRANT OPTION 时仅给 warning 的真实场景；事务内检验实际 CONNECT/schema/table/sequence 权限，失败回滚，owner 修复后同一 scope 可实际受限登录/DML，CREATE 被拒绝 |
| `quality-d/e/f/g/h/i` | 最终增量的格式/TypeScript/Vite/64 个受保护套件入口准入及 API 索引通过；115 paths / 164 operations。未重跑 64 套旧业务 UI |
| `deploy-disabled` / `deploy-enabled` | 旧 dispatcher 全部退出后才启用复制；精确 24 对象清单允许镜像和指定开关变化，9 个 PVC、834 个原 Secret UID 和其他六个 release 保留 |
| `images-c` / `deploy-grants` | 只更新 API/Worker 的实际 SQL 授权完整性检查；9 个 PVC、851 个当时既有 Secret UID、Web/Guest/common 配置和其他 release 保留；控制库升级前完整备份 `1ae34201155c6cadd981aca3040ebd800f120a64588d81bcc0ab00bd7c727ba7` |
| `ui-a/b/c/d` | 同一原项目累计七个唯一真实 Console/Neon/Guest 检查；新测试分支上的 v1 实际 SQL、无 Writer 冻结、父更新 v2 后延迟继承仍为 v1、新 SQL 角色及子写隔离、丢失真实 202 后同请求恢复、原失败操作修复续接、历史 SQL/v2 确认、全部自有函数退休和 Writer 缩零 |
| `original-branch-copy-readonly-proof` | 七个定义已删除、八个完成部署和环境 UID 保留、三个暂停复制部署从未产生实例、五个后续实际 Guest 均退休、四个冻结计划/两个仅定义计划、三个禁用触发器墓碑、四个独立 SQL UID、三条原生时间线仍 Ready；31 个原操作成功、原保留函数数量仍为一、三节点 Ready、VM 零 |
| `runtime-source-match.json` / `owned-terminal-jobs-retirement-final` | 最终实际源码逐项一致；本片 13 个自有终态 Job 全部按准确 UID/RV 留档退休（五个先前已退休，八个本次退休），未删除 PVC、业务数据或 Secret |

原复制操作 **op_ca0b396a876965a1** 首次在 SQL 准备阶段失败。实测发现 PostgreSQL 对不可转授的 GRANT 可以只给 warning，修复公共受限身份准备器，在原事务提交及 Proxy 发布前核对实际完整权限。原操作第二次给出 **function_database_grant_required**，owner 明确修复授权后，第三次沿用原函数、部署、环境和 SQL Secret UID/MAC 成功；新实例只在随后的明确唤醒操作中创建。

ui-a 的 owner REVOKE 夹具缺少 CASCADE，在复制前被 PG 拒绝；ui-b 保留旧通用错误；ui-c 已完成六项业务检查，最后页面精确 label 未匹配含 option 文本的 Endpoint 选择器，原自有测试 runner 经 SIGINT 停止并保留检查点。只修复测试选择器和等待边界，ui-d 在同一原六项检查点续测收尾 **publication-ui-20261019105459** 通过，没有重建分支或重复已通过业务检查。

本片关闭 F05 基础功能。跨项目代码共享、统一物理去重、任意历史代码时间点、全链路 TLS、计费/独立 edge、性能和可靠性专项不由这些回执代替。下一项短期管理证书续期属于运行功能缺口；当前验收不声称原实例证书已能自动续期。
