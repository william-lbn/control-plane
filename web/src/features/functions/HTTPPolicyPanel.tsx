import { useState } from 'react';
import { api } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

export function HTTPPolicyPanel({
  path,
  version,
  enabled,
  idle,
  timeout,
  canEdit,
  onSaved,
}: {
  path: string;
  version: string;
  enabled: boolean;
  idle: boolean;
  timeout: number;
  canEdit: boolean;
  onSaved: () => void;
}) {
  const [publicAccess, setPublicAccess] = useState(enabled);
  const [automaticIdle, setAutomaticIdle] = useState(idle);
  const [seconds, setSeconds] = useState(timeout || 60);
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function save() {
    setBusy(true);
    setError('');
    try {
      await api(`${path}/http-policy`, {
        method: 'PATCH',
        headers: {
          'Idempotency-Key': newRequestKey(),
          'If-Match': `"${version}"`,
        },
        body: JSON.stringify({
          public_enabled: publicAccess,
          idle_enabled: automaticIdle,
          idle_timeout_seconds: seconds,
          acknowledge_public_access_and_background_eviction: ack,
        }),
      });
      onSaved();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="function-authoring" data-testid="function-http-policy">
      <h3>HTTP 入口与自动休眠</h3>
      <p className="muted">
        公开入口无需控制台登录；应用鉴权由函数代码负责。活动 HTTP/SSE
        请求会阻止自动回收。空闲回收会结束后台任务，首次请求等待保留版本重新启动；手动停止保持关闭。当前入口不支持
        WebSocket 或 Cookie。
      </p>
      <label>
        <input
          type="checkbox"
          aria-label="公开 HTTP 入口"
          checked={publicAccess}
          disabled={!canEdit || busy}
          onChange={(e) => {
            setPublicAccess(e.target.checked);
            if (!e.target.checked) setAutomaticIdle(false);
          }}
        />
        公开 HTTP 入口
      </label>
      <label>
        <input
          type="checkbox"
          aria-label="自动休眠唤醒"
          checked={automaticIdle}
          disabled={!canEdit || busy || !publicAccess}
          onChange={(e) => setAutomaticIdle(e.target.checked)}
        />
        自动休眠与首次请求唤醒
      </label>
      <label>
        空闲秒数
        <input
          type="number"
          aria-label="函数空闲秒数"
          min={30}
          max={3600}
          value={seconds}
          disabled={!canEdit || busy}
          onChange={(e) => setSeconds(Number(e.target.value))}
        />
      </label>
      <label>
        <input
          type="checkbox"
          aria-label="确认公开访问与后台回收"
          checked={ack}
          disabled={!canEdit || busy}
          onChange={(e) => setAck(e.target.checked)}
        />
        确认公开访问，并允许空闲后终止后台任务
      </label>
      {error && <p role="alert">{error}</p>}
      {canEdit && (
        <button
          className="primary"
          disabled={busy || ((publicAccess || automaticIdle) && !ack)}
          onClick={save}
        >
          {busy ? '保存中…' : '保存入口策略'}
        </button>
      )}
    </section>
  );
}
