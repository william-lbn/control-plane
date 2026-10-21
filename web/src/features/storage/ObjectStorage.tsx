import { useEffect, useRef, useState } from 'react';
import './storage.css';
import { storageBlobDigest } from './fileDigest';
import { api, projectPath } from '../../api';
import type { Branch, Operation } from '../../api';
import { PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { followOperation } from '../../shared/followOperation';
import { MultipartUpload } from './MultipartUpload';
import { MultipartAdministration } from './MultipartAdministration';

type Instance = {
  generation: number;
  state: string;
  driver_enabled: boolean;
  s3_compatible?: boolean;
  s3_endpoint?: string;
  s3_region?: string;
  s3_missing?: string[];
  s3_supported?: string[];
  limits?: { object_bytes: number; branch_bytes: number; part_bytes?: number };
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
  const objectLimit = instance?.limits?.object_bytes ?? 8 * 1024 * 1024;
  const branchLimit = instance?.limits?.branch_bytes ?? 100 * 1024 * 1024;
  const [database, setDatabase] = useState('postgres');
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [bucket, setBucket] = useState('');
  const [bucketName, setBucketName] = useState('');
  const [access, setAccess] = useState<'private' | 'public_read'>('private');
  const [objects, setObjects] = useState<ObjectEntry[]>([]);
  const [selectedKeys, setSelectedKeys] = useState<string[]>([]);
  const [prefix, setPrefix] = useState('');
  const [folders, setFolders] = useState<string[]>([]);
  const [directoryMode, setDirectoryMode] = useState(false);
  const [copySource, setCopySource] = useState<(ObjectEntry & { bucket: string }) | null>(null);
  const [copyBucket, setCopyBucket] = useState('');
  const [copyKey, setCopyKey] = useState('');
  const [after, setAfter] = useState('');
  const [key, setKey] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [overwrite, setOverwrite] = useState(false);
  const [metadataJSON, setMetadataJSON] = useState('{}');
  const [corsJSON, setCORSJSON] = useState('[]');
  const [corsETag, setCORSETag] = useState('');
  const [objectMetadata, setObjectMetadata] = useState<Record<string, string> | null>(null);
  const [metadataSelection, setMetadataSelection] = useState<{ key: string; etag: string } | null>(
    null,
  );
  const [metadataEdit, setMetadataEdit] = useState('{}');
  const [uploadCapability, setUploadCapability] = useState<{
    url: string;
    headers: Record<string, string>;
    expires_at: string;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const [operation, setOperation] = useState<Operation | null>(null);
  const epoch = useRef(0);
  const tracking = useRef<AbortController | null>(null);
  const uncertainKey = useRef<{ fingerprint: string; key: string } | null>(null);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/storage`;
  const bucketPath = `${path}/buckets/${encodeURIComponent(bucket)}`;
  useEffect(() => {
    setCORSETag('');
    setCORSJSON('[]');
    setObjectMetadata(null);
    setUploadCapability(null);
  }, [branch, bucket]);
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
    setSelectedKeys([]);
    setBuckets([]);
    setBucket('');
    setObjects([]);
    setSelectedKeys([]);
    setNotice('');
    setOperation(null);
    setBusy(false);
    setFile(null);
    setKey('');
    setOverwrite(false);
    setMetadataJSON('{}');
    setCORSJSON('[]');
    setCORSETag('');
    setObjectMetadata(null);
    setUploadCapability(null);
    setAfter('');
    setPrefix('');
    setFolders([]);
    setDirectoryMode(false);
    setCopySource(null);
    setCopyBucket('');
    setCopyKey('');
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
        setSelectedKeys([]);
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
      setSelectedKeys([]);
      setAfter('');
    }
  }
  async function loadObjects(
    current: number,
    cursor = '',
    selectedPrefix = prefix,
    selectedBucket = bucket,
  ) {
    const value = await api<{
      items: ObjectEntry[];
      next_after: string;
      common_prefixes?: string[];
    }>(
      `${path}/buckets/${encodeURIComponent(selectedBucket)}/objects?${new URLSearchParams({ prefix: selectedPrefix, after: cursor, ...(directoryMode ? { delimiter: '/' } : {}) })}`,
    );
    if (epoch.current !== current) return;
    setObjects(value.items);
    setSelectedKeys([]);
    setFolders(value.common_prefixes ?? []);
    setAfter(value.next_after);
  }
  async function copyObject() {
    if (!copySource || !copyBucket || !copyKey) return;
    await run(async (current) => {
      await api(`${path}/buckets/${encodeURIComponent(copyBucket)}/copy`, {
        method: 'POST',
        headers: { 'If-None-Match': '*' },
        body: JSON.stringify({
          source_bucket: copySource.bucket,
          source_key: copySource.key,
          source_sha256: copySource.sha256,
          destination_key: copyKey,
        }),
      });
      if (epoch.current !== current) return;
      setCopySource(null);
      setCopyKey('');
      setNotice('对象已复制；原对象和共享不可变内容保留。');
      setBucket(copyBucket);
      setPrefix('');
      setAfter('');
      await loadBuckets(current);
      await loadObjects(current, '', '', copyBucket);
    });
  }
  async function deleteSelected() {
    const selected = objects.filter((item) => selectedKeys.includes(item.key));
    if (!selected.length) return;
    if (
      !window.confirm(
        `删除所选 ${selected.length} 个对象的当前目录记录？原内容仍按分支和历史引用保留。`,
      )
    )
      return;
    await run(async (current) => {
      await api(`${bucketPath}/delete-batch`, {
        method: 'POST',
        body: JSON.stringify({
          objects: selected.map((item) => ({ key: item.key, sha256: item.sha256 })),
        }),
      });
      if (epoch.current !== current) return;
      setSelectedKeys([]);
      setNotice(`已删除 ${selected.length} 个对象的当前目录记录，物理内容仍保留。`);
      await loadBuckets(current);
      await loadObjects(current);
    });
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
      setSelectedKeys([]);
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
      setNotice('正在按块计算文件 SHA-256…');
      const digest = await storageBlobDigest(file);
      if (epoch.current !== current) return;
      const receipt = await api<{ sha256: string; size: number }>(
        `${bucketPath}/objects?${new URLSearchParams({ key })}`,
        {
          method: 'PUT',
          headers: {
            'Content-Type': file.type || 'application/octet-stream',
            'X-Neon-Object-Metadata': JSON.stringify(JSON.parse(metadataJSON)),
            'X-Amz-Checksum-Sha256': digest.base64,
            ...(overwrite ? { 'If-Match': `"${existing!.sha256}"` } : { 'If-None-Match': '*' }),
          },
          body: file,
        },
      );
      if (receipt.sha256 !== digest.hex || receipt.size !== file.size)
        throw Error('上传回执与原文件不符，请保留原文件并读取对象状态。');
      if (epoch.current !== current) return;
      await loadObjects(current);
      await loadBuckets(current);
      setNotice('上传已完成，SHA-256 和目录已提交');
    });
  }
  async function loadCORS() {
    await run(async (current) => {
      const response = await fetch(`${bucketPath}/cors`, { credentials: 'same-origin' });
      if (!response.ok) throw new Error(`加载 CORS 失败 (${response.status})`);
      const value = await response.json();
      if (epoch.current !== current) return;
      setCORSJSON(JSON.stringify(value.rules, null, 2));
      setCORSETag(response.headers.get('ETag') || '');
    });
  }
  async function saveCORS() {
    await run(async (current) => {
      await api(`${bucketPath}/cors`, {
        method: 'PUT',
        headers: { 'If-Match': corsETag },
        body: JSON.stringify({ rules: JSON.parse(corsJSON) }),
      });
      if (epoch.current !== current) return;
      setCORSETag('');
      setNotice('CORS 已提交；签名与权限仍必需，浏览器 cookie 不获授权。');
    });
  }
  async function inspectMetadata(item: ObjectEntry) {
    await run(async (current) => {
      const response = await fetch(
        `${bucketPath}/objects?${new URLSearchParams({ key: item.key })}`,
        { method: 'HEAD', credentials: 'same-origin' },
      );
      if (!response.ok) throw new Error(`读取元数据失败 (${response.status})`);
      const data: Record<string, string> = {};
      response.headers.forEach((value, name) => {
        if (name.startsWith('x-amz-meta-')) data[name.slice(11)] = value;
      });
      if (epoch.current === current) {
        setObjectMetadata(data);
        setMetadataEdit(JSON.stringify(data, null, 2));
        setMetadataSelection({
          key: item.key,
          etag: response.headers.get('X-Neon-Metadata-ETag') || '',
        });
      }
    });
  }
  async function saveMetadata() {
    if (!metadataSelection) return;
    await run(async (current) => {
      const value = await api<{ metadata: Record<string, string>; metadata_etag: string }>(
        `${bucketPath}/objects/metadata?${new URLSearchParams({ key: metadataSelection.key })}`,
        {
          method: 'PATCH',
          headers: { 'If-Match': metadataSelection.etag },
          body: JSON.stringify({ metadata: JSON.parse(metadataEdit) }),
        },
      );
      if (epoch.current !== current) return;
      setObjectMetadata(value.metadata);
      setMetadataEdit(JSON.stringify(value.metadata, null, 2));
      setMetadataSelection({ ...metadataSelection, etag: value.metadata_etag });
      setNotice('对象元数据已更新，文件字节未改变。');
    });
  }
  async function prepareUpload() {
    if (!file || !key || overwrite || file.size > objectLimit) return;
    await run(async (current) => {
      const digest = (await storageBlobDigest(file)).hex;
      if (epoch.current !== current) return;
      const value = await api<{ url: string; headers: Record<string, string>; expires_at: string }>(
        `${bucketPath}/presign-upload`,
        {
          method: 'POST',
          body: JSON.stringify({
            key,
            sha256: digest,
            size: file.size,
            content_type: file.type || 'application/octet-stream',
            expires_seconds: 300,
          }),
        },
      );
      if (epoch.current !== current) return;
      setUploadCapability(value);
      setNotice(
        '临时上传地址已生成：5 分钟内有效，只允许此文件创建新对象。地址持有者可上传，请勿公开。',
      );
    });
  }
  async function uploadPrepared() {
    if (!file || !uploadCapability) return;
    const capability = uploadCapability;
    await run(async (current) => {
      // No Console cookie or CSRF is required by the capability endpoint.
      const response = await fetch(capability.url, {
        method: 'PUT',
        headers: capability.headers,
        body: file,
        credentials: 'omit',
        redirect: 'error',
        referrerPolicy: 'no-referrer',
      });
      if (!response.ok)
        throw new Error(`临时上传失败 (${response.status})；请检查文件、有效期和对象版本。`);
      if (epoch.current !== current) return;
      setUploadCapability(null);
      await loadObjects(current);
      await loadBuckets(current);
      setNotice('预签名上传已完成，文件摘要与大小已校验。');
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
          REST v1 · 单文件 {objectLimit / 1024 / 1024} MiB · 分支目录 {branchLimit / 1024 / 1024}{' '}
          MiB / 1000 个对象 · 物理文件保留以保护分支和恢复历史。S3 基础兼容的部署状态与范围见下方。
        </p>
        <section className="storage-s3-panel" data-testid="storage-s3-panel">
          <h3>S3 应用连接</h3>
          {instance?.s3_compatible ? (
            <>
              <label>
                分支 S3 Endpoint
                <input
                  aria-label="S3 Endpoint"
                  readOnly
                  value={instance.s3_endpoint || ''}
                  style={{ width: '100%' }}
                />
              </label>
              <p>
                Region：<code>{instance.s3_region}</code> · SDK 使用
                forcePathStyle。应用凭据页面可签发 S3 只读/读写密钥、轮换或撤销；Secret 只返回一次。
              </p>
              <a href={`#/projects/${projectId}/credentials`}>管理应用凭据 →</a>
              <p className="muted">
                SigV4 与预签名上传下载 · 单对象 {objectLimit / 1024 / 1024} MiB。
                {instance.s3_supported?.includes('StreamedPutObject') &&
                  '支持流式大对象与完整 SHA-256 校验。'}
                未支持：
                {(instance.s3_missing || []).join('、')}。
              </p>
            </>
          ) : (
            <p>此环境尚未配置 S3 兼容入口；REST 上传下载保持可用。</p>
          )}
        </section>
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
                          setSelectedKeys([]);
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
                <label>
                  <input
                    aria-label="按目录浏览"
                    type="checkbox"
                    checked={directoryMode}
                    disabled={busy}
                    onChange={(e) => {
                      setDirectoryMode(e.target.checked);
                      setAfter('');
                      setFolders([]);
                      setObjects([]);
                      setSelectedKeys([]);
                    }}
                  />
                  按目录浏览
                </label>
                {directoryMode && prefix && (
                  <button
                    className="button ghost"
                    disabled={busy}
                    onClick={() => {
                      const parent = prefix.replace(/\/$/, '').replace(/[^/]*$/, '');
                      setPrefix(parent);
                      void run((current) => loadObjects(current, '', parent));
                    }}
                  >
                    上级目录
                  </button>
                )}
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
              {folders.length > 0 && (
                <nav aria-label="对象子目录" className="storage-actions">
                  {folders.map((folder) => (
                    <button
                      key={folder}
                      className="button ghost"
                      disabled={busy}
                      onClick={() => {
                        setPrefix(folder);
                        void run((current) => loadObjects(current, '', folder));
                      }}
                    >
                      📁 {folder}
                    </button>
                  ))}
                </nav>
              )}
              <MultipartUpload
                key={`${projectId}/${branch}/${bucket}`}
                bucketPath={bucketPath}
                objectBytes={objectLimit}
                canEdit={canEdit}
                showError={showError}
                onPublished={async () => {
                  const current = epoch.current;
                  await loadObjects(current);
                  await loadBuckets(current);
                }}
              />
              {canAdmin && (
                <MultipartAdministration
                  key={`admin/${projectId}/${branch}/${bucket}`}
                  bucketPath={bucketPath}
                />
              )}
              <section className="backend-panel" data-testid="storage-cors-panel">
                <h3>浏览器跨域访问</h3>
                <p>
                  仅控制浏览器许可；实际请求仍需签名和当前存储权限。默认无规则。保存前加载当前版本。
                </p>
                <button
                  className="button ghost"
                  disabled={busy || !bucket}
                  onClick={() => void loadCORS()}
                >
                  加载 CORS 规则
                </button>
                <label>
                  CORS 规则 JSON
                  <textarea
                    aria-label="CORS 规则 JSON"
                    value={corsJSON}
                    disabled={busy || !canEdit}
                    onChange={(e) => setCORSJSON(e.target.value)}
                  />
                </label>
                {canEdit && (
                  <button
                    className="button"
                    disabled={busy || !corsETag}
                    onClick={() => void saveCORS()}
                  >
                    保存 CORS 规则
                  </button>
                )}
              </section>
              {objectMetadata && (
                <section className="backend-panel" data-testid="storage-object-metadata">
                  <h3>对象元数据</h3>
                  <pre>{JSON.stringify(objectMetadata, null, 2)}</pre>
                  {canEdit && metadataSelection && (
                    <>
                      <label>
                        编辑对象元数据 JSON
                        <textarea
                          aria-label="编辑对象元数据 JSON"
                          value={metadataEdit}
                          disabled={busy}
                          onChange={(e) => setMetadataEdit(e.target.value)}
                        />
                      </label>
                      <button
                        className="button"
                        disabled={busy || !metadataSelection.etag}
                        onClick={() => void saveMetadata()}
                      >
                        保存对象元数据
                      </button>
                      <p>以已读取状态为条件；冲突后请重新读取，不自动覆盖新属性。</p>
                    </>
                  )}
                </section>
              )}
              {canEdit && copySource && (
                <section className="backend-panel" data-testid="storage-copy-panel">
                  <h3>复制对象</h3>
                  <p>
                    来源：{copySource.bucket}/{copySource.key}
                    。目标必须不存在；源版本有变化时会拒绝复制。
                  </p>
                  <div className="storage-actions">
                    <label>
                      复制目标存储桶
                      <select
                        aria-label="复制目标存储桶"
                        value={copyBucket}
                        disabled={busy}
                        onChange={(e) => setCopyBucket(e.target.value)}
                      >
                        {buckets.map((item) => (
                          <option key={item.name} value={item.name}>
                            {item.name}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      复制目标对象键
                      <input
                        aria-label="复制目标对象键"
                        value={copyKey}
                        disabled={busy}
                        maxLength={1024}
                        onChange={(e) => setCopyKey(e.target.value)}
                      />
                    </label>
                    <button
                      className="button"
                      disabled={
                        busy ||
                        !copyBucket ||
                        !copyKey ||
                        (copyBucket === copySource.bucket && copyKey === copySource.key)
                      }
                      onClick={() => void copyObject()}
                    >
                      确认复制对象
                    </button>
                    <button
                      className="button ghost"
                      disabled={busy}
                      onClick={() => setCopySource(null)}
                    >
                      取消复制
                    </button>
                  </div>
                </section>
              )}
              {canEdit && (
                <div className="storage-actions">
                  <label>
                    对象键
                    <input
                      aria-label="对象键"
                      value={key}
                      onChange={(e) => {
                        setKey(e.target.value);
                        setUploadCapability(null);
                      }}
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
                        setUploadCapability(null);
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
                  <label>
                    对象元数据 JSON
                    <textarea
                      aria-label="对象元数据 JSON"
                      value={metadataJSON}
                      disabled={busy}
                      onChange={(e) => setMetadataJSON(e.target.value)}
                    />
                  </label>
                  <button
                    className="button ghost"
                    disabled={busy || !file || !key || overwrite || file.size > objectLimit}
                    onClick={() => void prepareUpload()}
                  >
                    生成临时上传地址
                  </button>
                  {uploadCapability && (
                    <section className="backend-panel" data-testid="storage-upload-capability">
                      <p>
                        临时地址到期：{new Date(uploadCapability.expires_at).toLocaleString()}
                        。仅创建已选择的文件，不覆盖现有对象。
                      </p>
                      <label>
                        临时上传地址
                        <input
                          aria-label="临时上传地址"
                          type="password"
                          readOnly
                          value={uploadCapability.url}
                        />
                      </label>
                      <button
                        className="button"
                        disabled={busy || !file}
                        onClick={() => void uploadPrepared()}
                      >
                        使用临时地址上传
                      </button>
                      <button
                        className="button ghost"
                        disabled={busy}
                        onClick={() => setUploadCapability(null)}
                      >
                        清除临时地址
                      </button>
                    </section>
                  )}
                </div>
              )}
              {file && file.size > objectLimit && (
                <p role="alert">文件超过 {objectLimit / 1024 / 1024} MiB 上限</p>
              )}
              {canEdit && (
                <div className="storage-actions">
                  <button
                    className="button danger"
                    aria-label="删除所选对象"
                    disabled={busy || selectedKeys.length === 0}
                    onClick={() => void deleteSelected()}
                  >
                    删除所选（{selectedKeys.length}）
                  </button>
                  <span className="subtle-note">
                    版本变动将拒绝整批删除。仅选择当前页面的对象。
                  </span>
                </div>
              )}
              <table>
                <thead>
                  <tr>
                    {canEdit && (
                      <th>
                        <input
                          aria-label="选择当前页全部对象"
                          type="checkbox"
                          disabled={busy || objects.length === 0}
                          checked={objects.length > 0 && selectedKeys.length === objects.length}
                          onChange={(e) =>
                            setSelectedKeys(e.target.checked ? objects.map((item) => item.key) : [])
                          }
                        />
                      </th>
                    )}
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
                      {canEdit && (
                        <td>
                          <input
                            aria-label={`选择对象 ${item.key}`}
                            type="checkbox"
                            disabled={busy}
                            checked={selectedKeys.includes(item.key)}
                            onChange={(e) =>
                              setSelectedKeys(
                                e.target.checked
                                  ? [...selectedKeys, item.key]
                                  : selectedKeys.filter((key) => key !== item.key),
                              )
                            }
                          />
                        </td>
                      )}
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
                        <button
                          className="button ghost"
                          disabled={busy}
                          onClick={() => void inspectMetadata(item)}
                        >
                          元数据
                        </button>
                        {canEdit && (
                          <>
                            <button
                              className="button danger"
                              disabled={busy}
                              onClick={() => void removeObject(item)}
                            >
                              删除
                            </button>
                            <button
                              className="button"
                              disabled={busy}
                              onClick={() => {
                                setCopySource({ ...item, bucket });
                                setCopyBucket(bucket);
                                setCopyKey('');
                              }}
                            >
                              复制
                            </button>
                          </>
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
                        setSelectedKeys([]);
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
