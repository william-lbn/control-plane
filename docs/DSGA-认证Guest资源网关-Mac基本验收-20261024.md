# 认证Guest资源网关

本批增加固定GET `/dsg/v1/resource/identity`和POST同前缀fence、apply、observe、
replay-original。执行和观察独立连接池，沿用原VM/Service/当前owner及管理TLS/SNI
验证，原始Bearer和请求字节完整转发。禁止任意路径、编码别名、query或重定向。
不生成root限额、预算完成或VM许可。初始化只读token不获此权限。

正式原配置已经由创建者生成immutable Secret；受影响broker测试夹具此前遗漏
immutable标志，导致实际broker拒绝签发。本批补齐夹具，未放松生产来源校验。

Mac新增资源代理用例及受影响实际TLS broker签发：2顶层/2pass，0fail/skip；
go1.26.5 race，内部原生目标是协议夹具，Kube API也是夹具，不是K8s实测。
go vet ./internal/control退出0。证据在工作区build/dsga-resource-http-cp-final.jsonl
和cp-vet.log；早期失败日志cp.jsonl保留，未算作通过。

本批前完整同步自有主线7899ec0，两项提交包含组织事务和依赖维护，独立合并
提交34aa7be。仅相应Mac契约6顶层/12事件通过，见同批主线027文档；未重复Web/
组织实库已验证业务。主线合并与本模块分开提交，后续共同推送既有功能分支。

资源领域协调、完整预算和排空、Host/最终VM、公开worker/生命周期及受控恢复
仍需编码。真实Linux K8s/NeonVM/HugeTLB/物理及性能后验，不构建镜像、不部署、
不合main。AS依赖a6d191af172fc9fb109b648145cbe6ba484ac94d已先推送并核对
远程，再更新本仓模块锁为v0.0.0-20261024132133-a6d191af172f；下载返回Origin Hash
与远程一致，准确依赖下go vet编译确认退出0，未重复功能测试，证据为工作区
build/dsga-resource-http-{module.json,cp-pinned-vet.log}。首次审批额度失败
命令未执行；所有者要求继续后按原审批流程成功，不绕过审批。最终本仓提交/推送
状态以实际Git/远程refs及工作区恢复点为准，不能把依赖推送冒称本仓已发布。
