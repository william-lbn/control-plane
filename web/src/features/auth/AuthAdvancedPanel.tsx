import { useEffect, useRef, useState } from 'react';
type User = { id: string; email: string; name: string; role?: string };
type Account = { id: string; providerId: string; accountId: string };
type Event = {
  id: string;
  event_type: string;
  state: string;
  delivery_batches: number;
  last_error?: string;
  created_at: string;
};
export function AuthAdvancedPanel({
  branch,
  providers,
  phoneEnabled,
  hooksEnabled,
  user,
  onUser,
}: {
  branch: string;
  providers: string[];
  phoneEnabled: boolean;
  hooksEnabled: boolean;
  user: User | null;
  onUser: (user: User | null) => void;
}) {
  const [phone, setPhone] = useState(''),
    [code, setCode] = useState(''),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState(''),
    [accounts, setAccounts] = useState<Account[]>([]),
    [events, setEvents] = useState<Event[]>([]),
    [dropped, setDropped] = useState(0);
  const epoch = useRef(0);
  useEffect(() => {
    ++epoch.current;
    return () => {
      ++epoch.current;
    };
  }, [branch]);
  async function request(action: string, body?: unknown) {
    const res = await fetch(`/auth/v1/${encodeURIComponent(branch)}/${action}`, {
      method: body === undefined ? 'GET' : 'POST',
      credentials: 'same-origin',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const value = await res.json();
    if (!res.ok)
      throw Error(
        `操作未完成（${res.status}）${typeof value?.code === 'string' ? ' · ' + value.code : ''}`,
      );
    return value;
  }
  async function run(action: () => Promise<string>) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const message = await action();
      if (current === epoch.current) setNotice(message);
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '请求失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function social(provider: string, link: boolean) {
    const callback = new URL(window.location.href);
    callback.searchParams.set('auth_branch', branch);
    const result = await request(link ? 'link-social' : 'sign-in/social', {
      provider,
      callbackURL: callback.href,
      newUserCallbackURL: callback.href,
      errorCallbackURL: callback.href,
      disableRedirect: true,
    });
    const url = new URL(result.url);
    if (url.username || url.password || !['https:', 'http:'].includes(url.protocol))
      throw Error('登录跳转地址无效');
    window.location.assign(url.href);
    return '正在前往身份提供商';
  }
  const admin = (user?.role || '').split(',').includes('admin');
  return (
    <section
      className="table-card backend-panel"
      style={{ padding: 24 }}
      data-testid="auth-advanced-panel"
    >
      <h3>第三方登录、手机与通知</h3>
      <p role="status" data-testid="auth-advanced-notice">
        {notice}
      </p>
      {!!providers.length && (
        <>
          <div className="button-row">
            {providers.map((id) => (
              <span key={id}>
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  onClick={() => void run(() => social(id, false))}
                >
                  {id} 登录
                </button>
                {user && (
                  <button
                    type="button"
                    className="button"
                    disabled={busy}
                    onClick={() => void run(() => social(id, true))}
                  >
                    绑定 {id}
                  </button>
                )}
              </span>
            ))}
            <button
              type="button"
              className="button"
              disabled={busy || !user}
              onClick={() =>
                void run(async () => {
                  setAccounts(await request('list-accounts'));
                  return '已读取当前应用账号的登录方式';
                })
              }
            >
              读取登录方式
            </button>
          </div>
          <ul>
            {accounts.map((a) => (
              <li key={a.id}>
                {a.providerId} · {a.accountId}{' '}
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  onClick={() =>
                    void run(async () => {
                      await request('unlink-account', { accountId: a.id });
                      setAccounts(await request('list-accounts'));
                      return '已解除绑定';
                    })
                  }
                >
                  解除绑定 {a.providerId}
                </button>
              </li>
            ))}
          </ul>
          <p className="subtle-note">
            已有同邮箱账户需要先登录再显式绑定。最后一种登录方式不能解除；提供商访问令牌不会保留。
          </p>
        </>
      )}
      {phoneEnabled && (
        <div className="backend-form">
          <label>
            手机号码
            <input
              aria-label="应用手机号码"
              type="tel"
              pattern="\+[1-9][0-9]{7,14}"
              maxLength={16}
              value={phone}
              disabled={busy}
              onChange={(e) => setPhone(e.target.value)}
            />
          </label>
          <label>
            短信验证码
            <input
              aria-label="应用短信验证码"
              type="password"
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={6}
              value={code}
              disabled={busy}
              onChange={(e) => setCode(e.target.value)}
            />
          </label>
          <div className="button-row backend-wide">
            <button
              type="button"
              className="button"
              disabled={busy || !/^\+[1-9][0-9]{7,14}$/.test(phone)}
              onClick={() =>
                void run(async () => {
                  await request('phone-number/send-otp', { phoneNumber: phone });
                  return '验证码已交付通知服务，请查看手机短信';
                })
              }
            >
              发送短信验证码
            </button>
            <button
              type="button"
              className="button"
              disabled={busy || !/^\d{6}$/.test(code)}
              onClick={() =>
                void run(async () => {
                  const input = code;
                  setCode('');
                  const result = await request('phone-number/verify', {
                    phoneNumber: phone,
                    code: input,
                    ...(user ? { updatePhoneNumber: true } : {}),
                  });
                  if (!result.user?.id) throw Error('手机验证响应缺少应用身份');
                  onUser(result.user);
                  return user ? '手机已验证并绑定' : '已通过手机验证码登录';
                })
              }
            >
              {user ? '验证并绑定手机' : '手机验证码登录'}
            </button>
          </div>
          <p className="subtle-note backend-wide">
            使用包含国家区号的号码。先登录已有应用账户可绑定手机；未绑定的号码不能直接注册。
          </p>
        </div>
      )}
      {hooksEnabled && (
        <>
          <a
            href={`/auth/v1/${encodeURIComponent(branch)}/.well-known/jwks.json`}
            target="_blank"
            rel="noreferrer"
          >
            事件签名公钥
          </a>
          {admin && (
            <>
              <div className="button-row">
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  onClick={() =>
                    void run(async () => {
                      const value = await request('webhooks/events');
                      setEvents(value.items);
                      setDropped(value.dropped_events);
                      return '已读取事件投递状态';
                    })
                  }
                >
                  读取 Auth 事件
                </button>
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  onClick={() =>
                    void run(async () => {
                      const result = await request('webhooks/drain', {});
                      return `本批投递成功 ${result.delivered}，失败 ${result.failed}`;
                    })
                  }
                >
                  投递待处理 Auth 事件
                </button>
              </div>
              <p>队列容量已满时未入队事件：{dropped}</p>
              <table>
                <thead>
                  <tr>
                    <th>事件</th>
                    <th>状态</th>
                    <th>批次</th>
                    <th>时间</th>
                  </tr>
                </thead>
                <tbody>
                  {events.map((e) => (
                    <tr key={e.id}>
                      <td>{e.event_type}</td>
                      <td>
                        {e.state}
                        {e.last_error ? ' · ' + e.last_error : ''}
                      </td>
                      <td>{e.delivery_batches}</td>
                      <td>{e.created_at}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
        </>
      )}
    </section>
  );
}
