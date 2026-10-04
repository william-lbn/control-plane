# 来源、Fork 与发行对应关系

核验日期：2026-10-04。工作区拥有研究副本、fork 开发副本与固定发行源码三类目录；
它们的 HEAD 不同是正常的，部署必须依据固定 manifest。

## 1. 核验结果

| 工作区用途 | Clone 来源 | 核验 HEAD |
| --- | --- | --- |
| 原 Neon 研究代码 | neondatabase/neon | fa504217c61bbcaf5c512d75830564541f917f8f |
| 原 Autoscaling 研究代码 | neondatabase/autoscaling | a4bce09e4d6bd7932ea634a1ed79afe26a9cfd1f |
| Neon fork 开发 | william-lbn/neon | b03390fd08caa3c7c66680209e22e6b458426457 |
| Autoscaling fork 开发 | william-lbn/autoscaling | c0052f5f2d38fce6c70e448f3f1ee2ee239a0a93 |
| PostgreSQL fork 开发 main | william-lbn/postgres | ec17a49088347aac95c3d93207a0b0f0d4a75c8c |
| 官网文档研究快照 | neondatabase/website | c0d49cbb6979b2ce79ea502d62dbc40780a923b0 |

这些工作区核验时均无未提交源码改动。Fork 已包含 CI、镜像构建、
依赖下载兼容性等已提交改动；这些不能笼统称为 Neon 运行时 Bug 修复。
未发现需要另行提交的未提交数据面 Bug patch。

## 2. 当前数据面镜像发行

tag：`2026.09.30-162021-1f30cd02092d-r36742713551-a1`。

| 组件 | 固定部署源码 | Fork |
| --- | --- | --- |
| Neon | 1f30cd02092dc151f5d00aef97e7c629105b454b | [william-lbn/neon](https://github.com/william-lbn/neon) |
| Autoscaling | c0052f5f2d38fce6c70e448f3f1ee2ee239a0a93 | [william-lbn/autoscaling](https://github.com/william-lbn/autoscaling) |
| PostgreSQL 16 | a42351fcd41ea01edede1daed65f651e838988fc | [william-lbn/postgres](https://github.com/william-lbn/postgres) |

固定源码 clone 的 origin 均指向用户 fork；HEAD 与镜像 manifest 对齐。
开发分支 HEAD 不能替代运行提交。Compute PG16 digest 见 Helm values 中的 computeImage。

PostgreSQL 开发副本是浅克隆，单靠本地 `branch --contains` 会产生假阴性。
GitHub 服务端确认 deployed PG16 commit 是 fork `REL_16_STABLE_neon` 的祖先：
2026-10-04 分支 HEAD `59027122a97c2d91c1acabd5aa512b4bf72f6281`，
ahead 387、behind 0。[服务端比较](https://github.com/william-lbn/postgres/compare/a42351fcd41ea01edede1daed65f651e838988fc...59027122a97c2d91c1acabd5aa512b4bf72f6281)。

## 3. 后续 Bug 修复流程

```bash
git remote -v
git status --porcelain
git rev-parse HEAD
git branch -r --contains <deployed-source-sha>
# 浅克隆结果不充分时使用服务端 compare 或按需加深对应分支。
git switch -c fix/<specific-bug> <verified-baseline>
# 最小修复 + 注释动机 + 复现/回归 + 新镜像 digest。
git commit -s
git push origin fix/<specific-bug>
```

不得把新的 fork main HEAD 当作运行版本，也不得为不存在的 Bug 制造 patch。
本控制面在独立 control-plane 仓库演进；本仓库提交不修改上述数据面源码。

## 4. 控制面发布凭据初始化

2026-10-04，仓库所有者授权复用 Neon fork 已有的 Docker Hub Secrets。
新增 CI 配置经 [Neon PR #1](https://github.com/william-lbn/neon/pull/1)
提交、合并，fork main 更新为 `cf8f08bfb3fd31e2e0e0675632fa58a117fca2d6`。
这是发布配置变化；上表记录的是配置变更前核验值，运行数据面发行 SHA 保持不变。

[一次性作业](https://github.com/william-lbn/neon/actions/runs/37179367194)
只允许 owner 在 main 手动触发，接收公钥固定为 control-plane 仓库。
作业输出 Libsodium sealed-box 密文，运维通过 GitHub Secrets API 写入本仓库；
没有读取、复制或保存 Secret 明文，没有把 GitHub 登录凭据注入工作流。
本仓库两个 Docker Hub Secret 于 2026-10-04 05:16–05:17 UTC 配置完成。
Secret 存在与实际镜像推送成功是两个独立检查；镜像发布必须保留自己的 CI 回执。
