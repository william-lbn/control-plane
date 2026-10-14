import { useEffect, useRef, useState } from 'react';
type OTPUser = { id: string; name: string; email: string; emailVerified?: boolean };
type Purpose = 'sign-in' | 'email-verification' | 'forget-password';
export function AuthOTPPanel({
  branch,
  emailHint,
  onUser,
}: {
  branch: string;
  emailHint?: string;
  onUser: (user: OTPUser | null) => void;
}) {
  const [email, setEmail] = useState(emailHint ?? ''),
    [purpose, setPurpose] = useState<Purpose>('sign-in'),
    [code, setCode] = useState(''),
    [password, setPassword] = useState(''),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState('');
  const epoch = useRef(0);
  useEffect(() => {
    ++epoch.current;
    setEmail(emailHint ?? '');
    setCode('');
    setPassword('');
    setBusy(false);
    setNotice('');
    return () => {
      ++epoch.current;
    };
  }, [branch]);
  async function call(submit: boolean) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    let route: string, body: Record<string, string>;
    if (!submit) {
      route =
        purpose === 'forget-password'
          ? 'email-otp/request-password-reset'
          : 'email-otp/send-verification-otp';
      body = purpose === 'forget-password' ? { email } : { email, type: purpose };
    } else {
      route =
        purpose === 'sign-in'
          ? 'sign-in/email-otp'
          : purpose === 'email-verification'
            ? 'email-otp/verify-email'
            : 'email-otp/reset-password';
      body = purpose === 'forget-password' ? { email, otp: code, password } : { email, otp: code };
    }
    try {
      const response = await fetch(`/auth/v1/${encodeURIComponent(branch)}/${route}`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      const result = await response.json();
      if (current !== epoch.current) return;
      if (!response.ok)
        throw new Error(`验证码请求失败（${response.status}）；请检查代码、有效期和请求限制。`);
      if (submit) {
        setCode('');
        setPassword('');
        const user = result?.user;
        if (purpose === 'sign-in') {
          if (!user || typeof user.id !== 'string' || typeof user.email !== 'string')
            throw new Error('登录响应缺少真实用户');
          onUser(user);
          setNotice('已通过邮箱验证码登录。');
        } else {
          onUser(null);
          setNotice(
            purpose === 'email-verification'
              ? '邮箱验证请求成功，请重新登录确认。'
              : '密码找回成功，请使用新密码登录。',
          );
        }
      } else setNotice('验证码请求已受理，请查看应用邮箱。未知账号的密码找回使用相同响应。');
    } catch (error) {
      if (current === epoch.current)
        setNotice(error instanceof Error ? error.message : '验证码请求失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  return (
    <section
      className="table-card backend-panel"
      style={{ padding: 24 }}
      data-testid="auth-otp-panel"
    >
      <div className="section-header">
        <h3>邮箱验证码</h3>
        <span className="badge green">5 分钟有效</span>
      </div>
      <p className="muted">
        使用真实邮件完成登录、邮箱确认或密码找回。登录可以创建新的应用账号；未验证旧账号首次通过验证码登录时，Auth
        库会撤销其旧密码和会话。
      </p>
      <div className="backend-form">
        <label>
          验证码邮箱
          <input
            aria-label="验证码邮箱"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            disabled={busy}
          />
        </label>
        <label>
          用途
          <select
            aria-label="验证码用途"
            value={purpose}
            onChange={(e) => {
              setPurpose(e.target.value as Purpose);
              setCode('');
              setPassword('');
              setNotice('');
            }}
            disabled={busy}
          >
            <option value="sign-in">登录 / 注册</option>
            <option value="email-verification">验证邮箱</option>
            <option value="forget-password">找回密码</option>
          </select>
        </label>
        <div className="backend-wide button-row">
          <button className="button" disabled={busy || !email} onClick={() => void call(false)}>
            发送应用验证码
          </button>
        </div>
        <label>
          6 位一次性验证码
          <input
            aria-label="应用邮箱验证码"
            type="password"
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/[^0-9]/g, ''))}
            disabled={busy}
          />
        </label>
        {purpose === 'forget-password' && (
          <label>
            找回后的新密码
            <input
              aria-label="验证码找回后的新密码"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={busy}
            />
          </label>
        )}
        <div className="backend-wide button-row">
          <button
            className="button primary"
            disabled={
              busy ||
              !email ||
              !/^\d{6}$/.test(code) ||
              (purpose === 'forget-password' && (password.length < 12 || password.length > 128))
            }
            onClick={() => void call(true)}
          >
            提交应用验证码
          </button>
        </div>
      </div>
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}
