import { useEffect, useRef, useState } from 'react';
import { api, ApiError, type Operation } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Identity = {
  binding_id: string;
  kind: 'role' | 'database';
  expected_name: string;
  observed_name?: string;
  status: string;
  intent_version: string;
};
type Pending = { url: string; key: string; version: string; body: string };
export function CatalogIdentityRestore({
  path,
  items,
  disabled,
  onBusy,
  onComplete,
}: {
  path: string;
  items: Identity[];
  disabled: boolean;
  onBusy: (value: boolean) => void;
  onComplete: () => Promise<void>;
}) {
  const [selected, setSelected] = useState('');
  const [ack, setAck] = useState(false);
  const [reset, setReset] = useState(false);
  const [pending, setPending] = useState<Pending | null>(null);
  const [operation, setOperation] = useState<Operation | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const epoch = useRef(0);
  useEffect(
    () => () => {
      epoch.current++;
    },
    [path],
  );
  const choices = items.filter((v) => v.status === 'renamed');
  const choice = choices.find((v) => v.binding_id === selected);
  const operationPath = path.split('/branches/')[0] + '/operations/';
  async function observe(initial: Operation, current: number) {
    let value = initial;
    const deadline = Date.now() + 490000;
    while (!['succeeded', 'failed', 'cancelled'].includes(value.state)) {
      if (Date.now() > deadline) throw Error('原恢复操作仍在执行，请继续观察同一个操作。');
      await new Promise((resolve) => setTimeout(resolve, 2000));
      if (epoch.current !== current) return;
      value = await api<Operation>(operationPath + value.id);
      if (epoch.current !== current) return;
      setOperation(value);
    }
    if (value.state !== 'succeeded')
      throw Error(`${value.error_message || '原恢复操作未成功'} · ${value.id}`);
    await onComplete();
    if (epoch.current === current) {
      setSelected('');
      setAck(false);
      setReset(false);
    }
  }
  async function run(request?: Pending, retry = false) {
    const current = epoch.current;
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      let value = operation;
      if (request) {
        const response = await api<{ operation: Operation }>(request.url, {
          method: 'POST',
          body: request.body,
          headers: { 'Idempotency-Key': request.key, 'If-Match': `"${request.version}"` },
        });
        if (epoch.current !== current) return;
        value = response.operation;
        setPending(null);
      } else if (retry && value) {
        value = await api<Operation>(operationPath + value.id + '/retry', {
          method: 'POST',
          body: '{}',
        });
      }
      if (!value || epoch.current !== current) return;
      setOperation(value);
      await observe(value, current);
    } catch (e) {
      if (epoch.current === current) {
        if (e instanceof ApiError && e.status < 500 && request) setPending(null);
        setError(e instanceof Error ? e.message : '原身份恢复暂不可用');
      }
    } finally {
      if (epoch.current === current) {
        setBusy(false);
        onBusy(false);
      }
    }
  }
  return (
    <form
      aria-label="恢复原目录身份"
      onSubmit={(e) => {
        e.preventDefault();
        if (!choice || !ack || (choice.kind === 'role' && !reset) || pending) return;
        const request = {
          url: path + '/catalog-identities/' + encodeURIComponent(choice.binding_id) + '/restore',
          key: newRequestKey(),
          version: choice.intent_version,
          body: JSON.stringify({
            acknowledge_restore: true,
            acknowledge_role_credential_reset: choice.kind === 'role',
          }),
        };
        setPending(request);
        setOperation(null);
        void run(request);
      }}
    >
      <h3>恢复原目录身份</h3>
      <p>
        仅恢复原 OID 的名称；同名新对象、名称占用、权限、管理标记或 Owner
        等冲突需要先处理。原数据库数据、ACL、Owner 和管理意图保留。
      </p>
      <label>
        待恢复的原对象
        <select
          aria-label="待恢复的原对象"
          value={selected}
          disabled={disabled || busy || !!pending}
          onChange={(e) => {
            setSelected(e.target.value);
            setAck(false);
            setReset(false);
            setOperation(null);
            setError('');
          }}
        >
          <option value="">选择仅名称变化的原对象</option>
          {choices.map((v) => (
            <option key={v.binding_id} value={v.binding_id}>
              {v.kind === 'role' ? '角色' : '数据库'} · {v.observed_name} → {v.expected_name}
            </option>
          ))}
        </select>
      </label>
      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={ack}
          disabled={disabled || busy || !!pending}
          onChange={(e) => setAck(e.target.checked)}
        />
        我确认把原 OID 恢复到记录的原名称。
      </label>
      {choice?.kind === 'role' && (
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={reset}
            disabled={disabled || busy || !!pending}
            onChange={(e) => setReset(e.target.checked)}
          />
          我确认将角色密码重设为平台原有受管理凭据；当前外部密码未观察。
        </label>
      )}
      <button
        className="button"
        disabled={
          disabled || busy || !!pending || !choice || !ack || (choice.kind === 'role' && !reset)
        }
      >
        恢复原名称
      </button>
      {pending && (
        <button
          type="button"
          className="button"
          disabled={disabled || busy}
          onClick={() => void run(pending)}
        >
          重试原身份恢复提交
        </button>
      )}
      {operation && (
        <div role="status">
          原恢复操作：{operation.state} · {operation.id}
        </div>
      )}
      {operation && operation.state !== 'succeeded' && (
        <button
          type="button"
          className="button"
          disabled={disabled || busy || (operation.state === 'failed' && !operation.retryable)}
          onClick={() => void run(undefined, operation.state === 'failed')}
        >
          {operation.state === 'failed' ? '重试原恢复操作' : '继续观察原恢复操作'}
        </button>
      )}
      {error && <p role="alert">{error}</p>}
    </form>
  );
}
