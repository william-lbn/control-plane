# 2026-10-23 S3 完整属性与分片快照交付

API/Worker 0.79.0 已部署，Chart 0.9.76，Core Helm 167。metadata schema 064 不变，原生存储以追加列升级至格式 4。Web 运行源码未变，继续复用 0.78 镜像；AI/Auth/Gateway 保留。固定 SDK 的完整 `GetObjectAttributes` 方法可以读取当前 multipart 的真实分片页，具体边界见[属性合同](OBJECT-STORAGE-ATTRIBUTES.md)。

| 验证范围 | 结果 |
|---|---|
| 新增 Go / PostgreSQL | 3 组通过：UTC 数值时区边界、分页/损坏拒绝、实际 SDK 的事务发布/分页/COPY/覆盖/旧回执重放 |
| 受影响检查 | 原生追加格式、multipart 原子发布、设置/COPY 三组通过；其他既有通过回执复用 |
| Web / SDK 工具 | 新测试格式、TypeScript、86 套入口门槛、API 索引及 Linux 探针编译/vet 通过；Web 运行字节未变，Vite 复用 |
| 实机 | 10 个唯一检查点通过；未重跑上一轮标签、POST、尾部校验路径 |
| 源码 | 436 个运行输入匹配归档，301 个 Control Go、12 个协议 Go 和 73 个 SQL 输入匹配最终编译来源 |

实机只创建一个新专用桶，使用新凭据发布含序号 2、7 的 multipart。完整 SDK 核对第一页、末页和越过最后一片的空页、总片数、整对象/分片 SHA-256、大小、最后修改时间和 null 版本。单次 COPY 保留原字节并清除 multipart 布局；普通 PUT 覆盖后返回新对象代际和空分片信息。控制台观察两个对象，新凭据最终撤销，测试 Compute 归零。原完成回执在覆盖后不重新发布旧字节/分片的路径另有真实 PostgreSQL 功能验证。

升级夹具首次把新增列计入旧行指纹，修正为比较旧列且单独检查新列默认值后，只补验该项。构建服务启动就绪超过初始等待窗口，随后只读确认原服务和缓存可用，没有再次重建缓存。部署前一次保护检查使用含糊的 VM 资源名，修正为完整 CRD 名并核对当前 VM/SSH Secret 的控制器 UID 后继续，首次检查未执行 Helm 升级。末次只读保全检查遇到一次冷醒 503 和一次 API 超时，保留失败记录，原范围补读成功；这些观察不作为可靠性认证。

## 部署与保全

源码快照 `0fefcd7c07833222b59e31041dbccea3626622314d0e2e8493452442e3507094`，归档 SHA-256 `e6d1bf9e5e4ead75de0ffcab69aa1870f9b960676b11874e9ae78cd40f9088b5`。只重建 API 镜像并从内网分发至三节点；其他五个镜像复用。API/Worker 镜像 SHA-256 为 `e29a8a32e81cdc4dfdbe217b30f3f44ad4800b726b7327cdcc89746c5d699f12`。升级前元数据备份 SHA-256 `5c3f38390211ab79456160fa6ad73f28e5193ce4fa86864b684f06db997423a4`。

24 个 Helm 对象逐项核对，仅 API/Worker 镜像实际变动。058–064 迁移冻结。17 张目录表无排除范围摘要一致；六类旧存储清单摘要一致，包含上一轮 S3 全部测试对象，只有本轮新专用桶排除在旧行比较之外。新列默认值独立验证。保留两个新对象、一次完成上传和两条原分片记录；没有推断或回填升级前的历史分片布局。

9 个 PVC、917 个原非 Helm Secret 身份、正式 BYOK 策略及其他六套 Helm manifest 保留。两个本轮终态 Job 已归档并按 UID/资源版本退休；Go PostgreSQL 测试 Pod 亦按 UID 退休。临时 Compute 的 SSH Secret 由控制器回收。最终三节点、三个 Safekeeper 和七个核心组件 Ready，VM / managed Compute / 非退休 Functions 为 0。

[脱敏回执](acceptance/2026-10-23-s3-attributes-functional.json)记录本轮范围。历史分片回填、完整兼容矩阵及其他业务缺口见[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)；性能、压力、可靠性和 HA/DR 后置。
