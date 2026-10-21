# Functions WebSocket

0.53.0 / migration 042 已完成基础闭环，见[精确交付](DELIVERY-2026-10-18-FUNCTION-WEBSOCKETS.md)。性能、可靠性、HA/DR 和跨实例栅栏专项按用户要求后置。

函数沿既有独立公开 HTTP origin 接受标准 GET WebSocket 握手。公开入口先验证方法、Connection/Upgrade、版本 13、16 字节 key 和子协议，再按原 Function 身份冷醒、锁定和重新检查权限。当前 Guest 的 TLS/HMAC/boot/generation 和 WebSocket capability 必须匹配。旧 Guest 在派发前明确拒绝。

控制端将原 URL、应用请求头、握手和代次放入有界签名 envelope；原 supervisor 只连接固定 loopback Node，跨两跳流式转发原始帧。Cookie、代理和平台头不会交给客户代码。握手接受值、选择的子协议和关闭帧保留，不自动重连或重放客户消息。应用鉴权在客户 handler 中完成，可返回 401/403 拒绝握手。平台不记录消息或查询凭据。

自托管 SDK 使用显式路径，与商业 `@neon/functions` 包分开：

```js
import { upgradeWebSocket } from '/opt/neon/functions/runtime/websocket.mjs';
export default request => {
  const { socket, response } = upgradeWebSocket(request);
  socket.onmessage = event => socket.send(event.data);
  return response;
};
```

支持 addEventListener 和 onopen/onmessage/onclose/onerror、文本、ArrayBuffer/TypedArray/Blob、binaryType、readyState、send、close 和客户端提供的子协议。升级 helper 只能在该原始握手 handler 内使用一次，response 必须原样返回；clone/重建会拒绝。单消息 1 MiB，发送缓冲有界，单实例并发 16，连接最长 15 分钟，不协商压缩。

supervisor 从接收握手到流结束计入原实例活动调用，打开的连接阻止自动空闲退休。正常断开更新活动时间；随后沿既有 idle 路径休眠，下一次显式连接可以新部署回执冷醒相同保留代码/环境。请求账本沿 `public_http` 记录握手状态 101 和结束信息，帧字节计数不是收费计量。活动连接沿既有共享准入锁保留原实例；手动停止若提示 busy，先断开客户端后重试。不承诺跨部署迁移连接。

Functions 页面提供显式连接、子协议、路径、文本发送、关闭和有界最近消息；不会因页面加载自动连接。浏览器需自行处理断线和应用凭据。示例见 `examples/functions/websocket/index.mjs`。

RFC 帧解析使用精确锁定的 MIT `ws` 8.22.0，禁用可选原生扩展和压缩。主来源：[ws 官方仓库](https://github.com/websockets/ws)。完整性能、跨实例连接迁移、独立 edge/计费、外部 fencing 与 Hono 专用适配属于后续扩展，不由基础握手和消息验收代替。

Console CSP 仅许可运营配置的确切 Functions origin 和对应 WS(S) authority。冷醒生成新部署回执；代码 digest、入口、环境名称和真实 retained-resume 意图才是保留依据，source_deployment_id 仅用于显式历史回滚。WS 字节字段目前记录双向帧总量，不是收费计量。
