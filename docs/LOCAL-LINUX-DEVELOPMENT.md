# Linux 本地开发与验收

## 交付顺序

每批功能依次执行：代码与契约 → Linux 定向测试 → 本地 OCI 镜像 → Kubernetes 部署 → 真实浏览器验收 → 批次回归 → 提交并运行远程 CI。远程发布不是本地开发的前置条件。新功能失败须保存原始输入、操作 ID、日志和截图，修复后继续原案例。

本地镜像使用 `neon.local` 名称和内容摘要；**只在已导入镜像的节点可用，不能作为公开安装镜像**。提交发布时用 Docker Hub 的公开摘要替换本地覆盖值。源码父提交不能代表未提交源码；本地构建记录完整文件 SHA-256 和快照 SHA-256。

## Linux 构建

运行在有已验证 BuildKit、Node 24 和足够临时空间的 Linux 上。BuildKit 应限制并行工作为 1，限制 CPU、内存与磁盘 I/O，保留基础数据库资源。禁止一边构建一边运行有状态功能验收。

```bash
node tools/local-images.mjs build \
  --source /opt/neon-control/source \
  --output /var/lib/neon-control/builds/attempt-001 \
  --components api,web,auth \
  --buildctl /opt/buildkit/bin/buildctl \
  --buildkit-address unix:///run/buildkit/buildkitd.sock
```

目录必须不存在。工具复制一份不可混入凭据的源码快照，按 `containers/builders.lock.json` 锁定基础镜像，串行构建并保留 OCI 包、摘要、源码清单和私有日志。失败不会覆盖上次记录。只构建受影响的组件；API/Worker 共用 `api` 镜像。未修改组件继续使用已验收的镜像，并在批次报告中分别记录来源。Go 模块与编译缓存由 BuildKit 管理，不进入运行镜像。

### 构建代理的故障定位

Linux 构建仍可能依赖配置的外部 HTTP 代理；这属于构建网络依赖，不能误报为
应用编译错误。若 BuildKit 在基础镜像 `load metadata` 阶段失败，先保存原始
构建日志，再检查代理监听与完整上游链路。`CONNECT ... 503` 且响应含
`Proxy-Agent` 通常表示代理未能连接下一跳，不等于 Docker Hub 已返回 503。
只看到 Linux 本地代理端口存活，也不能证明 Windows/另一台跳板的转发端口存活。

```bash
# 使用实际配置的代理，不在终端命令中放用户名、密码或 token。
curl --proxy http://127.0.0.1:7890 -I --max-time 15 https://registry-1.docker.io/v2/
```

能够完成 TLS 并收到 Registry 的 Bearer challenge（通常 HTTP 401）是匿名连通
探测成功，不能把它解释成产品账号无效。对照代理配置检查下一跳地址/端口，
恢复原来受限的管理网转发；不要更换镜像 digest、关闭 TLS 验证或开放公网代理。
新一次构建使用新输出目录，复用同一源码归档和编译缓存，保留旧失败。
若现场使用 Windows 转发，它仍是当前构建的外部依赖；要实现 Linux 独立构建，
需配置可持续运行的 Linux 出口或镜像代理，并另行验证。

## 导入与部署

将整个 `attempt-001` 镜像目录复制到每个可调度节点的相同绝对路径，然后各节点执行：

```bash
node tools/local-images.mjs import \
  --output /var/lib/neon-control/builds/attempt-001 \
  --ctr /var/lib/rancher/rke2/bin/ctr \
  --containerd-address /run/k3s/containerd/containerd.sock
```

工具校验每个 OCI 包 SHA-256，并确认 `k8s.io` namespace 中存在精确摘要引用。不要将本地镜像设置为 `Always` 拉取。节点替换或镜像 GC 后须重新导入。

将 `result.json` 的 `api/web/auth.reference` 写到私有 Helm 覆盖文件的 `api.image.reference`、`web.image.reference`、`managedAuth.runtimeImage`。保留数据库、Auth SMTP、Functions、AI、对象存储所有现有 Secret/PVC 配置。先 `helm lint/template`，查看实际 manifest 差异；仅升级控制面 release。数据库/PVC 不随代码版本重建。记录升级前 release 修订与资源 UID，用 Helm 已知上个版本回滚。

## 验收与发布

- 测试使用该构建快照中的 TypeScript，用已缓存的 Linux Chromium 执行；浏览器依赖镜像和产品镜像分别记录。
- 新功能优先做定向真实协议与 UI 测试；仅在剩余风险或发布门槛要求时扩大回归。
- 会话/一次性凭据/授权/数据完整性测试必须读取真实状态；不能只检查成功提示。
- 记录完成后停用该阶段测试服务、Compute 缩为 0，保存数据、邮件、操作和复测文件。仅清理有 UID 证据的临时资源。
- 全批通过后再提交代码、统一 CI 与公开镜像发布。公开锁仅接受可匿名拉取的摘要，不提交 `neon.local` 覆盖文件或私有凭据。

## 独立 SDK 工具与编译缓存

`node tools/local-images.mjs probe --source /opt/neon-control/source --output /var/lib/neon-control/builds/probe-001` 只导出同源 Linux SDK 二进制、文件摘要与回执，不构建或滚动 API/Web。用于诊断/修正测试工具；二进制不得冒充另一个版本，须记录原运行源码和工具摘要。

维护者的受信任 Linux 开发测试可缓存 Go 模块与编译结果。缓存目录使用专用 UID、版本范围与 checksum 验证，不共用于不受信任 PR。测试始终 `-count=1`；缓存编译不会缓存业务验收结果。基础组件保留 CPU/内存/I/O，功能测试串行。
