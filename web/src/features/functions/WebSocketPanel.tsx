import { useEffect, useRef, useState } from 'react';

export function WebSocketPanel({ url, enabled }: { url: string | null; enabled: boolean }) {
  const connection = useRef<WebSocket | null>(null);
  const [state, setState] = useState('未连接');
  const [suffix, setSuffix] = useState('/ws');
  const [protocol, setProtocol] = useState('');
  const [message, setMessage] = useState('hello');
  const [messages, setMessages] = useState<string[]>([]);
  useEffect(
    () => () => {
      connection.current?.close(1000, 'Console closed');
    },
    [url],
  );
  function connect() {
    if (!url || connection.current) return;
    try {
      const target = new URL(url);
      if (!suffix.startsWith('/') || suffix.startsWith('//') || suffix.includes('\\'))
        throw new Error('请使用函数下的相对路径');
      const extra = new URL(url + suffix);
      if (extra.origin !== target.origin || !extra.pathname.startsWith(target.pathname + '/'))
        throw new Error('路径必须保留当前函数');
      extra.protocol = target.protocol === 'https:' ? 'wss:' : 'ws:';
      const socket = new WebSocket(extra, protocol ? [protocol] : []);
      connection.current = socket;
      socket.binaryType = 'arraybuffer';
      setState('连接中');
      setMessages([]);
      socket.onopen = () => setState('已连接' + (socket.protocol ? ` · ${socket.protocol}` : ''));
      socket.onmessage = (event) =>
        setMessages((old) => [
          ...old.slice(-19),
          typeof event.data === 'string'
            ? event.data.slice(0, 4096)
            : `二进制 ${event.data.byteLength} 字节`,
        ]);
      socket.onerror = () => setState('连接失败，请检查应用鉴权或运行时版本');
      socket.onclose = (event) => {
        if (connection.current === socket) connection.current = null;
        setState(`已关闭 · ${event.code}`);
      };
    } catch (error) {
      setState(error instanceof Error ? error.message : String(error));
    }
  }
  return (
    <section className="function-authoring" data-testid="function-websocket-panel">
      <h3>WebSocket 调试</h3>
      <p className="muted">
        连接执行客户函数，可能唤醒实例；应用负责鉴权。打开的连接阻止自动休眠，连接最多持续 15
        分钟，单消息限 1 MiB，不压缩。控制台 Cookie 不发送到函数。客户代码可从
        /opt/neon/functions/runtime/websocket.mjs 导入 upgradeWebSocket。
      </p>
      <label>
        连接路径
        <input
          aria-label="WebSocket 路径"
          value={suffix}
          onChange={(e) => setSuffix(e.target.value)}
          disabled={!!connection.current}
        />
      </label>
      <label>
        子协议
        <input
          aria-label="WebSocket 子协议"
          value={protocol}
          onChange={(e) => setProtocol(e.target.value)}
          disabled={!!connection.current}
        />
      </label>
      <button disabled={!enabled || !!connection.current} onClick={connect}>
        连接 WebSocket
      </button>
      <button
        disabled={!connection.current}
        onClick={() => connection.current?.close(1000, 'Console closed')}
      >
        关闭 WebSocket
      </button>
      <p role="status" data-testid="function-websocket-state">
        {state}
      </p>
      <label>
        消息
        <input
          aria-label="WebSocket 消息"
          maxLength={4096}
          value={message}
          onChange={(e) => setMessage(e.target.value)}
        />
      </label>
      <button
        disabled={connection.current?.readyState !== WebSocket.OPEN}
        onClick={() => connection.current?.send(message)}
      >
        发送 WebSocket 消息
      </button>
      <pre data-testid="function-websocket-messages">{messages.join('\n')}</pre>
    </section>
  );
}
