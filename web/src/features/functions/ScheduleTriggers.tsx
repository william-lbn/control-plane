import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Trigger = {
  trigger_id: string;
  type: 'schedule' | 'storage_object_created';
  name: string;
  function_slug: string;
  function_path: string;
  schedule?: { cron: string };
  storage_object_created?: { bucket_name: string; prefix: string };
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
  type?: string;
  version_token: string;
  logical_invocation_id?: string;
  object?: { bucket_name: string; object_key: string };
  parent_invocation_id?: string | null;
  redrive_succeeded?: boolean;
  redrive_active?: boolean;
  redrive_count?: number;
};
type Page<T> = {
  items: T[];
  next_after: string | null;
  scheduling_enabled?: boolean;
  object_events_enabled?: boolean;
};
type Mutation = {
  id: string;
  method: 'POST' | 'PATCH' | 'DELETE';
  key: string;
  version?: string;
  body?: string;
  kind?: 'redrive';
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
  const [type, setType] = useState<Trigger['type']>('schedule');
  const [bucket, setBucket] = useState(''),
    [prefix, setPrefix] = useState(''),
    [ack, setAck] = useState(false),
    [objectEnabled, setObjectEnabled] = useState(false);
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
    setObjectEnabled(!!data.object_events_enabled);
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
    setType('schedule');
    setBucket('');
    setPrefix('');
    setAck(false);
    setObjectEnabled(false);
    setBusy(true);
    api<Page<Trigger>>(path + '?limit=100')
      .then((data) => {
        if (!disposed) {
          setItems(data.items);
          setCursor(data.next_after);
          setEnabled(!!data.scheduling_enabled);
          setObjectEnabled(!!data.object_events_enabled);
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
      if (m.kind === 'redrive') {
        await history(selected);
        return;
      }
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
    setType(t.type);
    setCron(t.schedule?.cron || '* * * * *');
    setBucket(t.storage_object_created?.bucket_name || '');
    setPrefix(t.storage_object_created?.prefix || '');
    setAck(false);
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
            type,
            name,
            function_slug: slug,
            function_path: targetPath,
            ...(type === 'schedule'
              ? { schedule: { cron } }
              : {
                  storage_object_created: { bucket_name: bucket, prefix },
                  acknowledge_at_least_once: ack,
                }),
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
    <section
      className="function-authoring"
      aria-label={type === 'schedule' ? '定时触发器' : '对象触发器'}
    >
      <h3>
        Functions 触发器 <span className="pill">UTC 定时 / 对象上传</span>
      </h3>
      <p className="muted">
        调度保存在控制面；用户 Compute 和函数可以自动休眠。每个时间点有稳定调用
        ID。对象上传按桶名和区分大小写的前缀匹配；失败时可能重复送达，请按 invocation_id
        去重。删除或暂停停止接收新事件；已排队调用可能继续。结果未知需明确确认后重投递。分支继承尚未开放。
      </p>
      {type === 'schedule' && !enabled && (
        <p role="status">当前未启用调度执行；可以保存禁用的计划。</p>
      )}
      {type === 'storage_object_created' && !objectEnabled && (
        <p role="status">当前未启用对象事件执行；可以保存暂停的触发器。</p>
      )}
      {error && <p role="alert">{error}</p>}
      {pending && !busy && (
        <div role="status">
          变更结果待确认，请保留原请求。
          <button onClick={() => void submit(pending)}>
            {type === 'schedule' ? '重试原定时请求' : '重试原对象请求'}
          </button>
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
        {type === 'schedule' ? '刷新定时计划' : '刷新对象触发器'}
      </button>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>触发器</th>
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
                    <code>
                      {t.type === 'schedule'
                        ? t.schedule?.cron
                        : `${t.storage_object_created?.bucket_name} / ${t.storage_object_created?.prefix || '全部对象'}`}
                    </code>
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
                      {t.type === 'schedule' ? '管理计划' : '管理对象触发器'} {t.name}
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
          更多触发器
        </button>
      )}
      {canEdit && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            change(current ? 'PATCH' : 'POST', false);
          }}
        >
          <h4>{current ? '编辑触发器' : '新建触发器（默认暂停）'}</h4>
          <label>
            触发类型
            <select
              aria-label="函数触发类型"
              value={type}
              disabled={busy || !!pending || !!current}
              onChange={(e) => {
                setType(e.target.value as Trigger['type']);
                setAck(false);
              }}
            >
              <option value="schedule">UTC 定时</option>
              <option value="storage_object_created">对象上传</option>
            </select>
          </label>
          <label>
            触发器名称
            <input
              aria-label={type === 'schedule' ? '定时计划名称' : '对象触发器名称'}
              value={name}
              disabled={busy || !!pending}
              required
              maxLength={256}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          {type === 'schedule' ? (
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
          ) : (
            <>
              <label>
                桶名
                <input
                  aria-label="对象触发桶"
                  value={bucket}
                  disabled={busy || !!pending}
                  required
                  maxLength={63}
                  onChange={(e) => setBucket(e.target.value)}
                />
              </label>
              <label>
                对象键前缀
                <input
                  aria-label="对象触发前缀"
                  value={prefix}
                  disabled={busy || !!pending}
                  maxLength={1024}
                  onChange={(e) => setPrefix(e.target.value)}
                />
              </label>
              <label>
                <input
                  type="checkbox"
                  aria-label="确认对象事件可能重复送达"
                  checked={ack}
                  disabled={busy || !!pending}
                  onChange={(e) => setAck(e.target.checked)}
                />
                确认失败时可能重复送达，函数按 invocation_id 去重
              </label>
            </>
          )}
          <label>
            函数路径
            <input
              aria-label={type === 'schedule' ? '定时函数路径' : '对象触发函数路径'}
              value={targetPath}
              disabled={busy || !!pending}
              required
              maxLength={2048}
              onChange={(e) => setTargetPath(e.target.value)}
            />
          </label>
          <p className="muted">
            {type === 'schedule'
              ? '五个数字字段：分、时、日、月、星期；支持 *、列表、范围和步长。最短一分钟。'
              : '上传、覆盖、复制和 multipart 完成会发布事件；仅修改元数据或上传未完成的分片不触发。需要已启用的 Object Storage，桶名按名称订阅。'}{' '}
            无查询参数。启用前须开放函数 HTTP 策略。应用应以 invocation_id 去重。
          </p>
          <button disabled={busy || !!pending}>
            {type === 'schedule'
              ? current
                ? '保存并暂停计划'
                : '保存暂停计划'
              : '保存暂停对象触发器'}
          </button>
          {current && (
            <>
              <button
                type="button"
                disabled={
                  busy || !!pending || (type === 'schedule' ? !enabled : !objectEnabled || !ack)
                }
                onClick={() => change('PATCH', true)}
              >
                {type === 'schedule' ? '保存并启用计划' : '保存并启用对象触发器'}
              </button>
              <button
                type="button"
                disabled={busy || !!pending}
                onClick={() => {
                  if (window.confirm('删除此触发器？后续不再排队，已排队调用和历史记录会保留。'))
                    change('DELETE');
                }}
              >
                {type === 'schedule' ? '删除定时计划' : '删除对象触发器'}
              </button>
              <button
                type="button"
                disabled={busy || !!pending}
                onClick={() => {
                  setSelected('');
                  setName('');
                  setAck(false);
                  setEvents([]);
                }}
              >
                {type === 'schedule' ? '新建另一计划' : '新建另一对象触发器'}
              </button>
            </>
          )}
        </form>
      )}
      {current && (
        <section aria-label={type === 'schedule' ? '定时调用记录' : '对象调用记录'}>
          <h4>{type === 'schedule' ? '定时调用记录' : '对象调用记录与重投递'}</h4>
          <button
            disabled={busy || !!pending}
            onClick={() => {
              setBusy(true);
              void history(current.trigger_id)
                .catch((e) => setError(String(e)))
                .finally(() => setBusy(false));
            }}
          >
            {type === 'schedule' ? '刷新定时调用' : '刷新对象调用'}
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
                  <td>
                    {v.state}
                    {v.redrive_succeeded ? ' · 已重投递完成' : ''}
                    {v.redrive_active ? ' · 重投递中' : ''}
                    {canEdit &&
                      v.type === 'storage_object_created' &&
                      !v.parent_invocation_id &&
                      ['failed', 'outcome_unknown'].includes(v.state) &&
                      !v.redrive_succeeded &&
                      !v.redrive_active &&
                      (v.redrive_count || 0) < 3 && (
                        <button
                          disabled={busy || !!pending || !objectEnabled}
                          onClick={() => {
                            if (
                              window.confirm(
                                '重投递同一个对象事件？可能重复执行副作用，请确认函数按调用标识去重。原失败记录会保留。',
                              )
                            )
                              void submit({
                                kind: 'redrive',
                                method: 'POST',
                                id: `${current.trigger_id}/invocations/${v.id}/retry`,
                                key: newRequestKey(),
                                version: v.version_token,
                                body: JSON.stringify({ acknowledge_duplicate_side_effects: true }),
                              });
                          }}
                        >
                          重投递对象事件
                        </button>
                      )}
                  </td>
                  <td>
                    {v.attempts} / {v.application_status || '—'}
                  </td>
                  <td>
                    {v.error_code || '—'}
                    <small>{v.request_event_id}</small>
                    {v.object && (
                      <small>
                        {v.object.bucket_name} / {v.object.object_key}
                      </small>
                    )}
                    {v.parent_invocation_id && <small>原回执 {v.parent_invocation_id}</small>}
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
              {type === 'schedule' ? '更多定时调用' : '更多对象调用'}
            </button>
          )}
          <p className="muted">
            保存必要的事件键、调用和响应状态，不保存对象内容或客户函数响应正文。succeeded 表示收到
            2xx 完整响应，业务是否成功由应用验证。
          </p>
        </section>
      )}
    </section>
  );
}
