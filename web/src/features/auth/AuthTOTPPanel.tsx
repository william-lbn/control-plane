import { useEffect, useRef, useState } from 'react';
type User = { id: string; name: string; email: string; twoFactorEnabled?: boolean };
export function AuthTOTPPanel({
  branch,
  onUser,
}: {
  branch: string;
  onUser: (user: User | null) => void;
}) {
  const [password, setPassword] = useState(''),
    [code, setCode] = useState(''),
    [backup, setBackup] = useState(''),
    [uri, setURI] = useState(''),
    [codes, setCodes] = useState<string[]>([]),
    [reveal, setReveal] = useState(false),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState('');
  const epoch = useRef(0);
  useEffect(() => {
    epoch.current++;
    setPassword('');
    setCode('');
    setBackup('');
    setURI('');
    setCodes([]);
    setReveal(false);
    setBusy(false);
    setNotice('');
  }, [branch]);
  async function call(action: string, body: unknown) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const response = await fetch(`/auth/v1/${branch}/${action}`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok)
        throw new Error(`MFA 请求失败 (${response.status})；请检查密码、验证码或恢复码。`);
      const value = await response.json();
      if (current !== epoch.current) return;
      if (action === 'two-factor/get-totp-uri') {
        setURI(value.totpURI || '');
        setReveal(false);
        setNotice('绑定地址已重新读取；请仅保存在自己的验证器。');
      } else if (action === 'two-factor/enable') {
        setURI(value.totpURI || '');
        setCodes(value.backupCodes || []);
        setReveal(false);
        setNotice('绑定资料已生成；输入验证器当前六位代码，确认后才启用。恢复码请安全保存。');
      } else if (action === 'two-factor/generate-backup-codes') {
        setCodes(value.backupCodes || []);
        setReveal(false);
        setNotice('恢复码已重新生成，旧恢复码失效。');
      } else {
        const session = await fetch(`/auth/v1/${branch}/get-session`, {
          credentials: 'same-origin',
        });
        if (!session.ok) throw new Error('MFA 已处理；请读取应用会话确认状态。');
        const data = await session.json();
        if (current !== epoch.current) return;
        onUser(data?.user || null);
        setNotice(
          action === 'two-factor/disable'
            ? '应用用户双重认证已关闭。'
            : data?.user
              ? '验证完成，应用会话有效。'
              : '验证已处理，未建立应用会话。',
        );
        if (action === 'two-factor/disable') {
          setURI('');
          setCodes([]);
          setReveal(false);
        }
      }
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : 'MFA 请求失败');
    } finally {
      if (current === epoch.current) {
        setBusy(false);
        setPassword('');
        setCode('');
        setBackup('');
      }
    }
  }
  return (
    <section className="backend-panel" data-testid="auth-totp-panel">
      <h3>应用用户 · TOTP 双重认证</h3>
      <p>
        先登录应用账户，再绑定验证器。密码登录需要完成第二步；邮箱 OTP/Magic Link
        属于各自的免密流程。当前入口不提供可信设备记忆。
      </p>
      <label>
        MFA 确认密码
        <input
          aria-label="MFA 确认密码"
          type="password"
          autoComplete="current-password"
          value={password}
          disabled={busy}
          onChange={(e) => setPassword(e.target.value)}
        />
      </label>
      <div className="backend-form">
        <button
          className="button"
          disabled={busy || password.length < 12}
          onClick={() => void call('two-factor/enable', { password, method: 'totp' })}
        >
          生成 TOTP 绑定资料
        </button>
        <button
          className="button ghost"
          disabled={busy || password.length < 12}
          onClick={() => void call('two-factor/generate-backup-codes', { password })}
        >
          重新生成 MFA 恢复码
        </button>
        <button
          className="button ghost"
          disabled={busy || password.length < 12}
          onClick={() => void call('two-factor/get-totp-uri', { password })}
        >
          重新读取 TOTP 绑定地址
        </button>
        <button
          className="button danger"
          disabled={busy || password.length < 12}
          onClick={() => {
            if (window.confirm('关闭当前应用账户的 TOTP 双重认证？'))
              void call('two-factor/disable', { password });
          }}
        >
          关闭应用用户双重认证
        </button>
      </div>
      {uri && (
        <label>
          TOTP 绑定地址
          <input
            aria-label="TOTP 绑定地址"
            readOnly
            type={reveal ? 'text' : 'password'}
            value={uri}
          />
        </label>
      )}
      {codes.length > 0 && (
        <label>
          MFA 恢复码（安全保存）
          <input
            aria-label="生成的 MFA 恢复码"
            readOnly
            type={reveal ? 'text' : 'password'}
            value={codes.join(' ')}
          />
        </label>
      )}
      {(uri || codes.length > 0) && (
        <button className="button ghost" onClick={() => setReveal(!reveal)}>
          {reveal ? '隐藏绑定资料' : '显示绑定资料'}
        </button>
      )}
      <label>
        TOTP 验证码
        <input
          aria-label="TOTP 验证码"
          inputMode="numeric"
          autoComplete="one-time-code"
          value={code}
          maxLength={6}
          disabled={busy}
          onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
        />
      </label>
      <button
        className="button"
        disabled={busy || !/^\d{6}$/.test(code)}
        onClick={() => void call('two-factor/verify-totp', { code, trustDevice: false })}
      >
        确认 TOTP 绑定或登录
      </button>
      <label>
        MFA 恢复码
        <input
          aria-label="MFA 恢复码"
          type="password"
          value={backup}
          disabled={busy}
          onChange={(e) => setBackup(e.target.value)}
        />
      </label>
      <button
        className="button ghost"
        disabled={busy || !backup}
        onClick={() =>
          void call('two-factor/verify-backup-code', { code: backup, trustDevice: false })
        }
      >
        使用恢复码完成登录
      </button>
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}
