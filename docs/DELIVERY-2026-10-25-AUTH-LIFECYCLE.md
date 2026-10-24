# 2026-10-25 应用 Auth 保留生命周期验收

A05 的保留生命周期基线完成：活跃 Auth 随叶分支/项目删除关闭，项目保留恢复后继续 disabled，显式新代次重新启用拒绝旧会话和旧 JWT 签名键，并保留账号、密码与客户 SQL 数据。[生命周期矩阵](AUTH-LIFECYCLE.md)和[脱敏回执](acceptance/2026-10-25-auth-lifecycle-functional.json)给出边界。当前 18 组有基础验收，9 个业务组与 3 个其他组继续；五组专项后置，整个项目尚未完成。

本批只增加功能验证与文档，复用已通过的 0.84 运行镜像、Core Helm 178、Chart 0.9.81、应用协议 0.28.0 / schema 065 / 原生存储格式 4，没有重建镜像或重复部署。既有原生 PostgreSQL/维护库的代次重放、受限身份、TOTP 重封装、账号/组织/手机挑战/事件隔离回执保持有效。

新增两组 Go 检查通过：删除/恢复路由移除 Auth/Data API 服务身份并保留客户身份，恢复重放不复活旧服务登录；仅停用自有 Auth 工作负载，保留镜像/配置 Secret/Pod 模板与 resourceVersion，对跨项目、跨分支、跨 Endpoint 及未知写入结果拒绝越权重写。这两组使用 Kubernetes HTTP 对端；实际 Neon 删除恢复由独立浏览器检查证明，没有混称为原生 Kubernetes 验收。

实际 Linux 浏览器累计八项通过：创建专用项目与 SQL 保留行；UI 启用 Auth、注册用户、原会话/Token/JWKS；物理子分支自动继承启用中的 Auth 和原账号；UI 删除活跃子分支后其身份入口关闭、父分支原会话仍有效；UI 解除保护并删除活跃父 Auth 所在项目，原 Cookie/JWKS/Token 入口均关闭；UI 恢复项目后 Auth 仍 disabled，先前删除的叶分支不复活；显式启用第二代 Auth 后旧会话为空、原 JWT 公钥集合均不再出现，原用户 ID/密码和客户 SQL 行保持；最终主动关闭 Auth、Compute 归零，原始删除与恢复 tombstone 保留。

初次浏览器等待子分支创建超时，原六个创建步骤随后由 Worker 正常续跑成功，无手工重试、无第二套资源。观察到 Worker 曾暂未 Ready、容器已有重启，三个 K8s API 的读取间歇超时；恢复后仍使用原 Operation、时间线和 VM。续验先验证原父会话仍有效，再把原 Cookie 从 Linux 私有检查点恢复到新浏览器，前两项不重复。四项通过后，脚本把通用保护标签用于实际按钮，未提交项目删除；核对状态后仅中断自有 Playwright CLI，保留四项结果，修正准确按钮名并给页面动作设置 30 秒上限。新增测试的 Linux 类型检查与 92 套入口清单通过后，只续验末四项。失败/中断回执保留，生产超时与运行代码没有修改。

882 个未变构建输入、504 个 Go/SQL 文件和最终 Linux 检查过的浏览器脚本与验证来源相符；058–065 迁移摘要保持。原 17 张目录表摘要不变，排除范围仅增加本批专用项目两个分支/Endpoint；旧六类存储清单回执复用。六条此前 Auth 的代次和 disabled 状态保持；本批父 Auth 最终 generation 3、子 Auth generation 1 均 disabled，两条原时间线映射保持。续验前 945 个持久 Secret UID、9 PVC 及另外六套 Helm 配置保留，唯一新增为显式第二代父 Auth 配置。最终 946 个持久 Secret，临时 VM SSH 凭据正常回收，首次目录发布累计 145；三节点、三个 Safekeeper、七个关键组件 Ready，VM/Compute 为零。七个终态测试 Job 已按 UID/resourceVersion 归档退役。

密码、原 Cookie、Token、原始检查点与私有资源快照留在 Linux 保护目录。单独删除分支恢复、物理 GC、KMS 根密钥在线轮换和完整 Backend 时间点恢复没有由本批开放；性能、压力、可靠性、HA/DR 未执行。
