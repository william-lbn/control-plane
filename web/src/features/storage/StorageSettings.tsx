import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { decodeAPIResponse } from '../../shared/apiResponse';
import { newRequestKey } from '../../shared/requestKey';

type Tag = { key: string; value: string };
type Block = {
  block_public_acls: boolean;
  ignore_public_acls: boolean;
  block_public_policy: boolean;
  restrict_public_buckets: boolean;
};
type Configuration = {
  tags: Tag[];
  public_access_block?: Block | null;
  versioning?: { status?: string; mfa_delete?: string };
  lifecycle?: unknown;
  object_lock?: unknown;
  response_headers?: Record<string, string>;
};
type Snapshot = { access?: 'private' | 'public_read'; settings: Configuration };
type Pending = { path: string; body: string; etag: string; key: string };
const noBlock: Block = {
  block_public_acls: false,
  ignore_public_acls: false,
  block_public_policy: false,
  restrict_public_buckets: false,
};
const blockLabels: Record<keyof Block, string> = {
  block_public_acls: '阻止新增公开 ACL',
  ignore_public_acls: '忽略公开 ACL',
  block_public_policy: '阻止新增公开策略',
  restrict_public_buckets: '限制公开桶的匿名读取',
};

export function StorageSettings({
  bucketPath,
  objectKey,
  canEdit,
  busy,
  onSaved,
}: {
  bucketPath: string;
  objectKey?: string;
  canEdit: boolean;
  busy: boolean;
  onSaved: () => Promise<void>;
}) {
  const objectMode = objectKey !== undefined;
  const path =
    bucketPath +
    (objectMode
      ? '/objects/settings?' + new URLSearchParams({ key: objectKey ?? '' })
      : '/settings');
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null),
    [etag, setETag] = useState(''),
    [access, setAccess] = useState<'private' | 'public_read'>('private'),
    [tags, setTags] = useState('[]'),
    [headers, setHeaders] = useState('{}'),
    [block, setBlock] = useState<Block | null>(null),
    [versioning, setVersioning] = useState(''),
    [lifecycle, setLifecycle] = useState('null'),
    [lock, setLock] = useState('null'),
    [working, setWorking] = useState(false),
    [uncertain, setUncertain] = useState(false),
    [error, setError] = useState(''),
    [notice, setNotice] = useState('');
  const pending = useRef<Pending | null>(null),
    epoch = useRef(0),
    active = useRef(false);
  useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
      epoch.current++;
      pending.current = null;
    };
  }, [path]);
  function current(id: number) {
    return active.current && epoch.current === id;
  }
  async function load() {
    const id = ++epoch.current;
    setWorking(true);
    setError('');
    try {
      const response = await fetch(path, { credentials: 'same-origin' });
      const value = await decodeAPIResponse<Snapshot>(response);
      const next = response.headers.get('ETag') || '';
      if (!current(id)) return;
      if (
        !/^"[a-f0-9]{64}"$/.test(next) ||
        !value.settings ||
        !Array.isArray(value.settings.tags) ||
        (!objectMode && value.access !== 'private' && value.access !== 'public_read')
      )
        throw Error('配置响应不完整，请重新读取当前状态');
      setSnapshot(value);
      setETag(next);
      setAccess(value.access || 'private');
      setTags(JSON.stringify(value.settings.tags, null, 2));
      setHeaders(JSON.stringify(value.settings.response_headers || {}, null, 2));
      setBlock(value.settings.public_access_block || null);
      setVersioning(value.settings.versioning?.status || '');
      setLifecycle(JSON.stringify(value.settings.lifecycle ?? null, null, 2));
      setLock(JSON.stringify(value.settings.object_lock ?? null, null, 2));
    } catch (e) {
      if (current(id)) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (current(id)) setWorking(false);
    }
  }
  async function send(request: Pending) {
    const id = ++epoch.current;
    setWorking(true);
    setError('');
    try {
      await api(request.path, {
        method: 'PUT',
        headers: { 'If-Match': request.etag, 'Idempotency-Key': request.key },
        body: request.body,
      });
      if (!current(id)) return;
      pending.current = null;
      setUncertain(false);
      setNotice('原提交已确认；已读取当前配置。');
      await load();
      if (active.current) await onSaved();
    } catch (e) {
      if (!current(id)) return;
      const definitive = e instanceof ApiError && e.status >= 400 && e.status < 500;
      if (definitive) {
        pending.current = null;
        setUncertain(false);
      } else {
        pending.current = request;
        setUncertain(true);
      }
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (current(id)) setWorking(false);
    }
  }
  function save() {
    if (!canEdit || working || busy || uncertain || !etag) return;
    setError('');
    setNotice('');
    try {
      const parsed: unknown = JSON.parse(tags);
      if (!Array.isArray(parsed)) throw Error('标签必须是 key/value 数组');
      const settings = objectMode
        ? { tags: parsed, response_headers: JSON.parse(headers) }
        : {
            tags: parsed,
            public_access_block: block,
            versioning: {
              ...(snapshot?.settings.versioning ?? {}),
              status: versioning || undefined,
            },
            lifecycle: JSON.parse(lifecycle),
            object_lock: JSON.parse(lock),
          };
      const request = {
        path,
        etag,
        key: newRequestKey(),
        body: JSON.stringify(objectMode ? { settings } : { access, settings }),
      };
      pending.current = request;
      void send(request);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }
  const blocked = busy || working || uncertain,
    kind = objectMode ? '对象' : '桶';
  const publicGrantBlocked = !!(
    block?.block_public_acls ||
    block?.block_public_policy ||
    snapshot?.settings.public_access_block?.block_public_acls ||
    snapshot?.settings.public_access_block?.block_public_policy
  );
  return (
    <section className="backend-panel" aria-label={kind + '设置'}>
      <h3>{objectMode ? '对象标签与响应头' : 'S3 桶配置'}</h3>
      {objectMode && (
        <p>
          对象：<code>{objectKey}</code>
        </p>
      )}
      <button className="button ghost" disabled={blocked} onClick={() => void load()}>
        加载{kind}设置
      </button>
      {snapshot && (
        <fieldset disabled={blocked || !canEdit}>
          <label>
            {kind}标签 JSON
            <textarea
              aria-label={kind + '标签 JSON'}
              rows={4}
              value={tags}
              onChange={(e) => setTags(e.target.value)}
            />
          </label>
          <p className="muted">
            标签使用 {'[{"key":"environment","value":"development"}]'}；
            {objectMode ? '最多 10 个' : '最多 50 个'}。
          </p>
          {objectMode ? (
            <label>
              对象响应头 JSON
              <textarea
                aria-label="对象响应头 JSON"
                rows={4}
                value={headers}
                onChange={(e) => setHeaders(e.target.value)}
              />
              <span className="muted">
                可配置 content-disposition 和 cache-control，文件字节保持不变。
              </span>
            </label>
          ) : (
            <>
              <label>
                桶公开访问
                <select
                  aria-label="桶配置访问级别"
                  value={access}
                  onChange={(e) => setAccess(e.target.value as typeof access)}
                >
                  <option value="private">私有</option>
                  <option
                    value="public_read"
                    disabled={publicGrantBlocked && snapshot.access !== 'public_read'}
                  >
                    公开读取
                  </option>
                </select>
              </label>
              <p>
                公开读取允许任何人下载已知对象路径。阻断配置可关闭匿名读取；不会撤销已有应用凭据。
              </p>
              {(Object.keys(blockLabels) as (keyof Block)[]).map((key) => (
                <label key={key}>
                  <input
                    type="checkbox"
                    aria-label={blockLabels[key]}
                    checked={!!block?.[key]}
                    onChange={(e) => setBlock({ ...noBlock, ...block, [key]: e.target.checked })}
                  />
                  {blockLabels[key]}
                </label>
              ))}
              <button className="button ghost" type="button" onClick={() => setBlock(null)}>
                移除公开访问阻断配置
              </button>
              {publicGrantBlocked && snapshot.access !== 'public_read' && (
                <p>先保存解除公开授权阻断，再开放公开读取。</p>
              )}
              <label>
                版本配置声明
                <select
                  aria-label="桶版本配置声明"
                  value={versioning}
                  onChange={(e) => setVersioning(e.target.value)}
                >
                  <option value="">未配置</option>
                  <option value="Enabled">Enabled</option>
                  <option value="Suspended">Suspended</option>
                </select>
              </label>
              <label>
                生命周期配置 JSON
                <textarea
                  aria-label="桶生命周期配置 JSON"
                  rows={5}
                  value={lifecycle}
                  onChange={(e) => setLifecycle(e.target.value)}
                />
              </label>
              <label>
                Object Lock 配置 JSON
                <textarea
                  aria-label="桶 Object Lock 配置 JSON"
                  rows={5}
                  value={lock}
                  onChange={(e) => setLock(e.target.value)}
                />
              </label>
              <p>
                版本、生命周期和 Object Lock 声明只保存配置，不执行对象版本、自动过期或 WORM 保留。
              </p>
            </>
          )}
          {canEdit && (
            <button className="button primary" type="button" disabled={!etag} onClick={save}>
              保存{kind}设置
            </button>
          )}
        </fieldset>
      )}
      {uncertain && (
        <p role="status">
          响应尚未确认，请保留当前页面并重试原提交。
          <button
            className="button"
            disabled={working || busy || !canEdit}
            onClick={() => {
              if (pending.current) void send(pending.current);
            }}
          >
            重试原{kind}设置提交
          </button>
        </p>
      )}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}
