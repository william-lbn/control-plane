import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
type Entry = {
  id: string;
  request_id: string;
  channel: string;
  method: string;
  outcome: string;
  application_status: number | null;
  response_bytes: number;
  duration_ms: number | null;
  started_at: string;
  completion_unconfirmed: boolean;
};
export function FunctionRequestLogs({ path }: { path: string }) {
  const [items, setItems] = useState<Entry[]>([]),
    [next, setNext] = useState(''),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const epoch = useRef(0);
  useEffect(() => {
    epoch.current++;
    setItems([]);
    setNext('');
    setError('');
    setBusy(false);
  }, [path]);
  async function load(after = '') {
    const current = epoch.current;
    setBusy(true);
    setError('');
    try {
      const value = await api<{ items: Entry[]; next_after: string }>(
        `${path}/request-logs?${new URLSearchParams({ limit: '25', ...(after ? { after } : {}) })}`,
      );
      if (current === epoch.current) {
        setItems(value.items);
        setNext(value.next_after);
      }
    } catch (e) {
      if (current === epoch.current) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  const outcome = (item: Entry) =>
    item.completion_unconfirmed
      ? '完成未确认'
      : {
          in_progress: '执行中',
          completed: '已完成',
          outcome_unknown: '结果未知',
          response_incomplete: '响应中断',
          client_disconnected: '客户端断开',
        }[item.outcome] || item.outcome;
  return (
    <section className="backend-panel" data-testid="function-request-logs">
      <h3>已调度请求日志</h3>
      <p>
        记录执行状态、应用 HTTP 状态和耗时；不采集请求路径、query、headers、body 或客户
        stdout。冷醒前错误以操作记录为准。
      </p>
      <button className="button" disabled={busy} onClick={() => void load()}>
        加载请求日志
      </button>
      {next && (
        <button className="button ghost" disabled={busy} onClick={() => void load(next)}>
          下一页请求日志
        </button>
      )}
      {error && <p role="alert">{error}</p>}
      <table>
        <thead>
          <tr>
            <th>时间 / Request ID</th>
            <th>入口 / 方法</th>
            <th>状态</th>
            <th>应用 HTTP</th>
            <th>耗时 ms</th>
            <th>响应字节</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={item.id}>
              <td>
                {new Date(item.started_at).toLocaleString()}
                <br />
                <code>{item.request_id}</code>
              </td>
              <td>
                {item.channel} / {item.method}
              </td>
              <td>{outcome(item)}</td>
              <td>{item.application_status ?? '—'}</td>
              <td>{item.duration_ms ?? '—'}</td>
              <td>{item.response_bytes}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {!busy && items.length === 0 && (
        <p className="muted">尚未加载或暂无已调度请求。失败和结果未知记录不会被展示成成功。</p>
      )}
    </section>
  );
}
