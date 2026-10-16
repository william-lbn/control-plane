import { useEffect, useRef, useState } from 'react';
import { api, ApiError, projectPath } from '../../api';
import { newRequestKey } from '../../shared/requestKey';
import type { Entry, HistoryLoadTarget } from './QueryHistory';

type Sharing = {
  history_id: string;
  is_shared: boolean;
  version_token: string;
  shared_at: string | null;
};
type Attempt = {
  key: string;
  version: string;
  body: { shared: boolean; acknowledge_sensitive_sql_sharing: boolean };
};
export function HistorySharingPanel({
  projectId,
  historyId,
  onClose,
  onChanged,
}: {
  projectId: string;
  historyId: string;
  onClose: () => void;
  onChanged: (value: Sharing) => void;
}) {
  const base =
    projectPath(projectId) + '/query-history/' + encodeURIComponent(historyId) + '/sharing';
  const [value, setValue] = useState<Sharing | null>(null),
    [ack, setAck] = useState(false),
    [busy, setBusy] = useState(true),
    [notice, setNotice] = useState(''),
    [uncertain, setUncertain] = useState(false);
  const attempt = useRef<Attempt | null>(null),
    epoch = useRef(0);
  useEffect(() => {
    let cancelled = false;
    epoch.current++;
    setBusy(true);
    api<Sharing>(base)
      .then((v) => {
        if (!cancelled) setValue(v);
      })
      .catch((e) => {
        if (!cancelled) setNotice(e instanceof Error ? e.message : '共享状态读取失败');
      })
      .finally(() => {
        if (!cancelled) setBusy(false);
      });
    return () => {
      cancelled = true;
      epoch.current++;
    };
  }, [base]);
  async function reload() {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const next = await api<Sharing>(base);
      if (current !== epoch.current) return;
      setValue(next);
      onChanged(next);
      attempt.current = null;
      setUncertain(false);
      setAck(false);
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '读取失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function save() {
    if (!value) return;
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      if (!attempt.current)
        attempt.current = {
          key: 'history-sharing-' + newRequestKey(),
          version: value.version_token,
          body: { shared: !value.is_shared, acknowledge_sensitive_sql_sharing: ack },
        };
      const original = attempt.current;
      await api<Sharing>(base, {
        method: 'PUT',
        headers: { 'Idempotency-Key': original.key, 'If-Match': '"' + original.version + '"' },
        body: JSON.stringify(original.body),
      });
      const next = await api<Sharing>(base);
      if (current !== epoch.current) return;
      setValue(next);
      onChanged(next);
      setAck(false);
      attempt.current = null;
      setUncertain(false);
      setNotice(
        next.is_shared
          ? '已共享给当前项目 SQL 编辑者；仅共享这一条历史。'
          : '已撤销共享；本人原始历史保持。',
      );
    } catch (e) {
      if (current !== epoch.current) return;
      const unknown = !!attempt.current && (!(e instanceof ApiError) || e.status >= 500);
      setUncertain(unknown);
      if (!unknown) attempt.current = null;
      setNotice(
        (e instanceof Error ? e.message : '共享结果未确认') +
          (unknown ? '；可重试原提交或加载当前状态核对。' : ''),
      );
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  return (
    <section className="panel" aria-label="本人历史共享设置">
      <h3>共享本人执行历史</h3>
      <code>{historyId}</code>
      <p>
        SQL 可能包含敏感常量。共享后，当前项目 Editor/Admin 可读取原
        SQL、角色/数据库名和执行状态，但不含密码或结果。撤回或删除/到期会关闭后续读取，无法收回他人已复制的内容。不会执行
        SQL。
      </p>
      {value && (
        <>
          <p data-testid="history-sharing-current">
            {value.is_shared ? '已共享' : '仅本人'} · 版本 {value.version_token}
          </p>
          {!value.is_shared && (
            <label>
              <input
                type="checkbox"
                checked={ack}
                disabled={busy || uncertain}
                onChange={(e) => setAck(e.target.checked)}
              />
              我确认将这一条敏感 SQL 历史共享给项目 SQL 编辑者
            </label>
          )}
          <button
            className="button"
            disabled={busy || (!value.is_shared && !ack && !uncertain)}
            onClick={() => void save()}
          >
            {uncertain ? '重试原共享提交' : value.is_shared ? '撤销此历史共享' : '确认共享此历史'}
          </button>
        </>
      )}
      <button className="button" disabled={busy} onClick={() => void reload()}>
        加载当前共享状态
      </button>
      <button className="button" disabled={busy || uncertain} onClick={onClose}>
        关闭共享设置
      </button>
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}

type SharedEntry = Omit<Entry, 'name' | 'name_version_token' | 'name_updated_at'> & {
  owner_username: string;
};
export function SharedHistory({
  projectId,
  onLoad,
}: {
  projectId: string;
  onLoad: (sql: string, entry: HistoryLoadTarget) => void;
}) {
  const base = projectPath(projectId) + '/shared-query-history',
    epoch = useRef(0);
  const [items, setItems] = useState<SharedEntry[]>([]),
    [cursor, setCursor] = useState<string | null>(null),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState(''),
    [loaded, setLoaded] = useState(false);
  useEffect(
    () => () => {
      epoch.current++;
    },
    [base],
  );
  async function load(more = false) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const next = await api<{ items: SharedEntry[]; next_cursor: string | null }>(
        base + (more && cursor ? '?cursor=' + encodeURIComponent(cursor) : ''),
      );
      if (current !== epoch.current) return;
      setItems((old) => (more ? [...old, ...next.items] : next.items));
      setCursor(next.next_cursor);
      setLoaded(true);
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '共享历史读取失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function detail(id: string) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const value = await api<SharedEntry & { sql: string }>(base + '/' + encodeURIComponent(id));
      if (current !== epoch.current) return;
      onLoad(value.sql, value);
      setNotice(
        value.truncated
          ? '已加载截断的共享 SQL，请审查补全；没有执行查询。'
          : '已加载共享 SQL，连接密码与结果已清空；没有执行查询。',
      );
    } catch (e) {
      if (current === epoch.current)
        setNotice(
          (e instanceof Error ? e.message : '共享历史不可读') +
            '；共享可能已撤回或成员权限已变化，请刷新。',
        );
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  return (
    <section
      className="panel"
      aria-label="项目共享 SQL 执行历史"
      data-testid="shared-history-panel"
    >
      <h3>项目共享执行历史</h3>
      <p>
        仅展示作者明确授权且当前仍有项目 SQL 权限的记录。历史加载不会执行
        SQL；复制后的内容无法由撤回操作收回。
      </p>
      <button className="button" disabled={busy} onClick={() => void load()}>
        刷新项目共享历史
      </button>
      {loaded && items.length === 0 && <p>暂无可读的项目共享执行历史。</p>}
      {items.length > 0 && (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>作者 / 记录</th>
                <th>数据库 / 角色</th>
                <th>结果 / 时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={item.id} data-testid={'shared-history-' + item.id}>
                  <td>
                    {item.owner_username}
                    <br />
                    <code>{item.id}</code>
                  </td>
                  <td>
                    {item.database_name}
                    <br />
                    {item.role_name}
                  </td>
                  <td>
                    {item.outcome}
                    <br />
                    {new Date(item.started_at).toLocaleString()}
                  </td>
                  <td>
                    <button
                      className="button"
                      disabled={busy}
                      aria-label={'加载共享历史 ' + item.id}
                      onClick={() => void detail(item.id)}
                    >
                      加载共享 SQL
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {cursor && (
        <button className="button" disabled={busy} onClick={() => void load(true)}>
          加载更早共享历史
        </button>
      )}
      {notice && <p role="status">{notice}</p>}
    </section>
  );
}
