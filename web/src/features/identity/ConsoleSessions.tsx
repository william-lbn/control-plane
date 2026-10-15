import { useState } from 'react';
import { api } from '../../api';

type Session = { id: string; created_at: string; expires_at: string; current: boolean };
export function ConsoleSessions() {
  const [items, setItems] = useState<Session[]>([]),
    [password, setPassword] = useState(''),
    [busy, setBusy] = useState(false),
    [loaded, setLoaded] = useState(false),
    [more, setMore] = useState(false),
    [notice, setNotice] = useState<{ message: string; error: boolean } | null>(null);
  async function load() {
    const result = await api<{ items: Session[]; has_more: boolean }>('/api/v1/account/sessions');
    setItems(result.items);
    setMore(result.has_more);
    setLoaded(true);
  }
  async function revoke(id?: string) {
    setBusy(true);
    setNotice(null);
    try {
      const result = await api<{ revoked_count: number }>(
        '/api/v1/account/sessions/' + (id ? encodeURIComponent(id) + '/revoke' : 'revoke-others'),
        { method: 'POST', body: JSON.stringify({ password }) },
      );
      await load();
      setNotice({
        message: `已撤销 ${result.revoked_count} 个会话；当前登录保持有效。`,
        error: false,
      });
    } catch (e) {
      setNotice({
        message: e instanceof Error ? e.message : '无法撤销会话，请刷新检查。',
        error: true,
      });
    } finally {
      setPassword('');
      setBusy(false);
    }
  }
  return (
    <section className="panel identity-panel" aria-label="我的控制台会话">
      <h2>我的控制台会话</h2>
      <p>
        管理当前账号的控制台登录。撤销后会话的后续请求立即被拒绝；已开始的请求不会被强制中断。应用
        Auth 会话独立管理。
      </p>
      <div className="selector-row">
        <button
          className="button"
          disabled={busy}
          onClick={() => {
            setNotice(null);
            void load().catch((e) =>
              setNotice({
                message: e instanceof Error ? e.message : '无法读取会话。',
                error: true,
              }),
            );
          }}
        >
          加载我的会话
        </button>
        <label>
          当前账号密码
          <input
            aria-label="会话管理密码"
            type="password"
            autoComplete="current-password"
            value={password}
            maxLength={1024}
            disabled={busy}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <button
          className="button"
          disabled={busy || !password || !items.some((s) => !s.current)}
          onClick={() => {
            if (window.confirm('撤销这个账号除当前页面以外的所有登录会话？')) void revoke();
          }}
        >
          撤销其他全部会话
        </button>
      </div>
      {notice && <p role={notice.error ? 'alert' : 'status'}>{notice.message}</p>}
      {loaded && (
        <p role="status">
          当前显示 {items.length} 个有效会话
          {more ? '，更多会话未展示；全部撤销覆盖全部有效会话。' : '。'}
        </p>
      )}
      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th>会话 ID</th>
              <th>创建时间</th>
              <th>过期时间</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((s) => (
              <tr key={s.id}>
                <td>
                  <code>{s.id}</code>
                </td>
                <td>{new Date(s.created_at).toLocaleString()}</td>
                <td>{new Date(s.expires_at).toLocaleString()}</td>
                <td>{s.current ? '当前会话' : '其他登录'}</td>
                <td>
                  <button
                    className="button"
                    disabled={busy || s.current || !password}
                    onClick={() => {
                      if (window.confirm('撤销所选登录会话？')) void revoke(s.id);
                    }}
                  >
                    撤销此会话
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
