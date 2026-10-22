import { useEffect, useRef, useState } from 'react';
import { storageBlobDigest } from './fileDigest';
import { api } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Upload = {
  upload_id: string;
  key: string;
  expected_sha256: string;
  expected_size: number;
  state: 'uploading' | 'completed' | 'aborted';
  sha256: string;
  size: number;
};
type Part = { part_number: number; etag: string; sha256: string; size: number };
const partBytes = 5 * 1024 * 1024;

export function MultipartUpload({
  bucketPath,
  objectBytes = 8 * 1024 * 1024,
  canEdit,
  showError,
  onPublished,
}: {
  bucketPath: string;
  objectBytes?: number;
  canEdit: boolean;
  showError: (error: unknown) => void;
  onPublished: () => Promise<void>;
}) {
  const [uploads, setUploads] = useState<Upload[]>([]);
  const [selected, setSelected] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [key, setKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const [progress, setProgress] = useState(0);
  const active = useRef<AbortController | null>(null);
  const createAttempt = useRef<{ fingerprint: string; key: string } | null>(null);
  const alive = useRef(true);
  const path = `${bucketPath}/multipart-uploads`;
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      active.current?.abort();
    };
  }, []);
  async function load(signal?: AbortSignal) {
    const value = await api<{ items: Upload[] }>(path, { signal });
    if (alive.current) setUploads(value.items);
    return value.items;
  }
  async function run(task: (signal: AbortSignal) => Promise<void>) {
    const controller = new AbortController();
    active.current = controller;
    setBusy(true);
    setNotice('');
    try {
      await task(controller.signal);
    } catch (error) {
      if (alive.current) {
        if (controller.signal.aborted)
          setNotice('已暂停。请求可能已经提交；加载原上传后再继续，不能假定分片未写入。');
        else showError(error);
      }
    } finally {
      if (active.current === controller) active.current = null;
      if (alive.current) setBusy(false);
    }
  }
  async function upload(resume: boolean, signal: AbortSignal) {
    if (!file || file.size > objectBytes || (!resume && !key)) return;
    const digest = (await storageBlobDigest(file, signal)).hex;
    let id = selected;
    if (!resume) {
      const input = {
        key,
        sha256: digest,
        size: file.size,
        content_type: file.type || 'application/octet-stream',
        metadata: {},
      };
      const fingerprint = JSON.stringify([path, input]);
      if (createAttempt.current?.fingerprint !== fingerprint)
        createAttempt.current = { fingerprint, key: newRequestKey() };
      const created = await api<Upload>(path, {
        method: 'POST',
        signal,
        headers: { 'Idempotency-Key': createAttempt.current.key },
        body: JSON.stringify(input),
      });
      id = created.upload_id;
      if (alive.current) setSelected(id);
    }
    const detail = await api<{ upload: Upload; parts: Part[] }>(`${path}/${id}`, { signal });
    if (detail.upload.expected_sha256 !== digest || detail.upload.expected_size !== file.size)
      throw new Error('请选择创建此上传时的同一个文件；大小和完整 SHA-256 必须相符。');
    if (detail.upload.state === 'completed') {
      setNotice('原上传已完成，未重复发布。');
      await load(signal);
      await onPublished();
      return;
    }
    if (detail.upload.state !== 'uploading') throw new Error('上传已经中止，请显式创建新的上传。');
    const manifest: { part_number: number; etag: string }[] = [];
    const count = Math.max(1, Math.ceil(file.size / partBytes));
    setProgress(0);
    for (let i = 0; i < count; i++) {
      signal.throwIfAborted();
      const chunk = file.slice(i * partBytes, (i + 1) * partBytes);
      const expected = (await storageBlobDigest(chunk, signal)).hex;
      let part = detail.parts.find((item) => item.part_number === i + 1);
      if (!part || part.sha256 !== expected || part.size !== chunk.size) {
        part = await api<Part>(`${path}/${id}/parts/${i + 1}`, {
          method: 'PUT',
          signal,
          headers: { 'Content-Type': 'application/octet-stream' },
          body: chunk,
        });
      }
      if (part.sha256 !== expected || part.size !== chunk.size)
        throw new Error('分片校验不一致；上传未发布。');
      manifest.push({ part_number: part.part_number, etag: part.etag });
      if (alive.current) setProgress(i + 1);
    }
    const completed = await api<Upload>(`${path}/${id}/complete`, {
      method: 'POST',
      signal,
      headers: { 'If-None-Match': '*' },
      body: JSON.stringify({ parts: manifest }),
    });
    if (completed.sha256 !== digest || completed.size !== file.size)
      throw new Error('完成回执与原文件不一致，请检查原上传。');
    if (!alive.current) return;
    setNotice('分段上传已原子发布，完整 SHA-256 校验通过。');
    await load(signal);
    await onPublished();
  }
  return (
    <section className="backend-panel" data-testid="storage-multipart-panel">
      <h3>分段上传与续传</h3>
      <p>
        当前文件上限 {objectBytes / 1024 / 1024} MiB，控制台每片 5
        MiB，串行发送。完成前对象不可见；续传须重新选择原文件。这里只创建新对象，已有同名对象会保留。
      </p>
      <button
        className="button ghost"
        disabled={busy || !canEdit}
        onClick={() =>
          void run(async (signal) => {
            await load(signal);
            setNotice('已加载本账号未完成的上传。');
          })
        }
      >
        加载我的分段上传
      </button>
      <label>
        未完成的上传
        <select
          aria-label="未完成的上传"
          value={selected}
          disabled={busy || !canEdit}
          onChange={(e) => setSelected(e.target.value)}
        >
          <option value="">选择上传</option>
          {uploads.map((item) => (
            <option value={item.upload_id} key={item.upload_id}>
              {item.key} · {item.upload_id}
            </option>
          ))}
          {selected && !uploads.some((item) => item.upload_id === selected) && (
            <option value={selected}>{selected} · 查询原回执</option>
          )}
        </select>
      </label>
      <label>
        分段对象名称
        <input
          aria-label="分段对象名称"
          value={key}
          disabled={busy || !canEdit}
          onChange={(e) => setKey(e.target.value)}
        />
      </label>
      <label>
        分段上传文件
        <input
          aria-label="分段上传文件"
          type="file"
          disabled={busy || !canEdit}
          onChange={(e) => {
            const value = e.target.files?.[0] ?? null;
            setFile(value);
            if (value && !key) setKey(value.name);
          }}
        />
      </label>
      {file && file.size > objectBytes && (
        <p role="alert">当前文件超过 {objectBytes / 1024 / 1024} MiB 上限。</p>
      )}
      <div className="storage-actions">
        <button
          className="button"
          disabled={busy || !canEdit || !file || file.size > objectBytes || !key}
          onClick={() => void run((signal) => upload(false, signal))}
        >
          开始分段上传
        </button>
        <button
          className="button"
          disabled={busy || !canEdit || !file || file.size > objectBytes || !selected}
          onClick={() => void run((signal) => upload(true, signal))}
        >
          继续所选上传
        </button>
        <button className="button ghost" disabled={!busy} onClick={() => active.current?.abort()}>
          暂停分段上传
        </button>
        <button
          className="button danger"
          disabled={busy || !canEdit || !selected}
          onClick={() => {
            if (!window.confirm('中止所选上传？未发布对象，已写入的物理分片仍保留等待独立 GC。'))
              return;
            void run(async (signal) => {
              await api(`${path}/${selected}`, { method: 'DELETE', signal });
              setSelected('');
              createAttempt.current = null;
              await load(signal);
              setNotice('上传已中止，未发布对象；物理分片仍保留。');
            });
          }}
        >
          中止所选上传
        </button>
      </div>
      {busy && <p role="status">已确认 {progress} 个分片；完成回执返回后才确认发布。</p>}
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}
