# 2026-10-16 当前交付：受控 SQL AI 助手

## 1. 本批结果

本地 **Web 功能切片 0.44.0 / Chart 0.9.34 / Helm revision 104**。
API/Worker 保持 **0.43.0、migration 037、103 paths / 147 operations**，独立 Go AI
Gateway 与数据面镜像保持。公开产品仍为 Go/PostgreSQL + React/TypeScript。
上一批已推送 `ee5abc21bfcb03815f4db3fb7ae3fbd9b9e0a454`，见
[分段上传运维交付](DELIVERY-2026-10-16-MULTIPART-ADMINISTRATION.md)。

| 本批功能 | 实际验证 | 限制 |
|---|---|---|
| SQL 草稿 | 真实 Linux UI → Go AI Gateway → Windows 本地 Ollama Qwen 0.5B → 草稿 | 模型输出未验证，不自动提交 SQL |
| 受控 Schema | 当前角色 public 列目录、限量/截断、清除单次密码、独立发送确认 | 默认不读取，不含数据行/默认表达式/注释；元数据本身仍可能敏感 |
| 人工使用草稿 | 单独载入、密码为空、用户重新输入密码/执行，实际 Neon Proxy 返回 42 | 草稿格式校验不代替数据库语法/权限校验 |
| 计划建议 | Explain 数值/运算符白名单摘要，真实模型文本返回，无额外 SQL | 只验通路和数据边界，未认证模型建议准确率 |
| 上下文 | 精确请求预览、变化重置确认、连接范围/重载清除上下文 | 不持久化本页提示/输出；主动执行/保存 SQL 适用各自历史策略 |

详细架构、消息/数据模型、授权/API、故障与手工步骤见[SQL 助手合同](SQL-ASSISTANT.md)。
**AI 修错、历史查询 AI 命名、动态供应商管理和其他协议等仍未完成**；不把本切片
当作所有 SQL Editor 或官网能力完成。全量剩余任务见[账本](REMAINING-REQUIREMENTS-2026-10-16.md)。

## 2. 构建、部署和源码追溯

- 构建父提交：`ee5abc21bfcb03815f4db3fb7ae3fbd9b9e0a454`；运行源码以不可变本地文件快照为准。
- Web 快照 SHA-256：`98e730dd2caf90167dfb13f1708f0a663a633064e5958ebafde16f72a3399ed1`。
- Web 镜像：`neon.local/control-web@sha256:4803578552c180ebedae5d604bc03e262b87a9998dd7149cc52f1a4881a7e00c`。
- 保留 API/Worker：`neon.local/control-api@sha256:5a0bcdf294ae7b35120e76f49327002b22e625efa5405dc74c2aeb1ea950449c`。
- API/Worker 快照：`18f34cad7281b7db8137f964a12bde203e5d1b9e9080cae10912c6b95693a3c4`。
- 最后 metadata backup SHA-256：`7ceba7bac25b130a2f32f75577fc87c06de4aa298b0880fd16d43e96905a580c`。
- 最终实际 UI Job：`publication-ui-20261016050239`。

仅 Linux 构建 Web、三节点按 digest 导入；两次窄 Helm 升级保留 9 个 PVC、全部既有
Secret UID 和其他六个 Neon 发布。最后重新核对当前源码的 API/Web 实际 Docker COPY
输入均与对应运行快照一致。无 Neon Rust/Autoscaling 内核修改，无 Python 产品代码。
此批镜像尚未公开发行；公开统一 Helm 0.1.31 候选仍锁 0.23.0 控制镜像，不能代替
本地运行版本。未等待 GitHub CI，未重复旧全量 UI 套件。

## 3. 验收和保留的失败

6 项新增 Linux 纯函数测试覆盖上下文选择、Schema 字段/大小/截断、Explain 与实际值
区别、计划白名单/体积、草稿格式和截断/tool/多 choice 拒绝；类型、格式、Vite 通过。
公开 workflow 声明与准入对齐为 50 个 suite，按最终选中的 suite 准备依赖；仍单
worker、零自动重试。50 是可执行测试入口数，不是 50 个新功能。

真实 UI 完成 8 条 ledger 检查（包括资源收尾），共 **3 次数据库 POST**：
固定 Schema 目录读、人工草稿执行、显式 Explain；**2 次模型 POST**：草稿和计划建议。
request_id 为 `air_6b68b42011476a485f5a3e64`、`air_42ecb7431a371db335ea451a`。
原 project/branch/Endpoint/业务数据不替换，最终 Compute/VM 均为 0。

失败 1：模型返回 502，查明原 Linux 转发 transient service 与 SSH 反向通道退出，
Windows Ollama 进程和最小模型仍在。恢复原 relay 二进制、原凭据和受限 loopback
通道后继续；没有换用假供应商。

失败 2/3：最小模型把 JSON 上下文复述成输出模板，一次缺必需草稿字段、一次达到
256 token 上限；前端均拒绝载入，没有执行该输出。保存原始响应/request_id；将
固定 system、JSON 参考数据、自然语言任务拆成独立消息并要求单一 SQL 围栏后通过。
缩短测试对已明确错误的等待；没有放松截断或字段检查，也没有人为改写模型草稿。

计划建议截图还观察到小模型把估算数值称作执行时间。因此该通过仅证明真实返回和
上下文隔离，**不能当作性能分析正确性验收**。界面提示人工核验、原始计划保留；
后续继续增强确定性的估算/实际值提示。实际耗时应以用户执行 Analyze 的 PG 数据为准。

维护者完整原件位于 `control-plane/production/evidence/20261016-sql-assistant`；
供应商原输出、DOM、口令输入、SSH/OCI/失败记录不进入公共仓库。

## 4. 资源和后续

本阶段 8 个自有终态质量/UI Job 均先保存描述/日志，再按 UID/resourceVersion 退休；
业务文件、历史、Schema、模型和镜像归档保持。最后 3/3 节点 Ready，无 Pressure，
87 个工作负载符合期望副本（含有意的 0），错误 Pod 0、托管 VM 0。

本批提交后继续 AI 修错及其余未开发能力，逐项实现和本地验证；阶段验收不表示
整体完成。生产多租户、分布式栅栏、HA/DR、全链路 TLS 等门槛保持独立。
