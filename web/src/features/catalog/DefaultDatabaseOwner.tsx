import { useEffect, useRef, useState, type FormEvent } from 'react';
import { api, ApiError, type Operation } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Preview = {
  database: string;
  database_oid: number;
  observed_owner: string;
  observed_owner_oid: number;
  platform_role: string;
  platform_role_oid: number;
  mode: string;
  state: string;
  status: string;
  version_token: string;
  operation_id?: string;
  owner?: string;
  managed_owners: { name: string; oid: number; version_token: string }[];
};
type Pending = { key: string; version: string; body: string };

export function DefaultDatabaseOwner({
  path,
  canAdmin,
  disabled,
  onBusy,
  onReserved,
  onComplete,
}: {
  path: string;
  canAdmin: boolean;
  disabled: boolean;
  onBusy: (value: boolean) => void;
  onReserved: (value: boolean) => void;
  onComplete: () => Promise<void>;
}) {
  const [current, setCurrent] = useState<Preview | null>(null);
  const [mode, setMode] = useState<'managed' | 'platform'>('managed');
  const [owner, setOwner] = useState(''),
    [password, setPassword] = useState('');
  const [ack, setAck] = useState(false),
    [initialAck, setInitialAck] = useState(false);
  const [pending, setPending] = useState<Pending | null>(null),
    [operation, setOperation] = useState<Operation | null>(null);
  const [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const epoch = useRef(0),
    url = path + '/default-database-owner';
  const operationPath = path.split('/branches/')[0] + '/operations/';
  useEffect(
    () => () => {
      epoch.current++;
    },
    [path],
  );
  useEffect(() => {
    onReserved(!!pending || current?.state === 'updating');
  }, [pending, current, onReserved]);
  async function refresh(version = epoch.current) {
    const value = await api<Preview>(url);
    if (epoch.current !== version) return;
    setCurrent(value);
    if (value.operation_id) {
      const op = await api<Operation>(operationPath + value.operation_id);
      if (epoch.current === version) setOperation(op);
    }
  }
  async function read() {
    const version = epoch.current;
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      await refresh(version);
    } catch (e) {
      if (epoch.current === version)
        setError(e instanceof Error ? e.message : '默认数据库 Owner 暂不可用');
    } finally {
      if (epoch.current === version) {
        setBusy(false);
        onBusy(false);
      }
    }
  }
  async function observe(start: Operation, version: number) {
    let value = start;
    const deadline = Date.now() + 490000;
    while (!['succeeded', 'failed', 'cancelled'].includes(value.state)) {
      if (Date.now() > deadline) throw Error('原 Owner 操作仍在执行，请继续观察同一个操作。');
      await new Promise((resolve) => setTimeout(resolve, 2000));
      if (epoch.current !== version) return;
      value = await api<Operation>(operationPath + value.id);
      if (epoch.current !== version) return;
      setOperation(value);
    }
    if (value.state !== 'succeeded')
      throw Error(`${value.error_message || '原 Owner 操作未成功'} · ${value.id}`);
    await refresh(version);
    if (epoch.current !== version) return;
    setAck(false);
    setInitialAck(false);
    await onComplete();
  }
  async function run(request?: Pending, retry = false) {
    const version = epoch.current;
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      let value = operation;
      if (request) {
        const response = await api<{ operation: Operation }>(url, {
          method: 'PATCH',
          body: request.body,
          headers: { 'Idempotency-Key': request.key, 'If-Match': `"${request.version}"` },
        });
        if (epoch.current !== version) return;
        value = response.operation;
        setPending(null);
        setPassword('');
        setCurrent((v) => (v ? { ...v, state: 'updating', operation_id: value!.id } : v));
      } else if (retry && value) {
        value = await api<Operation>(operationPath + value.id + '/retry', {
          method: 'POST',
          body: '{}',
        });
      }
      if (!value || epoch.current !== version) return;
      setOperation(value);
      await observe(value, version);
    } catch (e) {
      if (epoch.current === version) {
        if (e instanceof ApiError && e.status < 500 && request) setPending(null);
        setError(e instanceof Error ? e.message : '提交结果未知，请保留并重试原请求');
      }
    } finally {
      if (epoch.current === version) {
        setBusy(false);
        onBusy(false);
      }
    }
  }
  function submit(event: FormEvent) {
    event.preventDefault();
    if (
      !current ||
      !canAdmin ||
      !ack ||
      pending ||
      disabled ||
      busy ||
      current.state === 'updating' ||
      (current.mode === 'uninitialized' && !initialAck)
    )
      return;
    const selected = current.managed_owners.find((v) => v.name === owner);
    if (mode === 'managed' && !selected) return;
    const request = {
      key: newRequestKey(),
      version: current.version_token,
      body: JSON.stringify({
        mode,
        ...(mode === 'managed' ? { owner, owner_oid: selected!.oid } : {}),
        expected_database_oid: current.database_oid,
        expected_previous_owner_oid: current.observed_owner_oid,
        acknowledge_owner_change: true,
        acknowledge_initial_observation: current.mode === 'uninitialized',
        password,
      }),
    };
    setPending(request);
    setOperation(null);
    void run(request);
  }
  const locked = disabled || busy || !!pending || !canAdmin || current?.state === 'updating';
  return (
    <form className="panel" aria-label="默认数据库 Owner" onSubmit={submit}>
      <div className="panel-heading">
        <h2>默认数据库 Owner</h2>
        <span>原数据库 · Writer / Reader</span>
      </div>
      <p className="subtle-note">
        迁移原默认数据库的属主，保留数据库、表和 Schema 的 OID、数据、已有表和 Schema
        属主、注释及连接选项。PostgreSQL 会相应重写数据库 ACL
        中的属主和授权者。客户连接默认值独立设置。
      </p>
      <button
        type="button"
        className="button"
        disabled={disabled || busy || !!pending}
        onClick={() => void read()}
      >
        读取默认数据库 Owner
      </button>
      {current && (
        <>
          <p data-testid="default-owner-current">
            {current.database} · OID {current.database_oid} · 实际 Owner {current.observed_owner} (
            {current.observed_owner_oid}) · {current.status} · 版本 {current.version_token}
          </p>
          {current.mode === 'uninitialized' && (
            <p>尚未记录初始身份，首次确认只证明当前观察，不能证明更早的历史。</p>
          )}
          {!canAdmin && <p>需要项目 Admin 权限修改。</p>}
          <label>
            默认数据库 Owner 模式
            <select
              aria-label="默认数据库 Owner 模式"
              value={mode}
              disabled={locked}
              onChange={(e) => {
                setMode(e.target.value as typeof mode);
                setAck(false);
              }}
            >
              <option value="managed">迁移到受管理角色</option>
              <option value="platform">恢复原平台角色</option>
            </select>
          </label>
          {mode === 'managed' ? (
            <label>
              默认数据库目标 Owner
              <select
                aria-label="默认数据库目标 Owner"
                value={owner}
                required
                disabled={locked}
                onChange={(e) => {
                  setOwner(e.target.value);
                  setAck(false);
                }}
              >
                <option value="">选择已观察的受管理角色</option>
                {current.managed_owners.map((v) => (
                  <option value={v.name} key={v.oid}>
                    {v.name} · OID {v.oid}
                  </option>
                ))}
              </select>
            </label>
          ) : (
            <p>
              目标：{current.platform_role} · OID {current.platform_role_oid}
            </p>
          )}
          <label>
            当前平台角色密码
            <input
              aria-label="当前平台角色密码"
              type="password"
              autoComplete="off"
              required
              value={password}
              disabled={locked}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          <p className="subtle-note">
            使用当前 {current.platform_role}{' '}
            密码授权原操作；已接收操作成功提交后会清除其封存凭据。浏览器仅在当前页面内存保留未确定提交的原请求。
          </p>
          {current.mode === 'uninitialized' && (
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={initialAck}
                disabled={locked}
                onChange={(e) => setInitialAck(e.target.checked)}
              />
              我确认把当前原 OID 观察记录为初始身份，早期历史尚未证明。
            </label>
          )}
          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={ack}
              disabled={locked}
              onChange={(e) => setAck(e.target.checked)}
            />
            我确认迁移原默认数据库 Owner，并保留原数据库和数据。
          </label>
          <button
            className="button primary"
            disabled={
              locked ||
              !ack ||
              !password ||
              (current.mode === 'uninitialized' && !initialAck) ||
              (mode === 'managed' && !owner)
            }
          >
            迁移默认数据库 Owner
          </button>
        </>
      )}
      {pending && (
        <>
          <p role="status">提交结果尚未确定；保留原请求内容、版本和请求键。</p>
          <button
            type="button"
            className="button"
            disabled={disabled || busy}
            onClick={() => void run(pending)}
          >
            重试原 Owner 提交
          </button>
        </>
      )}
      {operation && (
        <p role="status" data-testid="default-owner-operation">
          原 Owner 操作：{operation.state} · {operation.id}
        </p>
      )}
      {operation && operation.state !== 'succeeded' && (
        <button
          type="button"
          className="button"
          disabled={
            disabled ||
            busy ||
            (!canAdmin && operation.state === 'failed') ||
            (operation.state === 'failed' && !operation.retryable)
          }
          onClick={() => void run(undefined, operation.state === 'failed')}
        >
          {operation.state === 'failed' ? '重试原 Owner 操作' : '继续观察原 Owner 操作'}
        </button>
      )}
      {error && <p role="alert">{error}</p>}
    </form>
  );
}
