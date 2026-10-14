import { useEffect, useRef, useState } from 'react';

// Mail links are delivered by the actual configured transport. Tokens only
// enter masked inputs/current memory, never localStorage or test evidence.
export function AuthRecoveryPanel({
  branch,
  projectId,
  emailHint,
  onChanged,
}: {
  branch: string;
  projectId: string;
  emailHint?: string;
  onChanged: () => void;
}) {
  const [email, setEmail] = useState(emailHint ?? ''),
    [resetToken, setResetToken] = useState(''),
    [verificationToken, setVerificationToken] = useState(''),
    [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false),
    [notice, setNotice] = useState('');
  const epoch = useRef(0);
  useEffect(() => {
    ++epoch.current;
    setResetToken('');
    setVerificationToken('');
    setPassword('');
    setNotice('');
    setBusy(false);
    setEmail(emailHint ?? '');
    const url = new URL(window.location.href),
      token = url.searchParams.get('token');
    if (url.searchParams.has('auth_branch') && token) {
      if (url.searchParams.get('auth_branch') === branch && /^[a-zA-Z0-9_-]{20,128}$/.test(token))
        setResetToken(token);
      url.searchParams.delete('token');
      window.history.replaceState(null, '', url.pathname + url.search + url.hash);
    }
    return () => {
      ++epoch.current;
    };
  }, [branch, projectId]);
  function callbackURL() {
    const url = new URL('/', window.location.origin);
    url.searchParams.set('auth_branch', branch);
    url.hash = `/projects/${encodeURIComponent(projectId)}/auth`;
    return url.href;
  }
  async function run(action: string, body?: unknown) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const query =
        action === 'verify-email'
          ? `?token=${encodeURIComponent(verificationToken)}&callbackURL=${encodeURIComponent(callbackURL())}`
          : '';
      const response = await fetch(`/auth/v1/${encodeURIComponent(branch)}/${action}${query}`, {
        method: body ? 'POST' : 'GET',
        credentials: 'same-origin',
        headers: body ? { 'Content-Type': 'application/json' } : {},
        body: body ? JSON.stringify(body) : undefined,
        redirect: 'manual',
      });
      if (current !== epoch.current) return;
      if (action === 'verify-email' && response.type === 'opaqueredirect') {
        setNotice('验证请求已返回跳转；请登录或检查应用会话，确认邮箱验证状态。');
        onChanged();
        return;
      }
      if (!response.ok)
        throw new Error(`应用邮件操作失败（${response.status}）；请检查原请求结果后再操作。`);
      if (action === 'reset-password') {
        setNotice('密码已更新，原应用会话已撤销。请使用新密码登录。');
        onChanged();
      } else if (action === 'verify-email') {
        setNotice('邮箱验证请求已处理；请检查应用会话确认状态。');
        onChanged();
      } else
        setNotice('请求已处理。若账号存在且符合条件，请查看邮箱；此页面不会返回邮件中的凭据。');
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '邮件请求失败');
    } finally {
      if (current === epoch.current) {
        setBusy(false);
        setPassword('');
        if (action === 'reset-password') setResetToken('');
        if (action === 'verify-email') setVerificationToken('');
      }
    }
  }
  return (
    <section className="backend-panel" data-testid="auth-recovery-panel">
      <h3>邮箱与账号恢复</h3>
      <p className="subtle-note">
        验证链接和密码找回链接发送到应用用户邮箱。未知账号使用相同请求结果；请保护邮件中的一次性凭据。
      </p>
      <div className="backend-form">
        <label>
          应用收信邮箱
          <input
            aria-label="应用收信邮箱"
            type="email"
            maxLength={254}
            autoComplete="email"
            value={email}
            disabled={busy}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
      </div>
      <div className="toolbar">
        <button
          className="button ghost"
          disabled={busy || !email}
          onClick={() => void run('send-verification-email', { email, callbackURL: callbackURL() })}
        >
          发送邮箱验证邮件
        </button>
        <button
          className="button ghost"
          disabled={busy || !email}
          onClick={() => void run('request-password-reset', { email, redirectTo: callbackURL() })}
        >
          发送密码找回邮件
        </button>
      </div>
      <details>
        <summary>使用邮件中的验证凭据</summary>
        <label>
          邮箱验证凭据
          <input
            aria-label="邮箱验证凭据"
            type="password"
            autoComplete="off"
            maxLength={4096}
            value={verificationToken}
            disabled={busy}
            onChange={(e) => setVerificationToken(e.target.value)}
          />
        </label>
        <button
          className="button ghost"
          disabled={busy || !verificationToken}
          onClick={() => void run('verify-email')}
        >
          验证应用邮箱
        </button>
      </details>
      <div className="backend-form">
        <label>
          密码找回凭据
          <input
            aria-label="密码找回凭据"
            type="password"
            autoComplete="off"
            maxLength={128}
            value={resetToken}
            disabled={busy}
            onChange={(e) => setResetToken(e.target.value)}
          />
        </label>
        <label>
          找回后的新密码
          <input
            aria-label="找回后的新密码"
            type="password"
            autoComplete="new-password"
            minLength={12}
            maxLength={128}
            value={password}
            disabled={busy}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
      </div>
      <button
        className="button primary"
        disabled={busy || !resetToken || password.length < 12 || password.length > 128}
        onClick={() => void run('reset-password', { token: resetToken, newPassword: password })}
      >
        完成密码找回
      </button>
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}
