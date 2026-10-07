import { useEffect, useRef, useState, type FormEvent } from 'react';
import { api, type Organization, type Page } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Invitation = {
  id: string;
  org_id: string;
  username: string;
  role: string;
  created_at: string;
  expires_at: string;
  accepted_at: string | null;
  revoked_at: string | null;
  state: 'pending' | 'expired' | 'revoked' | 'accepted';
  secret_available?: boolean;
  token?: string;
};
const states = { pending: '等待接受', expired: '已过期', revoked: '已撤销', accepted: '已接受' };

export function Invitations({
  organization,
  onAccepted,
  showError,
}: {
  organization: Organization | null;
  onAccepted: (id: string) => Promise<void>;
  showError: (error: unknown) => void;
}) {
  const isAdmin = organization?.role === 'admin' || organization?.role === 'owner';
  const path = `/api/v1/organizations/${encodeURIComponent(organization?.id || '')}/invitations`;
  const [items, setItems] = useState<Invitation[]>([]);
  const [username, setUsername] = useState('');
  const [role, setRole] = useState('collaborator');
  const [hours, setHours] = useState(24);
  const [token, setToken] = useState('');
  const [acceptToken, setAcceptToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const key = useRef('');
  async function reload() {
    setItems((await api<Page<Invitation>>(path)).items);
  }
  useEffect(() => {
    if (isAdmin) void reload().catch(showError);
  }, [path, isAdmin]);
  function changed() {
    key.current = '';
    setNotice('');
  }
  async function create(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setToken('');
    setNotice('');
    try {
      if (!key.current) key.current = newRequestKey();
      const item = await api<Invitation>(path, {
        method: 'POST',
        headers: { 'Idempotency-Key': key.current },
        body: JSON.stringify({ username, role, expires_hours: hours }),
      });
      setToken(item.token || '');
      setNotice(
        item.secret_available
          ? '邀请已创建。请通过安全渠道交给受邀账号，关闭后无法再次查看。'
          : '已找到同一邀请，凭据不会再次返回。若首次响应丢失，请撤销该邀请后重新创建。',
      );
      await reload();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  async function revoke(item: Invitation) {
    setBusy(true);
    try {
      await api(`${path}/${encodeURIComponent(item.id)}`, { method: 'DELETE' });
      setToken('');
      key.current = '';
      await reload();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  async function accept(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setNotice('');
    try {
      const result = await api<{ organization_id: string }>('/api/v1/invitations/accept', {
        method: 'POST',
        body: JSON.stringify({ token: acceptToken }),
      });
      setAcceptToken('');
      setNotice('邀请已接受，组织访问权限已更新。');
      await onAccepted(result.organization_id);
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel identity-panel invitation-panel">
      <div className="section-header">
        <h2>成员邀请</h2>
        <span>ACCOUNT / ONBOARDING</span>
      </div>
      <p className="muted">
        新用户在登录页选择“受邀注册”并自行设置密码。已有账号登录后接受邀请。
        邀请绑定账号，Collaborator 获得项目授权后才能访问项目。
      </p>
      {isAdmin && (
        <>
          <form className="identity-form" onSubmit={create}>
            <label>
              受邀账号
              <input
                aria-label="受邀账号"
                value={username}
                required
                minLength={3}
                maxLength={128}
                autoComplete="off"
                disabled={busy}
                onChange={(e) => {
                  setUsername(e.target.value);
                  changed();
                }}
              />
            </label>
            <label>
              邀请角色
              <select
                aria-label="邀请角色"
                value={role}
                disabled={busy}
                onChange={(e) => {
                  setRole(e.target.value);
                  changed();
                }}
              >
                {['admin', 'editor', 'viewer', 'collaborator'].map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </label>
            <label>
              有效期
              <select
                aria-label="邀请有效期"
                value={hours}
                disabled={busy}
                onChange={(e) => {
                  setHours(Number(e.target.value));
                  changed();
                }}
              >
                <option value={1}>1 小时</option>
                <option value={24}>24 小时</option>
                <option value={168}>7 天</option>
              </select>
            </label>
            <button className="button primary" disabled={busy}>
              创建邀请
            </button>
          </form>
          {token && (
            <div className="invitation-secret">
              <label>
                一次性邀请凭据
                <input
                  aria-label="一次性邀请凭据"
                  type="password"
                  value={token}
                  readOnly
                  autoComplete="off"
                  onFocus={(e) => e.target.select()}
                />
              </label>
              <p className="muted">选择输入框并复制。凭据不放入链接、浏览器存储或测试截图。</p>
              <button
                className="button"
                onClick={() => {
                  setToken('');
                  key.current = '';
                }}
              >
                已保存，关闭凭据
              </button>
            </div>
          )}
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>受邀账号</th>
                  <th>角色</th>
                  <th>有效期至</th>
                  <th>状态</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={item.id}>
                    <td>{item.username}</td>
                    <td>{item.role}</td>
                    <td>{new Date(item.expires_at).toLocaleString()}</td>
                    <td>{states[item.state]}</td>
                    <td>
                      <button
                        className="text-button"
                        disabled={busy || item.state === 'accepted' || item.state === 'revoked'}
                        onClick={() => void revoke(item)}
                      >
                        撤销邀请 {item.username}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
      <form className="identity-form" onSubmit={accept}>
        <label>
          接受发给当前账号的邀请
          <input
            aria-label="待接受邀请凭据"
            type="password"
            value={acceptToken}
            required
            autoComplete="off"
            disabled={busy}
            onChange={(e) => setAcceptToken(e.target.value)}
          />
        </label>
        <button className="button" disabled={busy}>
          接受组织邀请
        </button>
      </form>
      {notice && (
        <p role="status" className="muted">
          {notice}
        </p>
      )}
    </section>
  );
}

export function Signup() {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [done, setDone] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault();
    setError('');
    if (password !== confirm) {
      setError('两次密码输入不一致。');
      return;
    }
    setBusy(true);
    try {
      await api('/auth/signup', {
        method: 'POST',
        body: JSON.stringify({ username, password, token }),
      });
      setPassword('');
      setConfirm('');
      setToken('');
      setDone(true);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="login-screen">
      <div className="login-glow" />
      <div className="login-layout">
        <div className="login-intro">
          <div className="brand large">
            neon<span className="brand-dot">.</span>
          </div>
          <div className="eyebrow">WORKSPACE / INVITATION</div>
          <h1>
            加入团队，
            <br />
            <em>从你的账号开始。</em>
          </h1>
          <p>接受管理员分配的组织角色，独立管理你的控制台密码。</p>
        </div>
        <form className="login-card" onSubmit={submit}>
          <span className="eyebrow">受邀注册</span>
          <h2>创建控制台账号</h2>
          {done ? (
            <p role="status">注册成功。请使用新账号登录控制台。</p>
          ) : (
            <>
              <label>
                受邀用户名
                <input
                  aria-label="受邀用户名"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  required
                  minLength={3}
                  maxLength={128}
                  autoComplete="username"
                  disabled={busy}
                />
              </label>
              <label>
                邀请凭据
                <input
                  aria-label="邀请凭据"
                  type="password"
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  required
                  autoComplete="off"
                  disabled={busy}
                />
              </label>
              <label>
                设置密码
                <input
                  aria-label="设置密码"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  required
                  minLength={12}
                  maxLength={256}
                  autoComplete="new-password"
                  disabled={busy}
                />
              </label>
              <label>
                确认密码
                <input
                  aria-label="确认密码"
                  type="password"
                  value={confirm}
                  onChange={(e) => setConfirm(e.target.value)}
                  required
                  minLength={12}
                  maxLength={256}
                  autoComplete="new-password"
                  disabled={busy}
                />
              </label>
              {error && (
                <p role="alert" className="form-error">
                  {error}
                </p>
              )}
              <button className="button primary full" disabled={busy}>
                {busy ? '正在注册…' : '注册并加入组织'}
              </button>
            </>
          )}
          <a className="text-button" href="#/projects">
            返回登录
          </a>
          <small>
            仅接受有效邀请。已有账号请登录后在“组织与安全”接受邀请。此账号用于控制台管理。
          </small>
        </form>
      </div>
    </div>
  );
}
