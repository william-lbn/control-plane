import { useEffect, useRef, useState } from 'react';

type User = { id: string; name: string; email: string; emailVerified?: boolean };

// The console is an application playground. Email tokens enter masked memory
// only; the actual maintained Auth endpoint sets its HttpOnly branch cookie.
export function AuthMagicLinkPanel({
  branch,
  projectId,
  emailHint,
  onUser,
}: {
  branch: string;
  projectId: string;
  emailHint?: string;
  onUser: (user: User | null) => void;
}) {
  const [email, setEmail] = useState(emailHint ?? '');
  const [link, setLink] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const epoch = useRef(0);
  useEffect(() => {
    const current = ++epoch.current;
    setEmail(emailHint ?? '');
    setLink('');
    setBusy(false);
    setNotice('');
    const url = new URL(window.location.href);
    if (
      url.searchParams.get('magic_link_callback') === '1' &&
      url.searchParams.get('auth_branch') === branch
    ) {
      const failed = url.searchParams.has('error');
      for (const key of ['magic_link_callback', 'error', 'token']) url.searchParams.delete(key);
      window.history.replaceState(null, '', url.pathname + url.search + url.hash);
      setBusy(true);
      void fetch(`/auth/v1/${encodeURIComponent(branch)}/get-session`, {
        credentials: 'same-origin',
        cache: 'no-store',
      })
        .then(async (response) => {
          if (!response.ok) throw new Error('会话读取失败');
          const session = await response.json();
          if (current !== epoch.current) return;
          // A pre-existing cookie alone cannot prove a failed redemption succeeded.
          const user = failed ? null : (session?.user ?? null);
          onUser(user);
          setNotice(
            user ? '邮箱链接已建立应用会话' : '邮箱链接未建立会话，可能已使用、过期或注册未开放',
          );
        })
        .catch(() => {
          if (current === epoch.current) setNotice('无法确认链接结果，请检查原请求再操作');
        })
        .finally(() => {
          if (current === epoch.current) setBusy(false);
        });
    }
    return () => {
      ++epoch.current;
    };
  }, [branch, projectId]);
  function callbackURL() {
    const url = new URL('/', window.location.origin);
    url.searchParams.set('auth_branch', branch);
    url.searchParams.set('magic_link_callback', '1');
    url.hash = `/projects/${encodeURIComponent(projectId)}/auth`;
    return url.href;
  }
  async function send() {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const callback = callbackURL();
      const response = await fetch(`/auth/v1/${encodeURIComponent(branch)}/sign-in/magic-link`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          email,
          callbackURL: callback,
          newUserCallbackURL: callback,
          errorCallbackURL: callback,
        }),
      });
      if (!response.ok) throw new Error(`发送失败（${response.status}），请检查原请求`);
      if (current === epoch.current)
        setNotice('请查看邮箱并打开一次性登录链接；符合策略的账号可以登录。');
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '发送失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  function redeem() {
    try {
      const url = new URL(link);
      if (
        url.origin !== window.location.origin ||
        url.pathname !== `/auth/v1/${branch}/magic-link/verify` ||
        url.username ||
        url.password ||
        url.hash ||
        !url.searchParams.get('token') ||
        link.length > 4096
      )
        throw new Error('请使用当前分支邮件中的登录链接');
      const callback = callbackURL();
      for (const key of ['callbackURL', 'newUserCallbackURL', 'errorCallbackURL'])
        url.searchParams.set(key, callback);
      setLink('');
      setBusy(true);
      window.location.assign(url.href);
    } catch (e) {
      setLink('');
      setNotice(e instanceof Error ? e.message : '链接无效');
    }
  }
  return (
    <section className="backend-panel" data-testid="auth-magic-link-panel">
      <h3>邮箱免密码登录</h3>
      <p className="subtle-note">
        邮件中的链接只能使用一次。账号注册遵循当前分支策略；首次验证既有未验证账号会撤销旧会话与旧密码。
      </p>
      <div className="backend-form">
        <label>
          免密码登录邮箱
          <input
            aria-label="免密码登录邮箱"
            type="email"
            maxLength={254}
            autoComplete="email"
            disabled={busy}
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
      </div>
      <button className="button primary" disabled={busy || !email} onClick={() => void send()}>
        发送免密码登录链接
      </button>
      <details>
        <summary>使用本地收件箱的登录链接</summary>
        <label>
          应用登录邮件链接
          <input
            aria-label="应用登录邮件链接"
            type="password"
            maxLength={4096}
            autoComplete="off"
            value={link}
            disabled={busy}
            onChange={(e) => setLink(e.target.value)}
          />
        </label>
        <button className="button ghost" disabled={busy || !link} onClick={redeem}>
          打开应用登录链接
        </button>
      </details>
      <div className="backend-result" role="status" data-testid="auth-magic-link-result">
        {notice}
      </div>
    </section>
  );
}
