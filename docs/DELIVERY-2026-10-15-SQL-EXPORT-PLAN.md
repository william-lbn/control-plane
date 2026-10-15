# 2026-10-15：XLSX 与执行计划树本地交付

## 结论与版本

两项官网 SQL 工作台方向的功能已完成代码、Linux 单测、TypeScript/Vite 构建、Helm 部署和真实 UI 验收。合同、数据模型和手工步骤见 [XLSX](SQL-XLSX.md)与[计划树](SQL-PLAN-TREE.md)。不是全量官网功能或生产认证。

- API 0.36.0（本批未修改）；migration 032；Chart 0.9.22；Helm revision 88→89。
- 仅 Web 运行代码重新构建，源快照 2d932d59e8622b5cee6bf3f48c7f97e627330b5bd819bf4159fc3507107099f0，父 Git dc97bca。
- Web 镜像 neon.local/control-web@sha256:4cc77a42056e075cc2afeb6bf01a758764582ccc1882af036c43db0e1d9caa0d。
- API/Worker 继续使用 15bc5b7c78e85a0575a828aac7bd20cfaa437c58d74edaaa265959af2b239292 的 S3 条件验收版；Auth 保持 967e1b3 的已验收来源。没有重建或重测旧后端。

## 新增功能与验证

XLSX 使用固定版本 fflate 0.8.3，包的实际 SHA512 与 npm lock/pack integrity 独立比对。MIT 原文随开源源码及部署 Web 的 /licenses/fflate-0.8.3-MIT.txt 保留。早期候选 Web 缺少 public 静态目录的 Docker COPY，未部署；补齐后与计划树一起构建最终候选并验收。

XLSX 两项新 Node 测试通过；计划模型两项新 Node 测试通过。TS/Vite/Helm 和 40 个固定 suite 准入通过。新测试仅验证本批导出/计划边界，未重跑旧 Go/SQL执行/CSV/Auth/Storage UI。

### sql-xlsx.spec.ts

Linux Job **publication-ui-20261015143332**；4 项检查通过，实际新 SQL 请求 1 次：

- ui_real_sql_result_has_duplicate_headers_precision_null_and_unicode
- ui_downloaded_xlsx_zip_and_all_xml_valid_no_formula_precision_loss_or_second_sql
- deployed_web_retains_public_dependency_license
- owned_compute_stopped_original_data_and_workbook_evidence_retained

### sql-plan-tree.spec.ts

Linux Job **publication-ui-20261015143447**；4 项检查通过，实际新 SQL 请求 2 次：

- ui_real_explain_tree_displays_estimates_without_fabricating_actual_rows
- ui_plan_nodes_collapse_and_expand_without_additional_sql
- ui_real_analyze_tree_has_actual_metrics_and_retains_original_json
- owned_compute_stopped_no_application_data_modified

XLSX 的实际下载文件保留，逐项检查 ZIP 中所有 XML 和关系，明确无公式/外部关系；真实重复列名、大整数字符串、NULL 与 Unicode 保留，下载没有额外 SQL。独立浏览器 XML parser 检查格式，不只依赖导出库自身解压。

计划树用 PostgreSQL 实际 Limit→Sort→Function Scan 的 Explain/只读 Analyze；显示真正估算或实际指标，折叠/展开未产生 SQL，原始 JSON 保留。最终托管 Compute Pod/VM 均为空，原数据库/角色/对象/查询历史与全部证据保留，没有修改应用数据。

## 部署保护与限制

升级前 metadata 备份 SHA256 91a160d499802d0816a88ec9bf251d3b42b0f3e2200ece35167f5d3ce1fa1a47；9 个 PVC、779 个既有非 Helm Secret UID 和其他 6 个 release 保留。Linux 本地构建、三个节点串行摘要校验/导入，没有触发远程 CI。

XLSX 所有非 NULL 值显式为文本，避免公式执行与精度损失；不是数据库备份或自动类型还原。当前没有逐一人工认证所有 Excel/LibreOffice 版本。计划树是基础有界树视图，不是 AI 调优、计划 diff 或索引推荐。项目级共享历史/保留策略、物理 RAM 归还、严格 Guest CPU 隔离、HA/DR、跨实例栅栏与完整可信 TLS 仍按独立门槛推进。

维护者证据：control-plane/production/evidence/20261014-auth-mail/xlsx-*、sql-tools-*、sql-xlsx-ui-local2、sql-plan-tree-ui-local2。原 npm 探测确认宿主无 npm，随后使用已有固定 Linux Node 镜像的非 root 临时 Job 完成依赖锁定；没有安装 Windows 运行依赖。
