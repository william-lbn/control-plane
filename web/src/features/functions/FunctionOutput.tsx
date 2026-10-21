import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Policy = {
  enabled: boolean;
  version_token: string;
  capture_since: string | null;
  retention_days: number;
  last_error_code?: string;
};
type Entry = {
  instance_id: string;
  deployment_id: string;
  boot_id: string;
  sequence: number;
  timestamp: string;
  stream: string;
  severity: string;
  scope_name: string;
  message: string;
  truncated: boolean;
};
type Page = { items: Entry[]; next_after: string; guest_dropped_lines: number };
type Pending = { key: string; version: string; body: string };
export function FunctionOutput({ path, canEdit }: { path: string; canEdit: boolean }) {
  const [policy, setPolicy] = useState<Policy>({
      enabled: false,
      version_token: '0',
      capture_since: null,
      retention_days: 3,
    }),
    [collection, setCollection] = useState(false),
    [wanted, setWanted] = useState(false),
    [ack, setAck] = useState(false);
  const [items, setItems] = useState<Entry[]>([]),
    [after, setAfter] = useState(''),
    [dropped, setDropped] = useState(0),
    [level, setLevel] = useState(''),
    [search, setSearch] = useState(''),
    [tail, setTail] = useState(false);
  const [busy, setBusy] = useState(false),
    [error, setError] = useState(''),
    [pending, setPending] = useState<Pending | null>(null);
  const epoch = useRef(0);
  const historyLevel = useRef('');
  const historyRequest = useRef(0);
  async function history(cursor = '', severity = historyLevel.current) {
    const generation = epoch.current;
    const request = ++historyRequest.current;
    const data = await api<Page>(
      path +
        '/output-logs?limit=50' +
        (cursor ? '&after=' + encodeURIComponent(cursor) : '') +
        (severity ? '&severity=' + severity : ''),
    );
    if (
      generation !== epoch.current ||
      request !== historyRequest.current ||
      severity !== historyLevel.current
    )
      return;
    setItems((old) => (cursor ? [...old, ...data.items] : data.items));
    setAfter(data.next_after);
    setDropped(data.guest_dropped_lines);
  }
  async function refresh() {
    const generation = epoch.current;
    const data = await api<{ policy: Policy; collection_enabled: boolean }>(
      path + '/output-policy',
    );
    if (generation !== epoch.current) return;
    setPolicy(data.policy);
    setWanted(data.policy.enabled);
    setCollection(data.collection_enabled);
    await history();
  }
  useEffect(() => {
    ++epoch.current;
    setItems([]);
    setAfter('');
    setPending(null);
    setAck(false);
    setTail(false);
    setError('');
    setBusy(true);
    void refresh()
      .catch((e) => setError(String(e)))
      .finally(() => setBusy(false));
    return () => {
      ++epoch.current;
    };
  }, [path]);
  useEffect(() => {
    if (!tail) return;
    const timer = setInterval(() => {
      void history().catch((e) => setError(String(e)));
    }, 3000);
    return () => clearInterval(timer);
  }, [path, tail, level]);
  async function submit(m: Pending) {
    const generation = epoch.current;
    setPending(m);
    setBusy(true);
    setError('');
    try {
      await api(path + '/output-policy', {
        method: 'PATCH',
        headers: { 'Idempotency-Key': m.key, 'If-Match': `"${m.version}"` },
        body: m.body,
      });
      if (generation !== epoch.current) return;
      setPending(null);
      setAck(false);
      await refresh();
    } catch (e) {
      if (generation !== epoch.current) return;
      setError(String(e));
      if (
        e instanceof ApiError &&
        e.status >= 400 &&
        e.status < 500 &&
        ![408, 429].includes(e.status)
      )
        setPending(null);
    } finally {
      if (generation === epoch.current) setBusy(false);
    }
  }
  function download() {
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(items, null, 2) + '\n'], { type: 'application/json' }),
    );
    const a = document.createElement('a');
    a.href = url;
    a.download = 'function-output-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }
  const visible = items.filter(
    (v) => !search || v.message.toLocaleLowerCase().includes(search.toLocaleLowerCase()),
  );
  return (
    <section className="function-authoring" aria-label="客户输出日志">
      <h3>
        客户输出日志 <span className="pill">stdout / stderr</span>
      </h3>
      <p className="muted">
        采集默认关闭。开启后，当前项目有读取权限的成员可以查看客户输出；加密保存三天，最多保留最近
        10,000 行。已知环境值和数据库凭据会替换，但无法识别所有应用秘密，请勿打印敏感信息。
      </p>
      <p role="status">
        {policy.enabled ? '已开启采集' : '采集已关闭'}
        {policy.capture_since ? ' · 本次采集起点 ' + policy.capture_since : ''}
      </p>
      {!collection && <p>当前未启用客户输出服务。</p>}
      {policy.last_error_code && (
        <p role="status">最近采集暂不可用：{policy.last_error_code}。已保存的日志仍可查看。</p>
      )}
      {canEdit && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void submit({
              key: newRequestKey(),
              version: policy.version_token,
              body: JSON.stringify({ enabled: wanted, acknowledge_sensitive_output: ack }),
            });
          }}
        >
          <label>
            <input
              type="checkbox"
              aria-label="采集客户输出"
              checked={wanted}
              disabled={busy || !!pending}
              onChange={(e) => setWanted(e.target.checked)}
            />
            采集客户输出
          </label>
          <label>
            <input
              type="checkbox"
              aria-label="确认输出可能包含敏感数据"
              checked={ack}
              disabled={busy || !!pending}
              onChange={(e) => setAck(e.target.checked)}
            />
            确认输出可能包含敏感数据，并同意项目共享访问及三天保留
          </label>
          <button disabled={busy || !!pending || (wanted && (!ack || !collection))}>
            保存日志采集设置
          </button>
        </form>
      )}
      {error && <p role="alert">{error}</p>}
      {pending && !busy && (
        <p role="status">
          设置结果待确认。<button onClick={() => void submit(pending)}>重试原日志设置</button>
        </p>
      )}
      <div className="storage-actions">
        <button
          disabled={busy || !!pending}
          onClick={() => {
            setBusy(true);
            void refresh()
              .catch((e) => setError(String(e)))
              .finally(() => setBusy(false));
          }}
        >
          刷新客户日志
        </button>
        <label>
          级别
          <select
            aria-label="客户日志级别"
            value={level}
            onChange={(e) => {
              const next = e.target.value;
              historyLevel.current = next;
              setLevel(next);
              void history('', next).catch((e) => setError(String(e)));
            }}
          >
            <option value="">全部级别</option>
            {['DEBUG', 'INFO', 'WARN', 'ERROR'].map((v) => (
              <option key={v}>{v}</option>
            ))}
          </select>
        </label>
        <label>
          筛选已加载文本
          <input
            aria-label="筛选已加载日志"
            value={search}
            maxLength={256}
            onChange={(e) => setSearch(e.target.value)}
          />
        </label>
        <label>
          <input
            type="checkbox"
            aria-label="跟随最新客户日志"
            checked={tail}
            onChange={(e) => setTail(e.target.checked)}
          />
          跟随最新（每三秒）
        </label>
        <button disabled={!items.length} onClick={download}>
          导出已加载日志
        </button>
      </div>
      {dropped > 0 && (
        <p role="status">Guest 缓冲溢出，已确认丢弃 {dropped} 行。当前视图不代表完整输出。</p>
      )}
      <div className="table-wrap" data-testid="function-output-lines">
        <table>
          <thead>
            <tr>
              <th>UTC / 级别</th>
              <th>输出</th>
              <th>归属</th>
            </tr>
          </thead>
          <tbody>
            {visible.map((v) => (
              <tr key={`${v.instance_id}/${v.boot_id}/${v.sequence}`}>
                <td>
                  {v.timestamp}
                  <small>
                    {v.severity} · {v.stream}
                    {v.truncated ? ' · 整行已丢弃' : ''}
                  </small>
                </td>
                <td>
                  <pre>{v.message}</pre>
                  <small>{v.scope_name}</small>
                </td>
                <td>
                  <code>{v.instance_id}</code>
                  <small>
                    {v.deployment_id} · #{v.sequence}
                  </small>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!items.length && <p>没有已采集且仍在保留期内的输出。查看此页面不会唤醒函数或 Compute。</p>}
      {after && (
        <button
          disabled={busy}
          onClick={() => {
            setBusy(true);
            void history(after)
              .catch((e) => setError(String(e)))
              .finally(() => setBusy(false));
          }}
        >
          更多客户日志
        </button>
      )}
    </section>
  );
}
