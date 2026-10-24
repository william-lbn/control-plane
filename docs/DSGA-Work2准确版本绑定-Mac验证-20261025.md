# Work2准确版本绑定

保留既有V3服务分支及独立主线维护5ba50b8，网关固定Work GET路由本批未改。
准确绑定已发布AS `e539f2f336b5d63e58556a38447c32e5dc8ea0a9`，伪版本
`v0.0.0-20261024203629-e539f2f336b5`，下载Origin Hash逐项一致；仅新增对应两行
sum。Mac readonly vet通过，未重复未变路由业务；日志在工作区build目录
`dsga-prestart-cp-module.json`、`dsga-prestart-cp-vet.log`。

真实Work2源由Neon PG17 9db3在Postmaster fork前登记；Neon/Go实际门禁验证尚未
发startup包的GSS连接。此事实不能当作网关已做Linux/K8s验收或完整Quiesce。
本批不构建镜像、不准备/执行部署、不合主分支；持续连接门禁/后台排空、预算/
Host最终VM、公开worker及受控未知恢复代码仍继续实施。
