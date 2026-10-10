import { useEffect, useRef, useState } from 'react';
import { api, endpointPath, projectPath } from '../../api';
import type { Endpoint, Operation } from '../../api';
import { followOperation } from '../../shared/followOperation';
import { newRequestKey } from '../../shared/requestKey';
import { status } from '../../shared/ui';

export function DeleteEndpoint({
  projectId,
  endpoint,
  onDeleted,
  onClose,
}: {
  projectId: string;
  endpoint: Endpoint;
  onDeleted: () => Promise<void>;
  onClose: () => void;
}) {
  // Capture the reviewed target/version. Background observation must not change
  // the identity, confirmation or If-Match of a pending destructive request.
  const [target] = useState(endpoint);
  const [confirmation, setConfirmation] = useState('');
  const [operation, setOperation] = useState<Operation | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [observing, setObserving] = useState(false);
  const requestKey = useRef('');
  const controller = useRef<AbortController | null>(null);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      controller.current?.abort();
    };
  }, []);
  async function observe(op: Operation) {
    controller.current?.abort();
    const abort = new AbortController();
    controller.current = abort;
    setObserving(true);
    const final = await followOperation<Operation>({
      id: op.id,
      signal: abort.signal,
      read: (signal) => api(`${projectPath(projectId)}/operations/${op.id}`, { signal }),
      onValue: (value) => {
        if (mounted.current) setOperation(value);
      },
      onUnavailable: () => {
        if (mounted.current) setError('操作读取暂不可用，正在继续观察；删除请求没有重发。');
      },
    });
    if (!mounted.current) return;
    setObserving(false);
    if (final?.state === 'succeeded') {
      await onDeleted();
      onClose();
    } else if (final) setError(`${final.error_code}: ${final.error_message}`);
    else setError('观察超时。可以继续观察这个操作，或在操作记录中查看。');
  }
  async function submit() {
    if (busy || operation || confirmation !== target.selector) return;
    setBusy(true);
    setError('');
    try {
      if (!requestKey.current) requestKey.current = newRequestKey();
      const reply = await api<{ operation: Operation }>(endpointPath(projectId, target.id), {
        method: 'DELETE',
        headers: {
          'If-Match': `"${target.version}"`,
          'Idempotency-Key': requestKey.current,
        },
        body: JSON.stringify({ confirm_selector: confirmation }),
      });
      if (!mounted.current) return;
      setOperation(reply.operation);
      await observe(reply.operation);
    } catch (e) {
      if (mounted.current) setError(String(e));
    } finally {
      if (mounted.current) {
        setBusy(false);
        setObserving(false);
      }
    }
  }
  async function continueOperation(retry: boolean) {
    if (!operation || busy) return;
    setBusy(true);
    setError('');
    try {
      if (retry)
        await api(`${projectPath(projectId)}/operations/${operation.id}/retry`, {
          method: 'POST',
        });
      await observe(operation);
    } catch (e) {
      if (mounted.current) setError(String(e));
    } finally {
      if (mounted.current) {
        setBusy(false);
        setObserving(false);
      }
    }
  }
  return (
    <div className="modal-backdrop">
      <section
        className="modal endpoint-delete-modal"
        role="dialog"
        aria-modal="true"
        aria-label="删除 Compute Endpoint"
      >
        <div className="panel-heading">
          <h2>删除 Compute</h2>
          {operation && status(operation.state)}
        </div>
        <p>
          将关闭 <code>{target.selector}</code> 的连接入口并回收计算资源。分支、数据库、角色和
          文件数据保留；其他 Compute 独立运行。重新添加 Compute 会获得新的连接地址。
        </p>
        <p className="muted">
          请先停止使用此地址的应用。若 Data API、Auth 或 Object Storage 仍依赖此 Compute，
          需要先在对应页面停用服务。此操作不会自动迁移应用连接。
        </p>
        <label>
          输入完整 Endpoint Selector 确认
          <input
            aria-label="Endpoint Selector 确认"
            autoComplete="off"
            value={confirmation}
            disabled={busy || !!operation}
            onChange={(e) => setConfirmation(e.target.value)}
          />
        </label>
        {error && (
          <p className="inline-error" role="alert">
            {error}
          </p>
        )}
        {operation && (
          <div aria-live="polite">
            <small>{operation.id}</small>
            {operation.steps.map((step) => (
              <div className="list-row" key={step.ordinal}>
                <span>{step.name}</span>
                {status(step.state)}
              </div>
            ))}
          </div>
        )}
        <div className="form-actions">
          <button className="button" disabled={busy} onClick={onClose}>
            {operation ? '关闭窗口' : '取消'}
          </button>
          {operation?.state === 'failed' && operation.retryable && (
            <button className="button" disabled={busy} onClick={() => void continueOperation(true)}>
              重试既有操作
            </button>
          )}
          {operation && !['succeeded', 'failed', 'cancelled'].includes(operation.state) && (
            <button
              className="button"
              disabled={busy}
              onClick={() => void continueOperation(false)}
            >
              继续观察
            </button>
          )}
          <button
            className="button danger"
            disabled={busy || !!operation || confirmation !== target.selector}
            onClick={() => void submit()}
          >
            {observing ? '正在回收计算…' : '确认删除 Compute'}
          </button>
        </div>
      </section>
    </div>
  );
}
