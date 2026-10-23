import { useState } from 'react';

type User = { id: string; name: string; email: string; role?: string; banned?: boolean };
type Session = { id: string; expiresAt: string; token: string };
class AdminReplyError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

export function AuthAdminPanel({ branch }: { branch: string }) {
  const [users, setUsers] = useState<User[]>([]),
    [target, setTarget] = useState(''),
    [name, setName] = useState(''),
    [email, setEmail] = useState(''),
    [password, setPassword] = useState(''),
    [role, setRole] = useState('user'),
    [reason, setReason] = useState(''),
    [sessions, setSessions] = useState<Session[]>([]),
    [sessionID, setSessionID] = useState(''),
    [remove, setRemove] = useState(false),
    [busy, setBusy] = useState(false),
    [uncertain, setUncertain] = useState(false),
    [notice, setNotice] = useState('');
  const base = '/auth/v1/' + encodeURIComponent(branch) + '/admin/';
  async function call(action: string, data?: object) {
    setBusy(true);
    setNotice('');
    const writes =
      data !== undefined &&
      !['list-user-sessions', 'has-permission'].includes(action.split('?')[0]);
    try {
      const response = await fetch(base + action, {
        method: data === undefined ? 'GET' : 'POST',
        credentials: 'include',
        headers: data === undefined ? {} : { 'Content-Type': 'application/json' },
        body: data === undefined ? undefined : JSON.stringify(data),
      });
      const value = await response.json().catch(() => null);
      if (!response.ok) {
        throw new AdminReplyError(
          value?.message || `应用管理员请求失败（${response.status}）`,
          response.status,
        );
      }
      if (value === null) throw new Error('应用管理员响应无法确认。');
      setNotice('应用管理员操作已完成。');
      return value;
    } catch (error) {
      if (writes && (!(error instanceof AdminReplyError) || error.status >= 500))
        setUncertain(true);
      setNotice(error instanceof Error ? error.message : '请求结果未知');
      return undefined;
    } finally {
      setPassword('');
      setBusy(false);
    }
  }
  async function list() {
    const result = await call('list-users?limit=100&offset=0');
    if (result) setUsers(result.users);
  }
  async function act(action: string, data: object) {
    const result = await call(action, data);
    if (
      result &&
      ['set-role', 'update-user', 'ban-user', 'unban-user', 'create-user', 'remove-user'].includes(
        action,
      )
    )
      await list();
    if (result && action === 'remove-user') {
      setTarget('');
      setRemove(false);
    }
    if (result && ['revoke-user-session', 'revoke-user-sessions'].includes(action)) setSessions([]);
  }
  const disabled = busy || uncertain;
  return (
    <section className="table-card backend-panel" aria-label="应用管理员" style={{ padding: 24 }}>
      <h3>应用管理员</h3>
      <p>
        先在上方登录具有应用 admin 角色的账号。Console
        管理员身份不会授予这些接口权限。角色、封禁、会话和限时身份代入均作用于当前分支。
      </p>
      <button className="button" disabled={busy} onClick={() => void list()}>
        读取管理员用户列表
      </button>
      <div className="backend-form">
        <label>
          目标应用用户
          <select
            aria-label="管理员目标用户"
            value={target}
            disabled={busy}
            onChange={(e) => {
              setTarget(e.target.value);
              setSessions([]);
              setRemove(false);
            }}
          >
            <option value="">请选择用户</option>
            {users.map((u) => (
              <option key={u.id} value={u.id}>
                {u.email} · {u.role || 'user'}
                {u.banned ? ' · 已封禁' : ''}
              </option>
            ))}
          </select>
        </label>
        <label>
          应用角色
          <select
            aria-label="管理员应用角色"
            value={role}
            disabled={disabled}
            onChange={(e) => setRole(e.target.value)}
          >
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </label>
        <label>
          姓名
          <input
            aria-label="管理员用户姓名"
            value={name}
            disabled={disabled}
            onChange={(e) => setName(e.target.value)}
            maxLength={128}
          />
        </label>
        <label>
          邮箱
          <input
            aria-label="管理员用户邮箱"
            type="email"
            value={email}
            disabled={disabled}
            onChange={(e) => setEmail(e.target.value)}
            maxLength={254}
          />
        </label>
        <label>
          新密码
          <input
            aria-label="管理员用户新密码"
            type="password"
            autoComplete="new-password"
            value={password}
            disabled={disabled}
            onChange={(e) => setPassword(e.target.value)}
            minLength={12}
            maxLength={128}
          />
        </label>
        <label>
          封禁原因
          <input
            aria-label="管理员封禁原因"
            value={reason}
            disabled={disabled}
            onChange={(e) => setReason(e.target.value)}
            maxLength={512}
          />
        </label>
      </div>
      <div className="button-row">
        <button
          className="button"
          disabled={disabled || !email || !name || password.length < 12}
          onClick={() => void act('create-user', { email, name, password, role })}
        >
          创建应用用户
        </button>
        <button
          className="button"
          disabled={disabled || !target || !name}
          onClick={() => void act('update-user', { userId: target, data: { name } })}
        >
          更新应用姓名
        </button>
        <button
          className="button"
          disabled={disabled || !target}
          onClick={() => void act('set-role', { userId: target, role })}
        >
          设置应用角色
        </button>
        <button
          className="button"
          disabled={disabled || !target || password.length < 12}
          onClick={() => void act('set-user-password', { userId: target, newPassword: password })}
        >
          设置应用密码
        </button>
        <button
          className="button"
          disabled={disabled || !target}
          onClick={() => void act('ban-user', { userId: target, banReason: reason })}
        >
          封禁应用用户
        </button>
        <button
          className="button"
          disabled={disabled || !target}
          onClick={() => void act('unban-user', { userId: target })}
        >
          解除应用封禁
        </button>
        <button
          className="button"
          disabled={disabled || !target}
          onClick={() => void act('revoke-user-sessions', { userId: target })}
        >
          撤销目标全部会话
        </button>
        <button
          className="button"
          disabled={disabled || !target}
          onClick={() => void act('impersonate-user', { userId: target })}
        >
          开始身份代入（最多一小时）
        </button>
        <button
          className="button"
          disabled={disabled}
          onClick={() => void act('stop-impersonating', {})}
        >
          结束身份代入
        </button>
      </div>
      <p>身份代入后拥有目标账号的权限；不能代入其他管理员。完成后请结束代入以恢复原会话。</p>
      <button
        className="button"
        disabled={busy || !target}
        onClick={() =>
          void call('list-user-sessions', { userId: target }).then((value) => {
            if (value) {
              setSessions(value.sessions);
              setSessionID('');
            }
          })
        }
      >
        读取目标应用会话
      </button>
      <label>
        待撤销会话
        <select
          aria-label="管理员目标会话"
          value={sessionID}
          disabled={disabled}
          onChange={(e) => setSessionID(e.target.value)}
        >
          <option value="">请选择会话</option>
          {sessions.map((s) => (
            <option key={s.id} value={s.id}>
              {s.id} · {s.expiresAt}
            </option>
          ))}
        </select>
      </label>
      <button
        className="button"
        disabled={disabled || !sessionID}
        onClick={() => {
          const session = sessions.find((s) => s.id === sessionID);
          if (session) void act('revoke-user-session', { sessionToken: session.token });
        }}
      >
        撤销所选应用会话
      </button>
      <label>
        <input
          type="checkbox"
          aria-label="确认删除目标应用用户"
          checked={remove}
          disabled={disabled}
          onChange={(e) => setRemove(e.target.checked)}
        />
        确认永久删除目标应用用户及关联身份记录
      </label>
      <button
        className="button danger"
        disabled={disabled || !target || !remove}
        onClick={() => void act('remove-user', { userId: target })}
      >
        删除目标应用用户
      </button>
      {notice && <p role="status">{notice}</p>}
      {uncertain && (
        <p role="alert">
          上次写入结果未确认，已暂停后续写入。请读取用户与会话核对结果；本页不会自动重试管理员写入。
        </p>
      )}
    </section>
  );
}
