import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type SSOView = {
  provider_id: string;
  mode: 'disabled' | 'optional' | 'required';
  version: number;
  providers: { id: string; name: string }[];
  bindings: { id: string; provider_id: string; subject: string; revoked_at: string | null }[];
  can_manage: boolean;
  provider_available: boolean;
  current_sso: boolean;
};

async function redirectSSO(path: string, body: object) {
  const result = await api<{ authorization_url: string }>(path, {
    method: 'POST',
    body: JSON.stringify(body),
  });
  const target = new URL(result.authorization_url);
  if (!['http:', 'https:'].includes(target.protocol) || target.username || target.password)
    throw new Error('SSO 返回的身份提供商地址无效。');
  location.assign(target.href);
}

export function ConsoleSSOLogin({ organizationId = '' }: { organizationId?: string }) {
  const [org, setOrg] = useState(organizationId);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      await redirectSSO('/auth/sso/start', { organization_id: org.trim() });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  }
  return (
    <form onSubmit={submit} className="identity-form" aria-label="组织 SSO 登录">
      <label>
        组织 ID
        <input
          aria-label="SSO 组织 ID"
          value={org}
          maxLength={128}
          required
          onChange={(e) => setOrg(e.target.value)}
        />
      </label>
      <button className="button" disabled={busy}>
        {busy ? '正在前往身份提供商…' : '使用组织 SSO 登录'}
      </button>
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
    </form>
  );
}

export function ConsoleSSOMFA() {
  const [code, setCode] = useState('');
  const [recovery, setRecovery] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      await api('/auth/sso/mfa', { method: 'POST', body: JSON.stringify({ code, recovery }) });
      setCode('');
      location.replace('/#/organization');
      location.reload();
    } catch (e) {
      setCode('');
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  }
  return (
    <div className="login-screen">
      <form className="login-card" onSubmit={submit}>
        <h1>完成控制台双重验证</h1>
        <p>组织身份已验证。请输入此控制台账号的验证器码或单次恢复码。</p>
        <label>
          SSO 登录验证码
          <input
            aria-label="SSO 登录验证码"
            type="password"
            autoComplete="one-time-code"
            maxLength={64}
            required
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </label>
        <label>
          <input
            type="checkbox"
            checked={recovery}
            onChange={(e) => {
              setRecovery(e.target.checked);
              setCode('');
            }}
          />
          使用恢复码
        </label>
        {error && (
          <p role="alert" className="form-error">
            {error}
          </p>
        )}
        <button className="button primary" disabled={busy}>
          {busy ? '正在验证…' : '验证并进入控制台'}
        </button>
        <a href="/#/login">重新开始登录</a>
      </form>
    </div>
  );
}

export function ConsoleSSO({ organizationId }: { organizationId: string }) {
  const path = `/api/v1/organizations/${encodeURIComponent(organizationId)}/sso`;
  const [view, setView] = useState<SSOView | null>(null);
  const [provider, setProvider] = useState('');
  const [mode, setMode] = useState<SSOView['mode']>('optional');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [uncertain, setUncertain] = useState(false);
  const pending = useRef<{ body: string; key: string; version: number } | null>(null);
  async function reload() {
    const result = await api<SSOView>(path);
    setView(result);
    setProvider(result.provider_id || result.providers[0]?.id || '');
    setMode(result.mode === 'disabled' && result.version === 0 ? 'optional' : result.mode);
  }
  useEffect(() => {
    void reload().catch((e) => setError(String(e)));
  }, [path]);
  async function save(event: FormEvent) {
    event.preventDefault();
    if (!view) return;
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const request = pending.current || {
        body: JSON.stringify({ provider_id: provider, mode, password }),
        key: newRequestKey(),
        version: view.version,
      };
      pending.current = request;
      await api(path, {
        method: 'PUT',
        headers: { 'If-Match': `"${request.version}"`, 'Idempotency-Key': request.key },
        body: request.body,
      });
      pending.current = null;
      setUncertain(false);
      setPassword('');
      setNotice('组织 SSO 策略已保存。');
      if (mode === 'disabled' && view.current_sso) {
        location.replace('/#/login');
        location.reload();
        return;
      }
      await reload();
    } catch (e) {
      const unknown = !(e instanceof ApiError) || e.status >= 500;
      setUncertain(unknown);
      if (!unknown) {
        pending.current = null;
        setPassword('');
      }
      setError(
        (e instanceof Error ? e.message : String(e)) + (unknown ? ' 请使用原提交继续确认。' : ''),
      );
    } finally {
      setBusy(false);
    }
  }
  async function link() {
    setBusy(true);
    setError('');
    try {
      await redirectSSO(path + '/link', { password });
    } catch (e) {
      setPassword('');
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  }
  async function revoke(id: string) {
    setBusy(true);
    setError('');
    try {
      await api(path + '/bindings/' + encodeURIComponent(id), {
        method: 'DELETE',
        body: JSON.stringify({ password }),
      });
      if (view?.current_sso) {
        location.replace('/#/login');
        location.reload();
        return;
      }
      setPassword('');
      setNotice('绑定已撤销，关联的 SSO 会话已失效。');
      await reload();
    } catch (e) {
      setPassword('');
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel identity-panel" aria-label="控制台组织 SSO">
      <h2>控制台组织 SSO</h2>
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}
      {!view ? (
        <p>正在读取 SSO 策略…</p>
      ) : (
        <>
          <p>
            当前策略：{view.mode} ·{' '}
            {view.current_sso ? '当前会话已通过组织 SSO' : '当前为其他登录方式'}
          </p>
          {!view.providers.length && <p>请由运维配置此组织的 OIDC 身份提供商。</p>}
          <p className="muted">
            先使用本地账号绑定身份，再通过 SSO
            登录。组织强制策略约束控制台访问；机器凭据仍沿用独立授权。现有双重验证继续生效。
          </p>
          <label>
            当前控制台密码
            <input
              aria-label="SSO 当前控制台密码"
              type="password"
              autoComplete="current-password"
              maxLength={1024}
              value={password}
              disabled={busy || uncertain}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          {view.can_manage && view.providers.length > 0 && (
            <form onSubmit={save} className="identity-form" aria-label="组织 SSO 策略">
              <label>
                身份提供商
                <select
                  aria-label="SSO 身份提供商"
                  value={provider}
                  disabled={busy || uncertain}
                  onChange={(e) => setProvider(e.target.value)}
                >
                  {view.providers.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                控制台策略
                <select
                  aria-label="SSO 控制台策略"
                  value={mode}
                  disabled={busy || uncertain}
                  onChange={(e) => setMode(e.target.value as SSOView['mode'])}
                >
                  <option value="optional">允许 SSO</option>
                  <option value="required">要求组织 SSO</option>
                  <option value="disabled">停用 SSO</option>
                </select>
              </label>
              <button className="button" disabled={busy || !password}>
                {uncertain ? '继续确认原 SSO 策略提交' : '保存 SSO 策略'}
              </button>
            </form>
          )}
          {view.provider_available && (
            <button
              className="button"
              disabled={busy || uncertain || !password}
              onClick={() => void link()}
            >
              绑定当前账号的 SSO 身份
            </button>
          )}
          {view.bindings.map((b) => (
            <div key={b.id} className="section-header">
              <span>
                {b.provider_id} · {b.subject} · {b.revoked_at ? '已撤销' : '已绑定'}
              </span>
              {!b.revoked_at && (
                <button
                  className="button danger"
                  disabled={busy || uncertain || !password}
                  onClick={() => void revoke(b.id)}
                >
                  撤销此 SSO 绑定
                </button>
              )}
            </div>
          ))}
          {view.provider_available && <ConsoleSSOLogin organizationId={organizationId} />}
        </>
      )}
    </section>
  );
}
