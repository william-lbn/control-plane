import { useState } from 'react';
import { api } from '../../api';

type Policy = {
  enabled: boolean;
  enrollment_available: boolean;
  recovery_codes_remaining: number;
  pending_until: string | null;
};
export function ConsoleMFA() {
  const [policy, setPolicy] = useState<Policy | null>(null),
    [password, setPassword] = useState(''),
    [code, setCode] = useState(''),
    [recovery, setRecovery] = useState(false),
    [secret, setSecret] = useState(''),
    [codes, setCodes] = useState<string[]>([]),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState<{ message: string; error: boolean } | null>(null);
  async function load() {
    setPolicy(await api<Policy>('/api/v1/account/mfa'));
  }
  async function perform(action: 'enroll' | 'confirm' | 'disable') {
    setBusy(true);
    setNotice(null);
    try {
      const result = await api<{ secret?: string; recovery_codes?: string[] }>(
        '/api/v1/account/mfa/' + action,
        { method: 'POST', body: JSON.stringify({ password, code, recovery }) },
      );
      setSecret(action === 'enroll' ? result.secret || '' : '');
      setCodes(result.recovery_codes || []);
      await load();
      setNotice({
        message:
          action === 'enroll'
            ? '请在验证器手动添加此密钥，再输入当前验证码确认。'
            : action === 'confirm'
              ? '双重验证已启用；其他控制台会话已撤销。请立即保存恢复码。'
              : '双重验证已关闭；其他控制台会话已撤销。',
        error: false,
      });
    } catch (e) {
      setNotice({
        message: e instanceof Error ? e.message : '操作未确认，请重新加载状态。',
        error: true,
      });
    } finally {
      setPassword('');
      setCode('');
      setBusy(false);
    }
  }
  return (
    <section className="panel identity-panel" aria-label="控制台双重验证">
      <h2>控制台双重验证</h2>
      <p>
        使用验证器保护此账号。启用后每次登录需要密码和一次验证码；丢失验证器时可使用一次性恢复码。
      </p>
      <div className="selector-row">
        <button
          className="button"
          disabled={busy}
          onClick={() => {
            setNotice(null);
            void load().catch((e) => setNotice({ message: e.message, error: true }));
          }}
        >
          加载双重验证状态
        </button>
        {policy && (
          <strong data-testid="console-mfa-state">
            {policy.enabled ? '已启用' : '未启用'} · 剩余恢复码 {policy.recovery_codes_remaining}
          </strong>
        )}
      </div>
      {policy && (
        <>
          {!policy.enrollment_available && <p role="alert">运营方尚未配置双重验证加密密钥。</p>}
          <div className="selector-row">
            <label>
              双重验证操作密码
              <input
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </label>
            <label>
              双重验证操作验证码
              <input
                type="password"
                autoComplete="one-time-code"
                maxLength={64}
                value={code}
                onChange={(e) => setCode(e.target.value)}
              />
            </label>
            {policy.enabled && (
              <label>
                <input
                  type="checkbox"
                  checked={recovery}
                  onChange={(e) => {
                    setRecovery(e.target.checked);
                    setCode('');
                  }}
                />
                使用一次性恢复码
              </label>
            )}
          </div>
          {!policy.enabled && (
            <button
              className="button"
              disabled={busy || !password || !policy.enrollment_available}
              onClick={() => void perform('enroll')}
            >
              {secret ? '重新生成验证器密钥' : '设置验证器'}
            </button>
          )}
          {secret && (
            <div className="panel" data-testid="console-mfa-enrollment">
              <p>
                验证器账号：Neon Console；TOTP / SHA-1 / 6 位 / 30 秒。密钥仅保留在当前页面，10
                分钟内确认。
              </p>
              <label>
                验证器密钥
                <input readOnly autoComplete="off" value={secret} />
              </label>
              <p>添加完成后重新输入当前账号密码及验证器显示的验证码。</p>
              <button
                className="button primary"
                disabled={busy || !password || !code}
                onClick={() => void perform('confirm')}
              >
                确认启用双重验证
              </button>
            </div>
          )}
          {policy.enabled && (
            <button
              className="button"
              disabled={busy || !password || !code}
              onClick={() => void perform('disable')}
            >
              确认关闭双重验证
            </button>
          )}
          {codes.length > 0 && (
            <div className="panel" data-testid="console-mfa-recovery">
              <h3>保存一次性恢复码</h3>
              <p>只显示这一次，每项只能使用一次。请放入密码管理器；使用后不能通过刷新重新取得。</p>
              <label>
                一次性恢复码
                <textarea readOnly autoComplete="off" rows={10} value={codes.join('\n')} />
              </label>
              <button className="button" onClick={() => setCodes([])}>
                已保存，隐藏恢复码
              </button>
            </div>
          )}
        </>
      )}
      {notice && (
        <p
          role={notice.error ? 'alert' : 'status'}
          className={notice.error ? 'form-error' : 'muted'}
        >
          {notice.message}
        </p>
      )}
    </section>
  );
}
