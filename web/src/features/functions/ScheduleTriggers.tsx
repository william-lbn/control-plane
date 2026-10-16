import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Trigger = {
  trigger_id: string;
  type: 'schedule';
  name: string;
  function_slug: string;
  function_path: string;
  schedule: { cron: string };
  enabled: boolean;
  version_token: string;
  next_run_at: string | null;
  inherited: boolean;
};
type Invocation = {
  id: string;
  state: string;
  scheduled_at: string;
  attempts: number;
  application_status: number | null;
  error_code: string | null;
  request_event_id: string | null;
};
type Page<T> = { items: T[]; next_after: string | null; scheduling_enabled?: boolean };
type Mutation = {
  id: string;
  method: 'POST' | 'PATCH' | 'DELETE';
  key: string;
  version?: string;
  body?: string;
};

// Mutation input and key survive an uncertain response. Retrying is always an
// explicit replay of the exact saved request, never a new scheduling intent.
export function ScheduleTriggers({
  path,
  slug,
  canEdit,
}: {
  path: string;
  slug: string;
  canEdit: boolean;
}) {
  const [items, setItems] = useState<Trigger[]>([]),
    [cursor, setCursor] = useState<string | null>(null);
  const [enabled, setEnabled] = useState(false),
    [selected, setSelected] = useState('');
  const [name, setName] = useState(''),
    [cron, setCron] = useState('* * * * *'),
    [targetPath, setTargetPath] = useState('/');
  const [events, setEvents] = useState<Invocation[]>([]),
    [eventCursor, setEventCursor] = useState<string | null>(null);
  const [error, setError] = useState(''),
    [busy, setBusy] = useState(false),
    [pending, setPending] = useState<Mutation | null>(null);
  const epoch = useRef(0);
  const load = async (after?: string) => {
    const generation = epoch.current;
    const data = await api<Page<Trigger>>(
      path + '?limit=100' + (after ? '&after=' + encodeURIComponent(after) : ''),
    );
    if (generation !== epoch.current) return;
    setItems((old) => (after ? [...old, ...data.items] : data.items));
    setCursor(data.next_after);
    setEnabled(!!data.scheduling_enabled);
  };
  const history = async (id: string, after?: string) => {
    const generation = epoch.current;
    const data = await api<Page<Invocation>>(
      `${path}/${id}/invocations?limit=20` + (after ? '&after=' + encodeURIComponent(after) : ''),
    );
    if (generation !== epoch.current) return;
    setEvents((old) => (after ? [...old, ...data.items] : data.items));
    setEventCursor(data.next_after);
  };
  useEffect(() => {
    const generation = ++epoch.current;
    let disposed = false;
    setItems([]);
    setSelected('');
    setEvents([]);
    setPending(null);
    setError('');
    setName('');
    setCron('* * * * *');
    setTargetPath('/');
    setBusy(true);
    api<Page<Trigger>>(path + '?limit=100')
      .then((data) => {
        if (!disposed) {
          setItems(data.items);
          setCursor(data.next_after);
          setEnabled(!!data.scheduling_enabled);
        }
      })
      .catch((e) => {
        if (!disposed) setError(String(e));
      })
      .finally(() => {
        if (!disposed) setBusy(false);
      });
    return () => {
      disposed = true;
      if (epoch.current === generation) epoch.current++;
    };
  }, [path, slug]);
  const submit = async (m: Mutation) => {
    const generation = epoch.current;
    setPending(m);
    setBusy(true);
    setError('');
    try {
      await api(path + (m.id ? '/' + m.id : ''), {
        method: m.method,
        headers: {
          'Idempotency-Key': m.key,
          ...(m.version ? { 'If-Match': `"${m.version}"` } : {}),
        },
        body: m.body,
      });
      if (generation !== epoch.current) return;
      setPending(null);
      setSelected('');
      setEvents([]);
      setName('');
      await load();
    } catch (e) {
      if (generation !== epoch.current) return;
      setError(e instanceof Error ? e.message : String(e));
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
  };
  const current = items.find((t) => t.trigger_id === selected);
  const select = async (t: Trigger) => {
    setSelected(t.trigger_id);
    setName(t.name);
    setCron(t.schedule.cron);
    setTargetPath(t.function_path);
    setEvents([]);
    setEventCursor(null);
    setError('');
    setBusy(true);
    const generation = epoch.current;
    try {
      await history(t.trigger_id);
    } catch (e) {
      if (generation === epoch.current) setError(String(e));
    } finally {
      if (generation === epoch.current) setBusy(false);
    }
  };
  const change = (method: 'POST' | 'PATCH' | 'DELETE', active = false) => {
    const body =
      method === 'DELETE'
        ? undefined
        : JSON.stringify({
            type: 'schedule',
            name,
            function_slug: slug,
            function_path: targetPath,
            schedule: { cron },
            enabled: active,
          });
    void submit({
      method,
      id: method === 'POST' ? '' : current!.trigger_id,
      key: newRequestKey(),
      version: method === 'POST' ? undefined : current!.version_token,
      body,
    });
  };
  return (
    <section className="function-authoring" aria-label="定时触发器">
      <h3>
        定时触发器 <span className="pill">UTC · Cron</span>
      </h3>
      <p className="muted">
        调度保存在控制面；用户 Compute 和函数可以自动休眠。每个时间点有稳定调用
        ID。删除或暂停停止后续排队；已排队调用可能继续。结果未知不会自动重放。分支继承尚未开放。
      </p>
      {!enabled && <p role="status">当前未启用调度执行；可以保存禁用的计划。</p>}
      {error && <p role="alert">{error}</p>}
      {pending && !busy && (
        <div role="status">
          变更结果待确认，请保留原请求。
          <button onClick={() => void submit(pending)}>重试原定时请求</button>
        </div>
      )}
      <button
        disabled={busy || !!pending}
        onClick={() => {
          setBusy(true);
          void load()
            .catch((e) => setError(String(e)))
            .finally(() => setBusy(false));
        }}
      >
        刷新定时计划
      </button>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>计划</th>
              <th>表达式 / 路径</th>
              <th>状态</th>
              <th>下次运行 UTC</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {items
              .filter((t) => t.function_slug === slug)
              .map((t) => (
                <tr key={t.trigger_id}>
                  <td>
                    {t.name}
                    <small>
                      {t.trigger_id} · v{t.version_token}
                    </small>
                  </td>
                  <td>
                    <code>{t.schedule.cron}</code>
                    <br />
                    {t.function_path}
                  </td>
                  <td>
                    {t.enabled ? '启用' : '暂停'}
                    {t.inherited ? ' · 继承' : ''}
                  </td>
                  <td>{t.next_run_at || '—'}</td>
                  <td>
                    <button disabled={busy || !!pending} onClick={() => void select(t)}>
                      管理计划 {t.name}
                    </button>
                  </td>
                </tr>
              ))}
          </tbody>
        </table>
      </div>
      {cursor && (
        <button
          disabled={busy || !!pending}
          onClick={() => {
            setBusy(true);
            void load(cursor)
              .catch((e) => setError(String(e)))
              .finally(() => setBusy(false));
          }}
        >
          更多定时计划
        </button>
      )}
      {canEdit && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            change(current ? 'PATCH' : 'POST', false);
          }}
        >
          <h4>{current ? '编辑计划' : '新建计划（默认暂停）'}</h4>
          <label>
            计划名称
            <input
              aria-label="定时计划名称"
              value={name}
              disabled={busy || !!pending}
              required
              maxLength={256}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label>
            UTC Cron
            <input
              aria-label="UTC Cron"
              value={cron}
              disabled={busy || !!pending}
              required
              maxLength={128}
              onChange={(e) => setCron(e.target.value)}
            />
          </label>
          <label>
            函数路径
            <input
              aria-label="定时函数路径"
              value={targetPath}
              disabled={busy || !!pending}
              required
              maxLength={2048}
              onChange={(e) => setTargetPath(e.target.value)}
            />
          </label>
          <p className="muted">
            五个数字字段：分、时、日、月、星期；支持
            *、列表、范围和步长。最短一分钟，无查询参数。启用前须开放函数 HTTP 策略。应用应以
            invocation_id 去重。
          </p>
          <button disabled={busy || !!pending}>
            {current ? '保存并暂停计划' : '保存暂停计划'}
          </button>
          {current && (
            <>
              <button
                type="button"
                disabled={busy || !!pending || !enabled}
                onClick={() => change('PATCH', true)}
              >
                保存并启用计划
              </button>
              <button
                type="button"
                disabled={busy || !!pending}
                onClick={() => {
                  if (window.confirm('删除此计划？后续不再排队，已排队调用和历史记录会保留。'))
                    change('DELETE');
                }}
              >
                删除定时计划
              </button>
              <button
                type="button"
                disabled={busy || !!pending}
                onClick={() => {
                  setSelected('');
                  setName('');
                  setEvents([]);
                }}
              >
                新建另一计划
              </button>
            </>
          )}
        </form>
      )}
      {current && (
        <section aria-label="定时调用记录">
          <h4>定时调用记录</h4>
          <button
            disabled={busy || !!pending}
            onClick={() => {
              setBusy(true);
              void history(current.trigger_id)
                .catch((e) => setError(String(e)))
                .finally(() => setBusy(false));
            }}
          >
            刷新定时调用
          </button>
          <table>
            <thead>
              <tr>
                <th>调用 ID / UTC</th>
                <th>状态</th>
                <th>尝试 / HTTP</th>
                <th>诊断</th>
              </tr>
            </thead>
            <tbody>
              {events.map((v) => (
                <tr key={v.id}>
                  <td>
                    <code>{v.id}</code>
                    <small>{v.scheduled_at}</small>
                  </td>
                  <td>{v.state}</td>
                  <td>
                    {v.attempts} / {v.application_status || '—'}
                  </td>
                  <td>
                    {v.error_code || '—'}
                    <small>{v.request_event_id}</small>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {eventCursor && (
            <button
              disabled={busy}
              onClick={() => {
                setBusy(true);
                void history(current.trigger_id, eventCursor)
                  .catch((e) => setError(String(e)))
                  .finally(() => setBusy(false));
              }}
            >
              更多定时调用
            </button>
          )}
          <p className="muted">
            只记录调度与响应状态，不保存客户请求/响应正文。succeeded 表示收到 2xx
            完整响应，业务是否成功由应用验证。
          </p>
        </section>
      )}
    </section>
  );
}
