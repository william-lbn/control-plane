import { useEffect, useRef, useState, type FormEvent } from 'react';
import { api, ApiError, projectPath, type Operation } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Choice = {
  name: string;
  managed: boolean;
  protected: boolean;
  state: string;
  version_token?: string;
};
type Intent = {
  url: string;
  body: string;
  key: string;
  version: string;
  source: string;
  target: string;
};

// Keep the exact input/key after an unknown response. Once an Operation ID is
// known, recovery observes that ID rather than creating a second rename.
export function CatalogRename({
  path,
  projectId,
  roles,
  databases,
  canAdmin,
  disabled,
  onBusy,
  onComplete,
}: {
  path: string;
  projectId: string;
  roles: Choice[];
  databases: Choice[];
  canAdmin: boolean;
  disabled: boolean;
  onBusy: (value: boolean) => void;
  onComplete: () => Promise<void>;
}) {
  const [kind, setKind] = useState<'roles' | 'databases'>('databases'),
    [source, setSource] = useState(''),
    [target, setTarget] = useState(''),
    [ack, setAck] = useState(false),
    [migrateBackend, setMigrateBackend] = useState(false);
  const [pending, setPending] = useState<Intent | null>(null),
    [operation, setOperation] = useState<Operation | null>(null),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const epoch = useRef(0);
  useEffect(
    () => () => {
      epoch.current++;
    },
    [path],
  );
  const choices = (kind === 'roles' ? roles : databases).filter(
      (row) => row.managed && !row.protected && row.state === 'ready',
    ),
    selected = choices.find((row) => row.name === source),
    bytes = new TextEncoder().encode(target).length;
  const observe = async (original: Operation, currentEpoch: number) => {
    let value = original;
    const deadline = Date.now() + 490000;
    while (!['succeeded', 'failed', 'cancelled'].includes(value.state)) {
      if (Date.now() > deadline) throw Error('操作仍在执行，可继续观察同一个操作。');
      await new Promise((resolve) => setTimeout(resolve, 2000));
      if (epoch.current !== currentEpoch) return;
      value = await api<Operation>(projectPath(projectId) + '/operations/' + original.id);
      if (epoch.current !== currentEpoch) return;
      setOperation(value);
    }
    if (value.state !== 'succeeded')
      throw Error(`${value.error_message || '操作未成功，请查看原操作记录'} · ${value.id}`);
    await onComplete();
  };
  const send = async (intent: Intent) => {
    const currentEpoch = epoch.current;
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      const result = await api<{ operation: Operation }>(intent.url, {
        method: 'POST',
        headers: { 'Idempotency-Key': intent.key, 'If-Match': `"${intent.version}"` },
        body: intent.body,
      });
      if (epoch.current !== currentEpoch) return;
      setPending(null);
      setOperation(result.operation);
      await observe(result.operation, currentEpoch);
      if (epoch.current !== currentEpoch) return;
      setSource((value) => (value === intent.source ? '' : value));
      setTarget((value) => (value === intent.target ? '' : value));
      setAck(false);
    } catch (value) {
      if (epoch.current === currentEpoch) {
        if (value instanceof ApiError && value.status < 500) setPending(null);
        setError(value instanceof Error ? value.message : '重命名暂不可用');
      }
    } finally {
      if (epoch.current === currentEpoch) {
        setBusy(false);
        onBusy(false);
      }
    }
  };
  const continueObservation = async () => {
    if (!operation) return;
    const currentEpoch = epoch.current;
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      await observe(operation, currentEpoch);
    } catch (value) {
      if (epoch.current === currentEpoch)
        setError(value instanceof Error ? value.message : '原操作观察暂不可用');
    } finally {
      if (epoch.current === currentEpoch) {
        setBusy(false);
        onBusy(false);
      }
    }
  };
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!selected?.version_token || !ack || !target || target === source || bytes > 63 || pending)
      return;
    const intent: Intent = {
      url: path + '/' + kind + '/' + encodeURIComponent(source) + '/rename',
      body: JSON.stringify({
        new_name: target,
        ...(kind === 'databases' && canAdmin && migrateBackend
          ? { migrate_backend_dependencies: true }
          : {}),
      }),
      key: newRequestKey(),
      version: selected.version_token,
      source,
      target,
    };
    setPending(intent);
    setOperation(null);
    void send(intent);
  };
  return (
    <section className="panel" aria-label="目录重命名">
      <div className="panel-heading">
        <h2>重命名数据库或角色</h2>
        <span>保留数据与权限</span>
      </div>
      <p className="subtle-note">
        仅支持平台管理的非系统对象。数据库保留原数据，角色保留受管理密码及权限。使用旧名称的应用需要更新连接配置；仍被默认连接或
        Backend 服务引用的对象需要 Admin 显式选择迁移。迁移前须停用相关服务并挂起
        Functions，原数据、密码和签名密钥保留。
      </p>
      <form onSubmit={submit}>
        <div className="identity-grid">
          <label>
            重命名对象类型
            <select
              aria-label="重命名对象类型"
              value={kind}
              disabled={disabled || busy || !!pending}
              onChange={(event) => {
                setKind(event.target.value as 'roles' | 'databases');
                setSource('');
                setTarget('');
                setAck(false);
                setError('');
              }}
            >
              <option value="databases">数据库</option>
              <option value="roles">角色</option>
            </select>
          </label>
          <label>
            重命名原对象
            <select
              aria-label="重命名原对象"
              value={source}
              disabled={disabled || busy || !!pending}
              onChange={(event) => {
                setSource(event.target.value);
                setAck(false);
              }}
            >
              <option value="">选择受管理对象</option>
              {choices.map((row) => (
                <option key={row.name} value={row.name}>
                  {row.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            重命名新名称
            <input
              aria-label="重命名新名称"
              required
              maxLength={63}
              value={target}
              disabled={disabled || busy || !!pending}
              onChange={(event) => setTarget(event.target.value)}
            />
            <small>{bytes} / 63 UTF-8 字节</small>
          </label>
        </div>
        {source && target && (
          <p className="subtle-note">
            <code>{source}</code> → <code>{target}</code>
          </p>
        )}
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={ack}
            disabled={disabled || busy || !!pending}
            onChange={(event) => setAck(event.target.checked)}
          />
          确认更新应用连接名称
        </label>
        {canAdmin && kind === 'databases' && (
          <label className="checkbox-label">
            <input
              type="checkbox"
              checked={migrateBackend}
              disabled={disabled || busy || !!pending}
              onChange={(event) => setMigrateBackend(event.target.checked)}
            />
            同步迁移已停用 Backend 服务的数据库名称
          </label>
        )}
        <button
          className="button primary"
          disabled={
            disabled ||
            busy ||
            !!pending ||
            !selected?.version_token ||
            !ack ||
            !target ||
            target === source ||
            bytes > 63
          }
        >
          提交重命名
        </button>
      </form>
      {pending && !busy && (
        <div className="subtle-note" role="status">
          请求结果尚未确定。保留原名称、新名称和请求标识，重试会取回同一意图。
          <button className="button" disabled={disabled} onClick={() => void send(pending)}>
            重试原重命名请求
          </button>
        </div>
      )}
      {operation && (
        <div className="catalog-operation" role="status">
          <strong>重命名操作：{operation.state}</strong>
          <small>{operation.id}</small>
          <div>
            {operation.steps.map((step) => (
              <span className="tag" key={step.ordinal}>
                {step.name} · {step.state}
              </span>
            ))}
          </div>
          {!busy && !['succeeded', 'failed', 'cancelled'].includes(operation.state) && (
            <button
              className="button"
              disabled={disabled}
              onClick={() => void continueObservation()}
            >
              继续观察原重命名操作
            </button>
          )}
        </div>
      )}
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
