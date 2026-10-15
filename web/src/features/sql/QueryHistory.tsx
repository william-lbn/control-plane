import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import { HistoryPolicy } from '../identity/HistoryPolicy';
type Entry = {
  id: string;
  endpoint_id: string;
  branch_id: string;
  mode: 'single' | 'script';
  database_name: string;
  role_name: string;
  outcome: string;
  sqlstate: string;
  started_at: string;
  duration_ms: number | null;
  truncated: boolean;
  original_bytes: number;
  completion_unconfirmed: boolean;
};
export function QueryHistory({
  projectId,
  revision,
  onLoad,
}: {
  projectId: string;
  revision: number;
  onLoad: (sql: string, entry: Entry) => void;
}) {
  const epoch = useRef(0);
  const base = projectPath(projectId) + '/query-history';
  const [items, setItems] = useState<Entry[]>([]),
    [cursor, setCursor] = useState<string | null>(null),
    [enabled, setEnabled] = useState(false),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState('');
  useEffect(() => {
    epoch.current++;
    let cancelled = false;
    setItems([]);
    setCursor(null);
    setNotice('');
    setBusy(true);
    api<{ enabled: boolean; items: Entry[]; next_cursor: string | null }>(base)
      .then((v) => {
        if (!cancelled) {
          setItems(v.items);
          setCursor(v.next_cursor);
          setEnabled(v.enabled);
        }
      })
      .catch((e) => {
        if (!cancelled) setNotice(e instanceof Error ? e.message : '历史读取失败');
      })
      .finally(() => {
        if (!cancelled) setBusy(false);
      });
    return () => {
      cancelled = true;
    };
  }, [base, revision]);
  async function more() {
    if (!cursor) return;
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const v = await api<{ items: Entry[]; next_cursor: string | null }>(
        base + '?cursor=' + encodeURIComponent(cursor),
      );
      if (current !== epoch.current) return;
      setItems((old) => [...old, ...v.items]);
      setCursor(v.next_cursor);
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '历史读取失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function load(entry: Entry) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const detail = await api<Entry & { sql: string }>(base + '/' + entry.id);
      if (current !== epoch.current) return;
      onLoad(detail.sql, detail);
      setNotice(
        detail.truncated
          ? '历史仅保留前 9 KiB，已标记 QUERY TRUNCATED；请补全并审查后再执行。'
          : '已加载历史 SQL，连接密码与结果已清空；没有执行查询。',
      );
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '历史读取失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  const label = (entry: Entry) =>
    entry.completion_unconfirmed
      ? '结果未确认'
      : {
          admitted: '已受理 · 结果待确认',
          completed: '已完成',
          failed: 'SQL 失败',
          not_submitted: '未提交数据库',
          outcome_unknown: '结果未知',
        }[entry.outcome] || entry.outcome;
  return (
    <section
      className="table-card backend-panel"
      aria-label="我的 SQL 执行历史"
      data-testid="query-history-panel"
    >
      <div className="section-header">
        <h3>我的执行历史</h3>
        <span className="tag">个人 · 加密存储</span>
      </div>
      <p className="subtle-note">
        {enabled
          ? '记录本账户受理的 SQL 请求；仅自己可读。SQL 文本可能包含敏感常量。每项最多保留 9 KiB，不保存连接密码或结果。历史加载不会执行 SQL；未确认结果请核验原数据，不要盲目重发。'
          : '执行历史未由运维启用。启用后记录本账户的 SQL 请求，文本加密保存。'}
      </p>
      {enabled && (
        <>
          <details>
            <summary>管理本账号的历史采集与保留</summary>
            <HistoryPolicy />
          </details>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>时间 / Endpoint</th>
                  <th>数据库 / 角色</th>
                  <th>模式</th>
                  <th>状态</th>
                  <th>耗时</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {items.map((entry) => (
                  <tr key={entry.id} data-testid={'query-history-' + entry.id}>
                    <td>
                      {new Date(entry.started_at).toLocaleString()}
                      <br />
                      <code>{entry.endpoint_id}</code>
                    </td>
                    <td>
                      {entry.database_name}
                      <br />
                      {entry.role_name}
                    </td>
                    <td>
                      {entry.mode === 'script' ? '脚本' : '单条'}
                      {entry.truncated ? ' · 截断' : ''}
                    </td>
                    <td>
                      {label(entry)}
                      {entry.sqlstate && <small> · {entry.sqlstate}</small>}
                    </td>
                    <td>{entry.duration_ms === null ? '—' : entry.duration_ms + ' ms'}</td>
                    <td>
                      <button
                        className="button"
                        disabled={busy}
                        aria-label={'加载历史 ' + entry.id}
                        onClick={() => void load(entry)}
                      >
                        加载到编辑器
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {items.length === 0 && !busy && <p>尚无本账户的执行记录。</p>}
          {cursor && (
            <button className="button" disabled={busy} onClick={() => void more()}>
              加载更早历史
            </button>
          )}
        </>
      )}
      {notice && (
        <p role="status" data-testid="query-history-notice">
          {notice}
        </p>
      )}
    </section>
  );
}
