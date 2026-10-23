import { inputKind } from './inputFiles';
import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

export type AIInputFile = {
  id: string;
  filename: string;
  content_type: string;
  bytes: number;
  sha256: string;
  expires_at: number;
};
type Pending = { path: string; method: string; key: string; file?: File; ttl: number };
export function AIInputFiles({
  prefix,
  selected,
  onSelection,
  busy,
  types,
}: {
  prefix: string;
  selected: AIInputFile[];
  onSelection: (value: AIInputFile[]) => void;
  busy: boolean;
  types: string[];
}) {
  const [items, setItems] = useState<AIInputFile[]>([]),
    [file, setFile] = useState<File | null>(null),
    [ttl, setTTL] = useState('86400'),
    [working, setWorking] = useState(false),
    [uncertain, setUncertain] = useState(false),
    [error, setError] = useState('');
  const pending = useRef<Pending | null>(null),
    fileInput = useRef<HTMLInputElement | null>(null),
    active = useRef(prefix),
    selection = useRef(selected);
  active.current = prefix;
  selection.current = selected;
  useEffect(() => {
    active.current = prefix;
    let current = true;
    api<{ data: AIInputFile[] }>(prefix + '/files')
      .then((v) => {
        if (current) setItems(v.data);
      })
      .catch((e) => {
        if (current) setError(String(e));
      });
    return () => {
      current = false;
      pending.current = null;
      active.current = '';
    };
  }, [prefix]);
  async function refresh() {
    const value = await api<{ data: AIInputFile[] }>(prefix + '/files');
    if (active.current !== prefix) return;
    setItems(value.data);
    onSelection(selection.current.filter((v) => value.data.some((item) => item.id === v.id)));
  }
  async function send(request: Pending) {
    setWorking(true);
    setError('');
    try {
      const form = request.file ? new FormData() : undefined;
      if (form && request.file) {
        form.append('purpose', 'user_data');
        form.append('file', request.file);
      }
      await api(request.path, {
        method: request.method,
        body: form,
        headers: { 'Idempotency-Key': request.key, 'X-Neon-File-TTL': String(request.ttl) },
      });
      if (active.current !== prefix) return;
      pending.current = null;
      setUncertain(false);
      setFile(null);
      if (fileInput.current) fileInput.current.value = '';
      await refresh();
    } catch (e) {
      if (active.current !== prefix) return;
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
      if (active.current === prefix) setWorking(false);
    }
  }
  function upload() {
    if (!file || working || uncertain || busy) return;
    if (file.size < 1 || file.size > 1048576) {
      setError('单个文件须为 1 字节到 1 MiB');
      return;
    }
    const value = Number(ttl);
    if (!Number.isInteger(value) || value < 60 || value > 604800) {
      setError('文件保留时间为 60 秒到七天');
      return;
    }
    const request = {
      path: prefix + '/files',
      method: 'POST',
      key: newRequestKey(),
      file,
      ttl: value,
    };
    pending.current = request;
    void send(request);
  }
  function remove(id: string) {
    if (working || uncertain || busy) return;
    const request = {
      path: prefix + '/files/' + id,
      method: 'DELETE',
      key: newRequestKey(),
      ttl: 86400,
    };
    pending.current = request;
    void send(request);
  }
  function choose(item: AIInputFile, checked: boolean) {
    const next = checked ? [...selected, item] : selected.filter((v) => v.id !== item.id);
    if (next.length > 8 || next.reduce((sum, v) => sum + v.bytes, 0) > 1048576) {
      setError('每次请求最多选择八个文件，文件总量不超过 1 MiB');
      return;
    }
    setError('');
    onSelection(next);
  }
  return (
    <section className="panel" aria-label="AI 输入文件">
      <div className="panel-heading">
        <h2>AI 输入文件</h2>
        <button
          disabled={working || uncertain || busy}
          onClick={() => refresh().catch((e) => setError(String(e)))}
        >
          刷新输入文件
        </button>
      </div>
      <p>
        文件仅供当前分支使用并加密保存。勾选后才会随下一次调用发送到所选模型供应商；每个文件最多 1
        MiB。删除和过期阻止新调用，已经接纳的调用可能继续。
      </p>
      {error && <div role="alert">{error}</div>}
      {uncertain && (
        <div role="status">
          提交结果尚未确认。
          <button
            disabled={working || busy}
            onClick={() => {
              if (pending.current) void send(pending.current);
            }}
          >
            重试原输入文件提交
          </button>
        </div>
      )}
      <fieldset disabled={working || uncertain || busy}>
        <label>
          选择 AI 输入文件
          <input
            ref={fileInput}
            aria-label="选择 AI 输入文件"
            type="file"
            accept="image/png,image/jpeg,image/gif,image/webp,application/pdf,audio/wav,audio/mpeg,video/mp4"
            onChange={(e) => setFile(e.target.files?.[0] || null)}
          />
        </label>
        <label>
          输入文件保留秒数
          <input
            aria-label="输入文件保留秒数"
            type="number"
            min={60}
            max={604800}
            value={ttl}
            onChange={(e) => setTTL(e.target.value)}
          />
        </label>
        <button disabled={!file} onClick={upload}>
          上传 AI 输入文件
        </button>
      </fieldset>
      <table>
        <thead>
          <tr>
            <th>本次调用</th>
            <th>文件</th>
            <th>类型 / 字节</th>
            <th>到期时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={item.id}>
              <td>
                <input
                  aria-label={'附加文件 ' + item.filename}
                  type="checkbox"
                  disabled={
                    working || uncertain || busy || !types.includes(inputKind(item.content_type))
                  }
                  checked={selected.some((v) => v.id === item.id)}
                  onChange={(e) => choose(item, e.target.checked)}
                />
              </td>
              <td>
                {item.filename}
                <br />
                <code>{item.id}</code>
                <br />
                <small>SHA-256 {item.sha256}</small>
              </td>
              <td>
                {item.content_type} · {item.bytes}
              </td>
              <td>{new Date(item.expires_at * 1000).toLocaleString()}</td>
              <td>
                <a href={prefix + '/files/' + item.id + '/content'}>下载输入文件</a>
                <button
                  aria-label={'删除输入文件 ' + item.filename}
                  disabled={working || uncertain || busy}
                  onClick={() => remove(item.id)}
                >
                  删除
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
