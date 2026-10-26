# 引用安全对象清理

本增量实现 **prospective、从未发布的失败上传对象清理**，以及完整历史回收所需的严格证明合同。S03 整组仍未完成：已发布对象、历史版本、成功上传后目录提交失败的对象和未知结果继续持有。现有业务对象不会被扫描收编或删除。

## 当前可执行范围

控制面迁移 `071_storage_gc.sql` 新增四张保留证据表。`storage_gc_allocations` 在 backing PUT 前持久登记确切 store、project、随机 blob/part key、allocation ID 和大小；没有登记成功就不发 PUT。新 PUT 使用 `If-None-Match: *`，并在 backing 私有 metadata 中携带不可预测的 allocation ID，防止随机 key 碰撞时误认旧业务字节。`storage_gc_attempts` 追加保存每次删除/核对结果，后续核对不会抹去原来的未知回执。

| 分配状态 | 形成条件 | 物理删除 |
|---|---|---|
| 无记录 | 启用前的对象、其他 namespace、外部写入 | 永久保护，不收编 |
| `uploading` | PUT 前登记；中断或登记/解析结果未知 | 永久保护，不按时间转为 orphan |
| `may_reference` | PUT 成功且在允许目录发布前持久登记 | 永久保护，包括原分支、子分支、COPY、PITR、保留删除、multipart |
| `never_published` | backing PUT 返回错误或大小确认不符，调用者不会进入目录发布 | 满足独立策略和确切 backing 身份后可清理 |

成功 backing PUT 返回调用者前必须提交 `may_reference`；状态确认失败时调用者得到错误，不发布目录，原状态仍保护。失败 PUT 即便已经写入部分/完整 bytes，也没有返回可发布引用。终态不能回退、修改身份或删除证据；原 key 不能重用。Functions 的独立内容寻址 namespace 不参与本回收。

已发布引用全部保护，因此克隆、历史 Timeline、PITR lease、reset 备份、保留删除和共享物理对象都不会因为当前分支清单移除了引用而被删除。multipart 的成功上传分片同样是 `may_reference`；中止不释放其物理字节。成功 backing PUT 后失败/未知的目录提交也保持 `may_reference`。这是一项保守实现边界，不代表已经完成历史版本或中止上传分片回收。

普通 Object Storage client 保持既有 Get/Put 权限。journal 使用独立、最多两个连接的 metadata writer pool，避免所有请求持有主 metadata transaction 时等待第二个主 pool 连接。进程退出调用 `closeStorageGC`。独立 gateway 只需 journal 权限，不需要删除 credential。

## 显式计划、执行与回执

三个接口均经 Console session 身份和既有 CSRF 中间件；另外要求独立 GC policy 中的操作员和精确 project allowlist。组织 Owner、普通 Admin、AI/服务计划操作员及 API key 都不自动获得 GC 权限。操作时再次确认操作员账号未停用。

| 方法与路径 | 请求 |
|---|---|
| `POST /api/v1/platform/storage-gc/projects/{project}/runs` | `Idempotency-Key`；`{"limit":1}`，范围 1–100 |
| `GET /api/v1/platform/storage-gc/projects/{project}/runs/{gcRun}` | 查看持久计划及每项结果 |
| `POST /api/v1/platform/storage-gc/projects/{project}/runs/{gcRun}/execute` | `{"confirm_run":"sgr_…"}`，必须等于已审阅计划 ID |

没有后台自动清扫。计划只纳入 `never_published` 且经过策略最小等待时间的对象；此等待时间用于运维隔离，不是历史引用证明。相同 key/input 返回原计划，改变输入返回 409；原计划成员不可变，读取显示当前逐项结果。返回 allocation ID、大小和状态，不返回 backing key、credential 或私有 metadata。

执行先验证 backing bucket 从未启用 versioning；Enabled、Suspended、读取失败都拒绝。每项在 metadata 分配行锁下重新确认不可发布状态和最小等待时间，并 HEAD 验证唯一 allocation marker、大小、ETag 和空 VersionID；不匹配记为 `held`，不删除。不同计划/进程对同一 allocation 串行处理。

对 backing 发请求前，通过独立 journal 连接先提交不可变 `intent`；请求取消或进程中断导致结果事务未提交时，该未知尝试仍保留。独立提交只取 item 外键的 KEY SHARE，collector 使用 NO KEY UPDATE 锁，避免自相阻挡。每次完成再追加 `result`，保留前次未知状态。

DELETE 后必须再次 HEAD 确认 `NoSuchKey` 才记 `deleted`。网络错误/权限拒绝/未知结果记为 `delete_unknown`，保留原计划，可重放原执行并先检查确切对象。执行前已不存在记 `absent`，不冒充已删除字节；可继续复查。`deleted` 重放不重复 DELETE。结果是逐对象回执，整次 HTTP 200 不代表每项都删除成功。不会把失败信息原文、S3 secret 或业务对象 key写入公开响应。

## 配置与最小权限

迁移先于新进程部署。在 API、Worker 和独立 Storage gateway 的 `loadStorageBlobs` 成功后调用 `configureStorageGC()`，并在退出时 `defer closeStorageGC()`。未配置普通 Object Storage 时无动作。启用 journal 后，如果迁移/数据库不可用，新 backing 上传失败关闭，不绕过登记。

仅在需要人工清理的 API 实例挂载独立 `NEON_OBJECT_STORAGE_GC_CONFIG_FILE`。文件示意中的值均为占位符：

```json
{
  "backing": {
    "endpoint": "https://product-store.example.invalid",
    "bucket": "neon-product-fixture",
    "region": "us-east-1",
    "access_key": "separate-scoped-gc-identity",
    "secret_key": "mounted-outside-source",
    "lab_http": false
  },
  "operator_ids": ["approved_operator_id"],
  "allowed_project_ids": ["prj_0123456789abcdef"],
  "minimum_age_seconds": 86400
}
```

允许 1–16 个操作员、1–128 个精确 project；不提供通配所有项目。最小等待时间 60 秒至 30 天。endpoint 与 bucket 必须等于普通 product store；独立 GC credential 只授予该专用桶的 `s3:GetBucketVersioning`、`s3:GetObject`、`s3:DeleteObject`，无 List/Put、Pageserver bucket 或 root 权限。未挂载该文件时三个 GC 接口保持 disabled，journal 仍启用。

普通 gateway metadata 身份新增 `INSERT,SELECT ON storage_gc_allocations` 和 `UPDATE(state,resolved_at)`；不可变触发器继续限制终态和身份变更。GC 操作进程还需查询该表、创建/读取计划与 item、更新 item 结果，以及既有用户、项目和审计权限。控制数据库 DBA 属于可信运维边界，不向应用 SQL 用户开放这些表或 GC credential。

## 完整历史回收的必要证明

`storage_gc_proof.go` 定义内部 adapter 合同和严格检查，`assets/storage-gc.sql` 是只读 current-directory inventory 组件，**不是原生格式升级，也不能单独证明历史无引用**。没有用户可提交 proof 的 HTTP 接口，没有 bypass 开关。

完整回收要求 native authority 提供受栅栏保护的全部 Timeline 清单，包括当前、子分支、已保留删除、reset 备份、离线和 metadata 未映射的 Timeline；每个 Timeline 下所有数据库都需完成身份核验和完整引用扫描。必须覆盖 objects、所有状态 multipart_parts、其他物理引用与未知结果；当前目录无引用的已提交观察 LSN 必须严格早于该 Timeline 的有效 `min_readable_lsn`。LSN 用数值比较；有效边界必须包含 PITR、保留窗口、子分支和 lease pins。栅栏需一直保持到 DELETE，阻止新增引用/克隆/reset/旧 LSN lease。缺项、离线、未知、未保护的并发、重复/循环身份或任一引用都拒绝。

现有 deployed API 尚无这样的完整 inventory/fence/horizon 联合证明，`nativeStorageGCProof` 明确返回 unavailable。`may_reference` 没有转换为 orphan 的路径；metadata branch 列表、项目过期、墙钟年龄、applied GC cutoff 和一次 current manifest 查询都不能替代该证明。完整已发布历史回收仍需 native 实现与独立新增功能验收，不能以本增量关闭 S03。

## 新增验证与验收边界

新增 `TestStorageGC*` 单元组覆盖 ownership、重复 marker、错 size/version、权限/网络未知、DELETE 后确认、namespace/store 隔离，以及历史/子分支/retained/MPU/未知结果/数值 LSN/不完整 inventory/fence 等 proof 拒绝路径。新增 `TestStorageGCPostgres*` 只在独立 `control_ci` schema 中验证前置登记、终态证据、计划幂等/冲突、权限/跨项目拒绝、执行重放和未知结果核对；backing 是专用测试 HTTP/fake，不接触 live 业务对象。

主代理已完成 4 组新增纯函数、5 组 PostgreSQL 检查、vet 和相关编译，详见 2026-10-27 增量回执。尚未部署或执行 live 删除；源码检查不替代实机证据。现场物理删除功能只允许使用全新专用 project、allocation 和 backing bytes；既有业务数据、原 Secret/PVC、历史对象均保持。吞吐、压力、完整 5 GiB、跨实例故障可靠性、HA/DR 与完整历史 GC 另有门槛。
