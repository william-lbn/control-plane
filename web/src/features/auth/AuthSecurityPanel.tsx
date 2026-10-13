import { useEffect, useRef, useState } from 'react';

type SessionMetadata = {
  id: string;
  createdAt: string;
  expiresAt: string;
  userAgent?: string;
  ipAddress?: string;
};

// App session tokens stay in a private component ref. They never enter DOM,
// storage, URL, error text or evidence. Better Auth validates token ownership.
export function AuthSecurityPanel({
  branch,
  onChanged,
}: {
  branch: string;
  onChanged: () => void;
}) {
  const [sessions, setSessions] = useState<SessionMetadata[]>([]);
  const tokens = useRef(new Map<string, string>());
  const [currentPassword, setCurrentPassword] = useState('');
  const [nextPassword, setNextPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const epoch = useRef(0);
  useEffect(() => {
    ++epoch.current;
    tokens.current.clear();
    setSessions([]);
    setCurrentPassword('');
    setNextPassword('');
    return () => {
      ++epoch.current;
      tokens.current.clear();
    };
  }, [branch]);
  async function call(action: string, body?: unknown) {
    const current = epoch.current;
    const response = await fetch(`/auth/v1/${encodeURIComponent(branch)}/${action}`, {
      method: body ? 'POST' : 'GET',
      credentials: 'same-origin',
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    if (response.status === 401 && current === epoch.current) onChanged();
    if (!response.ok) throw new Error(`应用安全操作失败（${response.status}）`);
    return response.json();
  }
  async function load() {
    const current = epoch.current;
    const items = (await call('list-sessions')) as (SessionMetadata & { token: string })[];
    if (current !== epoch.current) return;
    if (!Array.isArray(items) || items.length > 100) throw new Error('会话列表格式或数量超出限制');
    tokens.current.clear();
    setSessions(
      items.map((item) => {
        tokens.current.set(item.id, item.token);
        return {
          id: item.id,
          createdAt: item.createdAt,
          expiresAt: item.expiresAt,
          userAgent: item.userAgent,
          ipAddress: item.ipAddress,
        };
      }),
    );
  }
  async function run(action: () => Promise<void>) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      await action();
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '操作失败');
    } finally {
      if (current === epoch.current) {
        setBusy(false);
        setCurrentPassword('');
        setNextPassword('');
      }
    }
  }
  async function changePassword() {
    const current = epoch.current;
    await call('change-password', {
      currentPassword,
      newPassword: nextPassword,
      revokeOtherSessions: true,
    });
    if (current !== epoch.current) return;
    tokens.current.clear();
    setSessions([]);
    onChanged();
    setNotice('密码已更新并撤销其他应用会话；请重新登录验证新密码。');
  }
  return (
    <section className="backend-panel" data-testid="auth-security-panel">
      <h3>应用账号安全</h3>
      <p className="subtle-note">
        仅操作当前分支的当前应用用户。撤销会话后，其后续会话请求/JWT 签发被拒绝；已经签发的 JWT
        最多保留 5 分钟有效期。此流程不修改 Console 账号。
      </p>
      <div className="backend-form">
        <label>
          当前应用密码
          <input
            aria-label="当前应用密码"
            type="password"
            autoComplete="current-password"
            value={currentPassword}
            disabled={busy}
            onChange={(e) => setCurrentPassword(e.target.value)}
          />
        </label>
        <label>
          新应用密码
          <input
            aria-label="新应用密码"
            type="password"
            autoComplete="new-password"
            minLength={12}
            maxLength={128}
            value={nextPassword}
            disabled={busy}
            onChange={(e) => setNextPassword(e.target.value)}
          />
        </label>
        <div className="backend-wide button-row">
          <button
            className="button"
            disabled={
              busy || !currentPassword || nextPassword.length < 12 || nextPassword.length > 128
            }
            onClick={() => void run(changePassword)}
          >
            更新密码并撤销其他会话
          </button>
          <button
            className="button"
            disabled={busy}
            onClick={() =>
              void run(async () => {
                await load();
                setNotice('已读取当前用户的应用会话。');
              })
            }
          >
            查看应用会话
          </button>
          <button
            className="button"
            disabled={busy}
            onClick={() =>
              void run(async () => {
                await call('revoke-other-sessions', {});
                await load();
                setNotice('其他应用会话已撤销。');
              })
            }
          >
            撤销其他应用会话
          </button>
        </div>
      </div>
      <div role="status" data-testid="auth-security-result">
        {notice}
      </div>
      <table>
        <thead>
          <tr>
            <th>创建时间</th>
            <th>过期时间</th>
            <th>客户端</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          {sessions.map((item) => (
            <tr key={item.id}>
              <td>{item.createdAt}</td>
              <td>{item.expiresAt}</td>
              <td>{item.userAgent || '未提供'}</td>
              <td>
                <button
                  className="button"
                  disabled={busy}
                  onClick={() =>
                    void run(async () => {
                      const token = tokens.current.get(item.id);
                      if (!token) throw new Error('请刷新会话列表');
                      await call('revoke-session', { token });
                      await load();
                      setNotice('选定应用会话已撤销。');
                    })
                  }
                >
                  撤销此会话
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
