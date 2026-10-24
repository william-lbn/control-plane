import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

export type AIStateReference = { id: string; kind: 'response' | 'conversation' };
type Handle = AIStateReference & {
  model_id: string;
  state: string;
  provider_status: string;
  expires_at: string;
};
type Pending = { path: string; method: string; body?: string; key: string };

export function AIProviderState({
  prefix,
  model,
  reference,
  onSelect,
  refresh,
  busy,
  canAdmin,
}: {
  prefix: string;
  model: string;
  reference: AIStateReference | null;
  onSelect: (value: AIStateReference | null) => void;
  refresh: number;
  busy: boolean;
  canAdmin: boolean;
}) {
  const [handles, setHandles] = useState<Handle[]>([]);
  const [working, setWorking] = useState(false),
    [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState(''),
    [details, setDetails] = useState('');
  const [metadata, setMetadata] = useState('{}'),
    [itemText, setItemText] = useState('');
  const [items, setItems] = useState<{ id: string }[]>([]);
  const [nextCursor, setNextCursor] = useState('');
  const [candidate, setCandidate] = useState(''),
    [evidence, setEvidence] = useState(''),
    [resolution, setResolution] = useState('adopted');
  const pending = useRef<Pending | null>(null),
    active = useRef(true);
  useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
    };
  }, []);
  async function reload() {
    const value = await api<{ items: Handle[] }>(prefix + '/provider-state');
    if (active.current) setHandles(value.items);
  }
  useEffect(() => {
    reload().catch((e) => {
      if (active.current) setError(String(e));
    });
  }, [prefix, refresh]);
  const pathFor = (h: AIStateReference) =>
    prefix +
    '/state/v1/' +
    (h.kind === 'response' ? 'responses/' : 'conversations/') +
    encodeURIComponent(h.id);
  async function send(request: Pending) {
    setWorking(true);
    setError('');
    try {
      const value = await api<Record<string, unknown>>(request.path, {
        method: request.method,
        body: request.body,
        headers: { 'X-Neon-Model': model, 'Idempotency-Key': request.key },
      });
      if (!active.current) return;
      if (request.method !== 'GET') {
        pending.current = null;
        setUncertain(false);
      }
      setDetails(JSON.stringify(value, null, 2));
      if (request.path.endsWith('/conversations') && typeof value.id === 'string')
        onSelect({ id: value.id, kind: 'conversation' });
      if (request.method === 'DELETE' && reference && request.path === pathFor(reference)) {
        onSelect(null);
        setItems([]);
      }
      if (request.method === 'DELETE' && request.path.includes('/items/'))
        setItems((old) =>
          old.filter((item) => !request.path.endsWith('/' + encodeURIComponent(item.id))),
        );
      if (Array.isArray(value.data))
        setNextCursor(
          value.has_more === true && typeof value.last_id === 'string' ? value.last_id : '',
        );
      if (Array.isArray(value.data))
        setItems(
          value.data.filter(
            (v): v is { id: string } =>
              typeof v === 'object' && v !== null && typeof v.id === 'string',
          ),
        );
      await reload();
    } catch (e) {
      if (active.current) {
        setError(String(e));
        if (request.method !== 'GET') {
          const rejected =
            e instanceof ApiError && [400, 401, 403, 404, 410, 422].includes(e.status);
          if (rejected) pending.current = null;
          setUncertain(!rejected);
          void reload().catch(() => {});
        }
      }
    } finally {
      if (active.current) setWorking(false);
    }
  }
  function mutate(path: string, method: string, value?: unknown) {
    if (pending.current) return;
    const request = {
      path,
      method,
      ...(value === undefined ? {} : { body: JSON.stringify(value) }),
      key: newRequestKey(),
    };
    pending.current = request;
    void send(request);
  }
  function read(path: string) {
    void send({ path, method: 'GET', key: '' });
  }
  const selected = handles.find((h) => h.id === reference?.id);
  const disabled = busy || working || uncertain;
  return (
    <section className="panel" aria-label="供应商持久响应与会话">
      <h3>供应商持久响应与会话</h3>
      <p>
        内容保存在供应商；本平台仅保存加密引用。历史上下文仍计费。引用有效期为 30
        天，过期不会自动删除供应商会话。删除会话也不会自动删除其中条目；如需移除条目，请先逐项删除。
      </p>
      {error && <p role="alert">{error}</p>}
      {uncertain && (
        <p role="status">
          原操作结果尚未确认，禁止新建或重发。
          {pending.current && (
            <button disabled={working} onClick={() => void send(pending.current!)}>
              读取原操作回执
            </button>
          )}
        </p>
      )}
      <label>
        持久上下文
        <select
          aria-label="持久上下文"
          value={reference?.id || ''}
          disabled={busy || working}
          onChange={(e) => {
            const h = handles.find((v) => v.id === e.target.value);
            onSelect(h ? { id: h.id, kind: h.kind } : null);
            setItems([]);
            setDetails('');
          }}
        >
          <option value="">不使用持久上下文</option>
          {handles
            .filter((h) => h.state !== 'deleted')
            .map((h) => (
              <option key={h.id} value={h.id}>
                {h.kind === 'conversation' ? '会话' : '响应'} {h.id} · {h.state}
              </option>
            ))}
        </select>
      </label>
      <label>
        会话元数据 JSON
        <textarea
          aria-label="会话元数据 JSON"
          value={metadata}
          disabled={disabled}
          onChange={(e) => setMetadata(e.target.value)}
        />
      </label>
      <button
        disabled={disabled}
        onClick={() => {
          try {
            mutate(prefix + '/state/v1/conversations', 'POST', { metadata: JSON.parse(metadata) });
          } catch (e) {
            setError(String(e));
          }
        }}
      >
        创建持久会话
      </button>
      {selected && (
        <>
          <p>
            当前引用：<code>{selected.id}</code> · {selected.provider_status} · 到期{' '}
            {selected.expires_at}
          </p>
          <button disabled={busy || working} onClick={() => read(pathFor(selected))}>
            读取持久资源
          </button>
          <button disabled={disabled} onClick={() => mutate(pathFor(selected), 'DELETE')}>
            {selected.kind === 'conversation' ? '删除供应商会话' : '删除供应商响应'}
          </button>
          {nextCursor && (
            <button
              disabled={busy || working}
              onClick={() =>
                read(
                  pathFor(selected) +
                    (selected.kind === 'conversation' ? '/items' : '/input_items') +
                    '?limit=20&after=' +
                    encodeURIComponent(nextCursor),
                )
              }
            >
              下一页上下文条目
            </button>
          )}
          <button
            disabled={busy || working}
            onClick={() =>
              read(
                pathFor(selected) + (selected.kind === 'conversation' ? '/items' : '/input_items'),
              )
            }
          >
            读取上下文条目
          </button>
          {selected.kind === 'conversation' && (
            <>
              <button
                disabled={disabled}
                onClick={() => {
                  try {
                    mutate(pathFor(selected), 'POST', { metadata: JSON.parse(metadata) });
                  } catch (e) {
                    setError(String(e));
                  }
                }}
              >
                更新会话元数据
              </button>
              <label>
                追加会话文本
                <textarea
                  aria-label="追加会话文本"
                  value={itemText}
                  disabled={disabled}
                  onChange={(e) => setItemText(e.target.value)}
                />
              </label>
              <button
                disabled={disabled || !itemText.trim()}
                onClick={() =>
                  mutate(pathFor(selected) + '/items', 'POST', {
                    items: [{ type: 'message', role: 'user', content: itemText }],
                  })
                }
              >
                追加会话条目
              </button>
              {items.map((item) => (
                <div key={item.id}>
                  <code>{item.id}</code>
                  <button
                    disabled={disabled}
                    onClick={() =>
                      mutate(pathFor(selected) + '/items/' + encodeURIComponent(item.id), 'DELETE')
                    }
                  >
                    删除会话条目
                  </button>
                </div>
              ))}
            </>
          )}
        </>
      )}
      {canAdmin && selected && (
        <fieldset>
          <legend>管理员确认原操作</legend>
          <p>
            仅在核对供应商记录后使用。系统只读取并验证原资源，不会重发创建、写入或推理；费用对账仍须单独确认。
          </p>
          <label>
            核对结果
            <select
              aria-label="持久状态核对结果"
              value={resolution}
              onChange={(e) => setResolution(e.target.value)}
            >
              <option value="adopted">找回原创建资源</option>
              <option value="confirmed_applied">原写操作已生效</option>
              <option value="confirmed_not_accepted">原写操作未受理</option>
              <option value="deleted">原删除已完成</option>
            </select>
          </label>
          <label>
            供应商原资源 ID（创建回执丢失时填写）
            <input
              aria-label="供应商原资源 ID"
              value={candidate}
              onChange={(e) => setCandidate(e.target.value)}
            />
          </label>
          <label>
            核对证据 SHA-256
            <input
              aria-label="持久状态核对证据"
              value={evidence}
              onChange={(e) => setEvidence(e.target.value)}
            />
          </label>
          <button
            disabled={busy || working || !/^[a-f0-9]{64}$/.test(evidence)}
            onClick={() => {
              const path = pathFor(selected) + '/reconcile';
              const request =
                pending.current?.path === path
                  ? pending.current
                  : {
                      path,
                      method: 'POST',
                      key: newRequestKey(),
                      body: JSON.stringify({
                        resolution,
                        evidence_sha256: evidence,
                        ...(candidate ? { remote_id: candidate } : {}),
                      }),
                    };
              pending.current = request;
              void send(request);
            }}
          >
            确认原操作并保留回执
          </button>
        </fieldset>
      )}
      {details && <pre aria-label="持久资源结果">{details}</pre>}
    </section>
  );
}
