# 2026-10-16 当前交付：个人历史名称与 AI 命名

## 1. 本批结果

本地 **API/Web 0.46.0 / migration 038 / Chart 0.9.36 / Helm revision 108**，
**104 paths / 149 operations**。此前 AI 修错已在
`f2c01b86b0b7ec8be6f36879be495f9c1ede5bfe` 推送，独立回执见
[0.45 交付](DELIVERY-2026-10-16-SQL-AI-REPAIR.md)。

本批实现并验收：

| 功能 | 实际验证 | 边界 |
|---|---|---|
| 个人历史名称 | 加密保存/清除、刷新可见、原记录关联 | 不改原 SQL/执行状态，个人名称不随 SQL 共享 |
| 并发和恢复 | 真实成功响应丢失，原 key/版本/输入取回完全相同回执；第二 UI 页面旧版本被拒绝 | 元数据变更；原回执不重施旧名称，刷新丢失本页意图时需按当前状态/原 key 核对 |
| 真实 AI 命名 | 明确读取原历史，精确发送预览/确认，本地 Qwen 1.5B 生成，人工采用后另行保存 | 不默认发送所有 SQL，不认证模型语义准确率 |
| 权限与留存 | 当前 Editor+本人、API key 拒绝、其他 Admin 不越权、加密/审计/回执、删除级联 | 备份/WAL 仍有各自保留；未知执行结果不改名 |
| 无数据面副作用 | 真实 UI 全程普通 SQL POST 0、Compute/VM 保持 0 | 模型调用产生自己的既有 AI 用量账本 |

[名称合同](SQL-HISTORY-NAMES.md)定义完整接口、密文模型、组件/时序、错误、人工步骤与
官网差异。新增表 `sql_history_name_requests`，原 history 增加独立名称版本、密文、
密钥版本和更新时间。幂等回执/审计不保存名称或 SQL 明文。

本阶段完成 Q03 的基本交互：计划比较、受控 SQL AI 草稿/Schema、计划建议通路、
修错及显式历史 AI 命名。默认自动命名/长期授权和模型质量等扩展仍保留。
其余[需求账本](REMAINING-REQUIREMENTS-2026-10-16.md)与生产门槛继续推进，**整体未完成**。

## 2. 精确构建与部署

- 构建父提交：`f2c01b86b0b7ec8be6f36879be495f9c1ede5bfe`；运行源码由不可变本地文件快照确认。
- API/Web 源快照 SHA-256：`de9216590f99523b2fdf09594c32593dc97839aeb34c3fb1823d20a99220dea4`。
- API/Worker 镜像：`neon.local/control-api@sha256:1a34a458c9aa25fb8bbb9c6ad3b39442c6c8e599370e64e0c84e151d23299a09`。
- Web 镜像：`neon.local/control-web@sha256:22240817f0843239983f83f8c6ca3eaf3f0a40bb85bc2fa8cefab619333fe544`。
- metadata backup SHA-256：`6cfe79ebbe8e2c3f952a2bd91a80c4dac900d7fa445cc13b7e56a5541468cb08`。
- 最终 UI Job：`publication-ui-20261016055149`。

仅在 Linux 构建 API/Web，三节点串行 digest 导入。Helm 升级前保存 metadata、
values、manifest；迁移 038 为加法式，保留 9 个 PVC、既有 Secret UID、其他六个
Neon 发布和运营方模型配置。最终实际 API/Web 构建输入与当前源码重新核对一致。
未更改 Neon Rust 或 Autoscaling 内核代码，没有 Python 产品后端/公开 Python 验证器。
最新本地镜像尚未统一公开发行；公开统一 Helm 0.1.31 的 0.23.0 镜像锁不能冒充本地版本。

## 3. 本地验收与失败保留

2 项新 Go race/真实 PostgreSQL 测试通过（其中集成测试包含多项 API/事务场景）：
名称字符/UTF-8/Unicode 换行限制，AEAD 跨项目/账号/记录/SQL 信封隔离，当前账号与
Admin/Viewer/外账号/API key 负例、确认终态、审计失败回滚、精确重试与内容冲突、
并发版本冲突、旧回执不重施、密钥缺失/轮换、60 新意图限额、回执复合外键和
删除级联。实际 HTTP 字段与闭合 OpenAPI 模型核对；Go vet 与三项 OpenAPI 检查通过。

前端新增 3 项纯函数检查、受影响的既有 9 项助手/修错检查通过；类型/格式/Vite
通过。52 个可信 live suite 声明/准入对齐，API 操作索引生成/核对通过。没有重复
其他旧全量套件，也没有等待 GitHub CI。

真实 UI 8 条 ledger（含收尾）通过：

1. 原 history `qh_aae3c2f2e83e5e41c7c81487` 从版本 0 开始，不创建替代 SQL 记录。
2. 第一次服务器已保存名称但浏览器丢弃响应，原 key/版本/输入/回执留存；重试完全
   相同请求，回执相同，当前版本只到 1。
3. 另一 UI 页面用旧版本提交返回 412；重载看到胜出名称，无静默覆盖。
4. 主页面清除名称到版本 2，原 SQL/执行状态保持。
5. 明确读取该 history 的原 SQL，SHA-256 与保留输入一致，不使用未保存编辑器。
   真实模型请求 `air_11f49fbe4d39313c83195014`，生成/采用均未写入名字。
6. 用户独立保存原模型建议到版本 3，刷新后同 ID 名称仍在。

共 **5 次名称 PUT**（含一次响应丢失的原成功、一次同 key 重试及一次 412），
**普通 SQL 请求 0、模型请求 1**。名称和原失败/成功历史、所有业务字节保留。

首个前端 gate 的 12 项纯函数检查已通过，随后 TypeScript 指出测试闭包的可选私密
目录类型，补充已校验的 string 绑定后通过。首个 live Job 在登录/写入前因私有
执行器的 fixture 白名单漏传新增 history ID/hash 而停止；修正私有参数白名单后
复用完全相同的原 history 验收，未对产品或数据做替代。原失败日志保留。

## 4. 收尾与下一项

两个临时 PG Pod 已各自保存原日志并按 UID 退休；4 个本阶段终态质量/UI Job 同样
先归档、再按 UID/resourceVersion 退休。最后 3/3 节点 Ready，无 Pressure，87 个
工作负载与期望一致（含有意的 0），错误 Pod 0、托管 VM 0。模型服务、原配置、
测试历史、失败、文件、源码/OCI 均保留。

维护者完整证据在 `control-plane/production/evidence/20261016-history-names`，
口令、DOM、原模型输出及私密工具不上传公共 Git。下一项进入 **F02 Functions
定时触发器**的代码、UI、持久调度/调用回执与串行本地验收；阶段提交后继续工作。
