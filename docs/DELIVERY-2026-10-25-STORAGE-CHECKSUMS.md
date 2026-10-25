# 2026-10-25 完整对象校验持久化验收

CRC64NVME 与五种完整字节校验值的事务保存、读取、复制/覆盖基线已完成。五组新增 Go/真实 PostgreSQL/签名检查通过，八组受影响检查通过；没有重做原 17 MiB UI/SDK 上传或性能验证。见 [合同](STORAGE-CHECKSUMS.md)和[脱敏回执](acceptance/2026-10-25-storage-checksums-functional.json)。S02 的 5 GiB 和 multipart 扩展协商仍缺，整组继续。

真实服务上四个新 SDK 检查点通过：CRC64NVME PUT 后 HEAD/GET/属性读取五种正确值；错误 CRC64 返回 BadDigest 且目标不存在；复制保留校验值、空对象覆盖替换旧值；单分片合并后的五种完整字节校验值与对象代次一致。复用已有 Boto3 1.34.46，通过标准签名钩子发送 SDK 模型尚未包含的 CRC64 头，读取实际响应头/XML，没有额外外网下载。

只复用原专用项目/数据分支和原 Writer，创建一个新私有桶、一个 self/storage:write 凭据及三个对象，最终对象大小为 9、0、9 字节。原六类存储清单排除新桶及新增默认列后的行数/摘要一致，原 17 张目录表全范围摘要一致。旧对象的新增 checksum 映射全部保留为空；没有历史字节回填、重传或 GC。专用凭据已撤销到 generation 2，原 Writer 已通过同一 suspend Operation 缩回零。

API 镜像在 Linux 构建一份，经内网导入三节点。第三节点首次 SSH 连接中断后只续传原归档，失败记录保留，没有重建或重新拉取。Web、Auth、AI、Gateway 和浏览器镜像复用。发布清单严格限定 API/Worker 两个镜像变更，其他资源和策略引用保持原值。

最终 API/Worker 0.87.0 / Chart 0.9.84 / Core Helm 183 / 元数据 schema 067；本次数据分支原生存储格式 5。924 个非文档构建输入和 523 个 Go/SQL 来源与当前源码一致；构建后只增加已通过真实接口验证的专用 SDK 探针，不改变运行镜像。原迁移 058–067 保持冻结字节。

最终三节点、三个 Safekeeper、七个关键数据组件 Ready；VM/Compute 与未退休 Functions 为零，30 个现存受管理 Auth 工作负载均为零。9 PVC、953 个原持久 Secret UID、147 条原 bootstrap 证明和另外六套 Helm 配置保留。校验质量 Pod 已按 UID 归档退休，密码、私有 SDK 配置和检查点保留在 Linux。

清理时专用凭据撤销已成功，随后只读证明 SQL 错用了不存在的 `jsonb_object_length`，修正查询后续验，没有重复撤销。最终守卫曾把八个指定 Auth 代次误作全部工作负载数；只读核对实际 30 个均为零后修正守卫，未改变任何 Auth 部署。这些采集错误和最初 SSH 中断记录均保留，不能描述成产品检查已一次全过。

CLI/MCP 上一提交 `76a7699` 的 GitHub 工作流已全部 15 项通过；本增量的托管工作流在提交后独立记录，不用它代替上述 Linux/实机证据。全项目仍有九个业务组和三个其他组未整体闭环；[后置容量验证](ENVIRONMENT-DEFERRED-VALIDATION.md)与仍缺实现项分开记录。
