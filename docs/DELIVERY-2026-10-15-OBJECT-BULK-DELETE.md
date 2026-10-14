# 对象批量删除本地交付

已验收：API 0.27.0 / chart 0.9.11 / Helm neon-control-plane revision 76。

源快照 SHA-256 `70c5de815fc2327007436b56a6cd3c9efd760b792ff3939c119435f0143bfee0`；父 Git revision `5449a11` 仅为继承关系，未代表运行中的工作区修改。

API/Worker `neon.local/control-api@sha256:77756deceb9535fd1edb19dda0ecdf9c5ea29a19c5d5b46f8c18d7e938d1dbd9`；Web `neon.local/control-web@sha256:790a6ad59ccb51dccb01e32e46aa11f8f320f992b0c3c408ac2f4c3f397321de`。Auth 沿用已验收镜像。

Linux 实际 PostgreSQL/race/vet 全量检查 799 pass / 0 fail / 0 skip。UI Job `publication-ui-20261014173123` 的 8 项检查全部通过，包括过期版本整批不变、前端删除所选、真实 MinIO SDK DeleteObjects 与不存在 key、保留控制对象实际内容、停止服务与 Compute 缩 0。

保留 bucket `ci-s3-bulk-1791999086317` 的 `keep.txt`；四个测试辅助对象仅删除当前目录记录，物理历史与私有 payload、全部证据保存。Operations `op_eb854c1ffce8fa52cc992193`、`op_464835ddac898c53efb9b1c1`。全部 9 PVC 及 768 个原非 Helm Secret 身份保留，其余 6 个 Helm release manifest 未变化。

证据目录：本地忽略目录 `control-plane/production/evidence/20261014-auth-mail/bulk-delete-ui-local-attempt1`；源码不含密钥与私有测试材料。公开源码和镜像待整批通过后统一发布；该本地验收不等同生产准入。
