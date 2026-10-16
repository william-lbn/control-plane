import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Upload = {
  upload_id: string;
  key: string;
  initiator: string;
  state: string;
  part_count: number;
  part_bytes: number;
  created_at: string;
  updated_at: string;
};
type Receipt = {
  intent_id: string;
  request_id: string;
  upload_id: string;
  state: string;
  physical_gc: string;
};

export function MultipartAdministration({ bucketPath }: { bucketPath: string }) {
  const [items, setItems] = useState<Upload[]>([]);
  const [reason, setReason] = useState('');
  const [inspect, setInspect] = useState('');
  const [busy, setBusy] = useState(false);
  const [receipt, setReceipt] = useState<Receipt | null>(null);
  const [notice, setNotice] = useState('');
  const [failure, setFailure] = useState('');
  const live = useRef(true);
  const controller = useRef<AbortController | null>(null);
  const attempts = useRef(new Map<string, { reason: string; key: string }>());
  const path = `${bucketPath}/multipart-administration`;
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
      controller.current?.abort();
    };
  }, []);
  async function run(task: (signal: AbortSignal) => Promise<void>) {
    const active = new AbortController();
    controller.current = active;
    setBusy(true);
    setNotice('');
    setFailure('');
    try {
      await task(active.signal);
    } catch (error) {
      if (live.current) {
        setNotice(
          '请求结果尚未确认。若提交过中止，先查询原上传 ID，再使用相同原因显式重试；已有意图和审计会保留。',
        );
        setFailure(
          error instanceof ApiError
            ? `${error.message}${error.requestId ? ` · 请求 ${error.requestId}` : ''}`
            : '连接中断或响应未确认。',
        );
      }
    } finally {
      if (controller.current === active) controller.current = null;
      if (live.current) setBusy(false);
    }
  }
  async function load(signal: AbortSignal, id = '') {
    const value = await api<{ items: Upload[] }>(
      path + (id ? '?' + new URLSearchParams({ upload_id: id }) : ''),
      { signal },
    );
    if (live.current) setItems(value.items);
  }
  async function abort(item: Upload, signal: AbortSignal) {
    let attempt = attempts.current.get(item.upload_id);
    if (!attempt || attempt.reason !== reason) {
      attempt = { reason, key: newRequestKey() };
      attempts.current.set(item.upload_id, attempt);
    }
    const value = await api<Receipt>(`${path}/${item.upload_id}/abort`, {
      method: 'POST',
      signal,
      headers: { 'Idempotency-Key': attempt.key },
      body: JSON.stringify({ reason: attempt.reason }),
    });
    if (!live.current) return;
    setReceipt(value);
    setNotice('原上传已逻辑中止；已发布对象和物理分片均保留。');
    await load(signal);
  }
  return (
    <section className="backend-panel" data-testid="storage-multipart-administration">
      <h3>分段上传运维</h3>
      <p>
        仅当前项目管理员可查看所有上传发起身份并中止遗留上传。可处理已撤销凭据的上传，释放逻辑活动配额；不接管文件内容，也不删除已发布对象或物理字节。加载目录会按需唤醒
        Compute。
      </p>
      <button
        className="button ghost"
        disabled={busy}
        onClick={() =>
          void run(async (signal) => {
            await load(signal);
            setNotice('已加载本桶全部活动上传。');
          })
        }
      >
        加载全部活动上传
      </button>
      <label>
        查询原上传 ID
        <input
          aria-label="查询原上传 ID"
          value={inspect}
          disabled={busy}
          onChange={(e) => setInspect(e.target.value)}
        />
      </label>
      <button
        className="button ghost"
        disabled={busy || !/^mpu_[a-f0-9]{24}$/.test(inspect)}
        onClick={() => void run((signal) => load(signal, inspect))}
      >
        查询原上传状态
      </button>
      <label>
        中止原因
        <textarea
          aria-label="中止原因"
          value={reason}
          disabled={busy}
          maxLength={1024}
          onChange={(e) => setReason(e.target.value)}
          placeholder="说明凭据失效、上传被遗弃等原因；不要填写密钥。"
        />
      </label>
      <p>8–512 个字符，不能含控制符或首尾空白；当前 {Array.from(reason).length} 个字符。</p>
      <table>
        <thead>
          <tr>
            <th>对象 / 上传</th>
            <th>发起身份</th>
            <th>状态 / 已确认分片</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={item.upload_id} data-testid={`multipart-admin-row-${item.upload_id}`}>
              <td>
                {item.key}
                <br />
                <small>{item.upload_id}</small>
              </td>
              <td>{item.initiator}</td>
              <td>
                {item.state} · {item.part_count} 片 / {item.part_bytes} 字节
              </td>
              <td>
                <button
                  className="button danger small"
                  disabled={
                    busy ||
                    (item.state !== 'uploading' &&
                      !(item.state === 'aborted' && attempts.current.has(item.upload_id))) ||
                    Array.from(reason).length < 8 ||
                    Array.from(reason).length > 512 ||
                    reason.trim() !== reason
                  }
                  onClick={() => {
                    if (
                      !window.confirm(
                        `逻辑中止 ${item.upload_id}（${item.key}）？将持久记录原因；已有对象和物理字节保留。`,
                      )
                    )
                      return;
                    void run((signal) => abort(item, signal));
                  }}
                >
                  {item.state === 'aborted' ? '确认原中止回执' : '中止遗留上传'}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {busy && <p role="status">正在处理原上传…</p>}
      {notice && <p role="status">{notice}</p>}
      {failure && (
        <p role="alert" data-testid="multipart-admin-error">
          {failure}
        </p>
      )}
      {receipt && (
        <p data-testid="multipart-admin-receipt">
          {receipt.intent_id} · {receipt.upload_id} · {receipt.state} · 请求 {receipt.request_id}
        </p>
      )}
    </section>
  );
}
