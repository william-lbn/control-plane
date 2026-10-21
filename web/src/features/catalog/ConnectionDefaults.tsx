import { useEffect, useRef, useState, type FormEvent } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Defaults = {
  mode: 'platform' | 'managed';
  role: string;
  database: string;
  version_token: string;
  platform_role?: string;
  platform_database?: string;
};
type Choice = { name: string; managed: boolean; protected: boolean; state: string };
type Intent = { key: string; version: string; body: string };

export function ConnectionDefaults({
  path,
  roles,
  databases,
  canAdmin,
  disabled,
  onBusy,
}: {
  path: string;
  roles: Choice[];
  databases: Choice[];
  canAdmin: boolean;
  disabled: boolean;
  onBusy: (value: boolean) => void;
}) {
  const [current, setCurrent] = useState<Defaults | null>(null);
  const [mode, setMode] = useState<'platform' | 'managed'>('managed');
  const [role, setRole] = useState(''),
    [database, setDatabase] = useState('');
  const [ack, setAck] = useState(false),
    [error, setError] = useState(''),
    [saved, setSaved] = useState(false);
  const [pending, setPending] = useState<Intent | null>(null);
  const epoch = useRef(0);
  const url = path + '/connection-defaults';
  async function refresh() {
    const version = epoch.current;
    try {
      const value = await api<Defaults>(url);
      if (version === epoch.current) {
        setCurrent(value);
        setError('');
      }
    } catch (e) {
      if (version === epoch.current)
        setError(e instanceof Error ? e.message : '连接默认值暂不可用');
    }
  }
  useEffect(() => {
    void refresh();
    return () => {
      epoch.current++;
    };
  }, [url]);
  async function send(intent: Intent) {
    const version = epoch.current;
    onBusy(true);
    setError('');
    setSaved(false);
    try {
      const value = await api<Defaults>(url, {
        method: 'PATCH',
        headers: { 'Idempotency-Key': intent.key, 'If-Match': `"${intent.version}"` },
        body: intent.body,
      });
      if (version !== epoch.current) return;
      setCurrent(value);
      setPending(null);
      setAck(false);
      setSaved(true);
    } catch (e) {
      if (version !== epoch.current) return;
      if (e instanceof ApiError && e.status < 500) setPending(null);
      setError(e instanceof Error ? e.message : '提交结果未知，请重试原提交');
    } finally {
      if (version === epoch.current) onBusy(false);
    }
  }
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!current || !canAdmin || !ack || pending || disabled) return;
    const intent = {
      key: newRequestKey(),
      version: current.version_token,
      body: JSON.stringify(mode === 'platform' ? { mode } : { mode, role, database }),
    };
    setPending(intent);
    void send(intent);
  }
  const locked = disabled || !!pending || !canAdmin;
  return (
    <form className="panel" aria-label="客户连接默认值" onSubmit={submit}>
      <div className="panel-heading">
        <h2>客户连接默认值</h2>
        <span>分支内全部 Writer / Reader</span>
      </div>
      <p className="subtle-note">
        连接串与 SQL 编辑器使用所选角色和数据库。使用该角色的现有密码；平台管理身份、已有连接及
        Backend 服务账号保持原配置。
      </p>
      {current && (
        <p>
          当前：
          <strong data-testid="connection-default-current">
            {current.role} / {current.database}
          </strong>{' '}
          · 版本 {current.version_token}
        </p>
      )}
      {!canAdmin && <p>需要项目 Admin 权限修改。</p>}
      <label>
        连接默认值模式
        <select
          aria-label="连接默认值模式"
          value={mode}
          disabled={locked}
          onChange={(e) => {
            setMode(e.target.value as typeof mode);
            setAck(false);
            setSaved(false);
          }}
        >
          <option value="managed">选择受管理身份</option>
          <option value="platform">恢复平台初始默认值</option>
        </select>
      </label>
      {mode === 'managed' && (
        <>
          <label>
            默认连接角色
            <select
              aria-label="默认连接角色"
              required
              value={role}
              disabled={locked}
              onChange={(e) => {
                setRole(e.target.value);
                setAck(false);
                setSaved(false);
              }}
            >
              <option value="">选择角色</option>
              {roles
                .filter((r) => r.managed && !r.protected && r.state === 'ready')
                .map((r) => (
                  <option key={r.name} value={r.name}>
                    {r.name}
                  </option>
                ))}
            </select>
          </label>
          <label>
            默认连接数据库
            <select
              aria-label="默认连接数据库"
              required
              value={database}
              disabled={locked}
              onChange={(e) => {
                setDatabase(e.target.value);
                setAck(false);
                setSaved(false);
              }}
            >
              <option value="">选择数据库</option>
              {databases
                .filter(
                  (d) =>
                    (!d.protected ||
                      d.name === current?.platform_database ||
                      (current?.mode === 'platform' && d.name === current.database)) &&
                    d.state === 'ready',
                )
                .map((d) => (
                  <option key={d.name} value={d.name}>
                    {d.name}
                  </option>
                ))}
            </select>
          </label>
          <p className="subtle-note">
            支持受管理数据库或原平台默认数据库。数据库属主、表、Schema 和权限不会随此选择迁移。
          </p>
        </>
      )}
      <label className="checkbox-row">
        <input
          type="checkbox"
          checked={ack}
          disabled={locked}
          onChange={(e) => setAck(e.target.checked)}
        />
        确认后续连接使用所选默认值和对应角色密码
      </label>
      <div className="button-row">
        <button
          className="button primary"
          disabled={locked || !current || !ack || (mode === 'managed' && (!role || !database))}
        >
          保存连接默认值
        </button>
        <button
          className="button"
          type="button"
          disabled={disabled || !!pending}
          onClick={() => void refresh()}
        >
          刷新连接默认值
        </button>
        {pending && (
          <button
            className="button"
            type="button"
            disabled={disabled}
            onClick={() => void send(pending)}
          >
            重试原连接默认值提交
          </button>
        )}
      </div>
      {pending && <p role="status">原提交尚未取得确定回执；重试保留同一请求内容、版本和请求键。</p>}
      {saved && <p role="status">连接默认值已保存。</p>}
      {error && <p role="alert">{error}</p>}
    </form>
  );
}
