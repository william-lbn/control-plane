import { useEffect, useRef, useState } from 'react';
import './storage.css';
import { api, projectPath } from '../../api';
import type { Branch, Operation } from '../../api';
import { PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { followOperation } from '../../shared/followOperation';

type Instance = {
  generation: number;
  state: string;
  driver_enabled: boolean;
  spec: { database: string } | null;
};
type Bucket = {
  name: string;
  access: 'private' | 'public_read';
  object_count: number;
  bytes: number;
};
type ObjectEntry = {
  key: string;
  sha256: string;
  size: number;
  content_type: string;
  updated_at: string;
};
const objectLimit = 8 * 1024 * 1024;

export function ObjectStorage({
  projectId,
  branches,
  canEdit,
  canAdmin,
  showError,
}: {
  projectId: string;
  branches: Branch[];
  canEdit: boolean;
  canAdmin: boolean;
  showError: (e: unknown) => void;
}) {
  const [branch, setBranch] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [instance, setInstance] = useState<Instance | null>(null);
  const [database, setDatabase] = useState('postgres');
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [bucket, setBucket] = useState('');
  const [bucketName, setBucketName] = useState('');
  const [access, setAccess] = useState<'private' | 'public_read'>('private');
  const [objects, setObjects] = useState<ObjectEntry[]>([]);
  const [prefix, setPrefix] = useState('');
  const [after, setAfter] = useState('');
  const [key, setKey] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [overwrite, setOverwrite] = useState(false);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const [operation, setOperation] = useState<Operation | null>(null);
  const epoch = useRef(0);
  const tracking = useRef<AbortController | null>(null);
  const uncertainKey = useRef<{ fingerprint: string; key: string } | null>(null);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/storage`;
  const bucketPath = `${path}/buckets/${encodeURIComponent(bucket)}`;
  async function loadState(current: number) {
    const value = await api<Instance>(path);
    if (epoch.current !== current) return;
    setInstance(value);
    if (value.spec) setDatabase(value.spec.database);
  }
  useEffect(() => {
    const current = ++epoch.current;
    tracking.current?.abort();
    uncertainKey.current = null;
    setInstance(null);
    setBuckets([]);
    setBucket('');
    setObjects([]);
    setNotice('');
    setOperation(null);
    setBusy(false);
    setFile(null);
    setKey('');
    setOverwrite(false);
    setAfter('');
    if (branch) void loadState(current).catch(showError);
    const timer = setInterval(() => {
      if (branch) void loadState(current).catch(() => {});
    }, 5000);
    return () => {
      clearInterval(timer);
      tracking.current?.abort();
    };
  }, [branch, projectId]);
  async function run(task: (current: number) => Promise<void>) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      await task(current);
    } catch (e) {
      if (epoch.current === current) showError(e);
    } finally {
      if (epoch.current === current) setBusy(false);
    }
  }
  async function mutate(disable: boolean) {
    if (!instance) return;
    const body = disable ? undefined : { database };
    const fingerprint = JSON.stringify([path, instance.generation, disable, body]);
    if (uncertainKey.current?.fingerprint !== fingerprint)
      uncertainKey.current = { fingerprint, key: newRequestKey() };
    await run(async (current) => {
      const value = await api<{ operation: Operation }>(path, {
        method: disable ? 'DELETE' : 'POST',
        headers: {
          'If-Match': `"${instance.generation}"`,
          'Idempotency-Key': uncertainKey.current!.key,
        },
        body: body ? JSON.stringify(body) : undefined,
      });
      if (epoch.current !== current) return;
      setOperation(value.operation);
      const controller = new AbortController();
      tracking.current = controller;
      const final = await followOperation<Operation>({
        id: value.operation.id,
        signal: controller.signal,
        read: (signal) =>
          api<Operation>(`${projectPath(projectId)}/operations/${value.operation.id}`, { signal }),
        onValue: (op) => {
          if (epoch.current === current) setOperation(op);
        },
        onUnavailable: () => {
          if (epoch.current === current) setNotice('状态服务暂不可用，继续查询原 Operation。');
        },
      });
      if (epoch.current !== current) return;
      if (!final) {
        setNotice('请在操作记录中继续查询原 Operation。');
        return;
      }
      setOperation(final);
      await loadState(current);
      if (final.state === 'succeeded') {
        uncertainKey.current = null;
        setBuckets([]);
        setBucket('');
        setObjects([]);
        setNotice(
          disable
            ? '对象存储已禁用；目录与文件保留，旧下载链接失效。'
            : '对象存储已启用。加载存储桶将按需唤醒 Compute。',
        );
      }
    });
  }
  async function loadBuckets(current: number) {
    const value = await api<{ items: Bucket[] }>(`${path}/buckets`);
    if (epoch.current !== current) return;
    setBuckets(value.items);
    if (!value.items.some((b) => b.name === bucket)) {
      setBucket(value.items[0]?.name || '');
      setObjects([]);
      setAfter('');
    }
  }
  async function loadObjects(current: number, cursor = '') {
    const value = await api<{ items: ObjectEntry[]; next_after: string }>(
      `${bucketPath}/objects?${new URLSearchParams({ prefix, after: cursor })}`,
    );
    if (epoch.current !== current) return;
    setObjects(value.items);
    setAfter(value.next_after);
  }
  async function createBucket() {
    await run(async (current) => {
      await api(`${path}/buckets`, {
        method: 'POST',
        body: JSON.stringify({ name: bucketName, access }),
      });
      if (epoch.current !== current) return;
      await loadBuckets(current);
      setBucket(bucketName);
      setBucketName('');
      setObjects([]);
      setAfter('');
      setNotice('存储桶已创建');
    });
  }
  async function upload() {
    if (!file || !key || file.size > objectLimit) return;
    const existing = objects.find((item) => item.key === key);
    if (overwrite && !existing) {
      setNotice('覆盖前请加载当前对象目录并确认 ETag。');
      return;
    }
    await run(async (current) => {
      await api(`${bucketPath}/objects?${new URLSearchParams({ key })}`, {
        method: 'PUT',
        headers: {
          'Content-Type': file.type || 'application/octet-stream',
          ...(overwrite ? { 'If-Match': `"${existing!.sha256}"` } : { 'If-None-Match': '*' }),
        },
        body: file,
      });
      if (epoch.current !== current) return;
      await loadObjects(current);
      await loadBuckets(current);
      setNotice('上传已完成，SHA-256 和目录已提交');
    });
  }
  async function removeObject(item: ObjectEntry) {
    if (!window.confirm(`删除当前分支的 ${item.key}？其他分支及历史文件保留。`)) return;
    await run(async (current) => {
      await api(`${bucketPath}/objects?${new URLSearchParams({ key: item.key })}`, {
        method: 'DELETE',
        headers: { 'If-Match': `"${item.sha256}"` },
      });
      if (epoch.current !== current) return;
      await loadObjects(current);
      await loadBuckets(current);
      setNotice('当前分支对象已删除；共享文件保留');
    });
  }
  async function download(item: ObjectEntry) {
    await run(async (current) => {
      const value = await api<{ url: string }>(`${bucketPath}/presign`, {
        method: 'POST',
        body: JSON.stringify({ key: item.key, expires_seconds: 60 }),
      });
      if (epoch.current !== current) return;
      const link = document.createElement('a');
      link.href = value.url;
      link.download = item.key.split('/').at(-1) || 'download';
      document.body.appendChild(link);
      link.click();
      link.remove();
      setNotice('下载链接已签发，有效期 60 秒');
    });
  }
  const active = instance?.state === 'active';
  return (
    <div className="storage-workspace">
      <PageHeading
        kicker="BRANCH FILES"
        title="Object Storage"
        description="文件与数据库共享分支快照；父子分支修改彼此隔离。"
      />
      <section className="panel pad backend-panel storage-panel">
        <div className="panel-heading">
          <h2>分支对象服务</h2>
          {instance && status(instance.state)}
        </div>
        <div className="backend-form">
          <label>
            存储分支
            <select
              aria-label="存储分支"
              value={branch}
              disabled={busy}
              onChange={(e) => setBranch(e.target.value)}
            >
              {branches.map((b) => (
                <option key={b.id} value={b.id}>
                  {b.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            存储数据库
            <input
              aria-label="存储数据库"
              value={database}
              disabled={busy || !!instance?.spec || !canAdmin}
              onChange={(e) => setDatabase(e.target.value)}
            />
          </label>
        </div>
        <p className="muted">
          REST v1 · 单文件 8 MiB · 分支目录 100 MiB / 1000 个对象 ·
          物理文件保留以保护分支和恢复历史。S3 客户端兼容性仍待实现。
        </p>
        <div className="storage-actions">
          {canAdmin && (
            <button
              className="button"
              disabled={
                busy || !instance?.driver_enabled || (!!instance && instance.state !== 'disabled')
              }
              onClick={() => void mutate(false)}
            >
              启用对象存储
            </button>
          )}
          {canAdmin && (
            <button
              className="button danger"
              disabled={busy || !active}
              onClick={() => void mutate(true)}
            >
              禁用对象存储
            </button>
          )}
          <button
            className="button"
            disabled={busy || !active}
            onClick={() => void run(loadBuckets)}
          >
            加载存储桶
          </button>
        </div>
        {operation && (
          <p className="storage-notice" role="status" data-testid="storage-operation">
            {operation.id} · {operation.state}
            {operation.error_code && ` · ${operation.error_code}`}
          </p>
        )}
        {notice && (
          <p className="storage-notice" role="status" data-testid="storage-notice">
            {notice}
          </p>
        )}
      </section>
      {active && (
        <>
          <section className="panel pad backend-panel storage-panel">
            <div className="panel-heading">
              <h2>存储桶</h2>
              <span className="muted">分支 {branch}</span>
            </div>
            {canEdit && (
              <div className="storage-actions">
                <label>
                  存储桶名称
                  <input
                    aria-label="存储桶名称"
                    value={bucketName}
                    onChange={(e) => setBucketName(e.target.value)}
                    disabled={busy}
                    placeholder="uploads"
                  />
                </label>
                <label>
                  访问级别
                  <select
                    aria-label="存储桶访问级别"
                    value={access}
                    onChange={(e) => setAccess(e.target.value as typeof access)}
                    disabled={busy}
                  >
                    <option value="private">私有 · 需要授权</option>
                    <option value="public_read">公共读取 · 匿名可下载</option>
                  </select>
                </label>
                <button
                  className="button primary"
                  disabled={busy || !bucketName}
                  onClick={() => void createBucket()}
                >
                  创建存储桶
                </button>
              </div>
            )}
            <table>
              <thead>
                <tr>
                  <th>存储桶</th>
                  <th>访问</th>
                  <th>对象</th>
                  <th>容量</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {buckets.map((b) => (
                  <tr key={b.name}>
                    <td>{b.name}</td>
                    <td>{b.access}</td>
                    <td>{b.object_count}</td>
                    <td>{b.bytes} B</td>
                    <td>
                      <button
                        className="button"
                        disabled={busy}
                        onClick={() => {
                          setBucket(b.name);
                          setObjects([]);
                          setAfter('');
                        }}
                      >
                        选择
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!buckets.length && (
              <p className="muted">加载目录后可以查看已有桶，或创建新的存储桶。</p>
            )}
          </section>
          {bucket && (
            <section className="panel pad backend-panel storage-panel">
              <div className="panel-heading">
                <h2>对象目录 · {bucket}</h2>
              </div>
              <div className="storage-actions">
                <label>
                  对象前缀
                  <input
                    aria-label="对象前缀"
                    value={prefix}
                    disabled={busy}
                    onChange={(e) => setPrefix(e.target.value)}
                  />
                </label>
                <button
                  className="button"
                  disabled={busy}
                  onClick={() => void run((current) => loadObjects(current))}
                >
                  加载对象
                </button>
                {after && (
                  <button
                    className="button"
                    disabled={busy}
                    onClick={() => void run((current) => loadObjects(current, after))}
                  >
                    下一页
                  </button>
                )}
              </div>
              {canEdit && (
                <div className="storage-actions">
                  <label>
                    对象键
                    <input
                      aria-label="对象键"
                      value={key}
                      onChange={(e) => setKey(e.target.value)}
                      disabled={busy}
                      placeholder="documents/report.txt"
                    />
                  </label>
                  <label>
                    文件
                    <input
                      aria-label="上传文件"
                      type="file"
                      disabled={busy}
                      onChange={(e) => {
                        const value = e.target.files?.[0] || null;
                        setFile(value);
                        if (!key && value) setKey(value.name);
                      }}
                    />
                  </label>
                  <label>
                    <input
                      type="checkbox"
                      aria-label="覆盖已有对象"
                      checked={overwrite}
                      disabled={busy}
                      onChange={(e) => setOverwrite(e.target.checked)}
                    />{' '}
                    使用当前 ETag 覆盖
                  </label>
                  <button
                    className="button"
                    disabled={busy || !file || !key || file.size > objectLimit}
                    onClick={() => void upload()}
                  >
                    上传对象
                  </button>
                </div>
              )}
              {file && file.size > objectLimit && <p role="alert">文件超过 8 MiB 上限</p>}
              <table>
                <thead>
                  <tr>
                    <th>对象键</th>
                    <th>大小</th>
                    <th>类型</th>
                    <th>SHA-256</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {objects.map((item) => (
                    <tr key={item.key}>
                      <td>{item.key}</td>
                      <td>{item.size} B</td>
                      <td>{item.content_type}</td>
                      <td>
                        <code title={item.sha256}>{item.sha256.slice(0, 16)}…</code>
                      </td>
                      <td>
                        <button
                          className="button"
                          disabled={busy || !canEdit}
                          onClick={() => void download(item)}
                        >
                          下载
                        </button>
                        {canEdit && (
                          <button
                            className="button danger"
                            disabled={busy}
                            onClick={() => void removeObject(item)}
                          >
                            删除
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {canEdit && (
                <button
                  className="button danger"
                  disabled={busy || (buckets.find((b) => b.name === bucket)?.object_count || 0) > 0}
                  onClick={() => {
                    if (!window.confirm(`删除空桶 ${bucket}？`)) return;
                    void run(async (current) => {
                      await api(bucketPath, { method: 'DELETE' });
                      if (epoch.current === current) {
                        setObjects([]);
                        setAfter('');
                        await loadBuckets(current);
                        setNotice('空存储桶已删除');
                      }
                    });
                  }}
                >
                  删除空存储桶
                </button>
              )}
            </section>
          )}
        </>
      )}
    </div>
  );
}
