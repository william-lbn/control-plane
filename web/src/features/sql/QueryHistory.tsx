import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import { HistoryPolicy } from '../identity/HistoryPolicy';
import { HistorySharingPanel, SharedHistory } from './SharedHistory';
import { HistoryNamePanel } from './HistoryNames';
export type Entry = {
  id: string;
  endpoint_id: string;
  branch_id: string;
  mode: 'single' | 'script';
  database_name: string;
  role_name: string;
  outcome: string;
  sqlstate: string;
  started_at: string;
  finished_at: string | null;
  duration_ms: number | null;
  truncated: boolean;
  original_bytes: number;
  completion_unconfirmed: boolean;
  shared_at: string | null;
  sharing_version_token: string;
  name: string | null;
  name_version_token: string;
  name_updated_at: string | null;
};
export type HistoryLoadTarget = Pick<Entry, 'endpoint_id' | 'database_name' | 'role_name' | 'mode'>;
export function QueryHistory({
  projectId,
  revision,
  onLoad,
}: {
  projectId: string;
  revision: number;
  onLoad: (sql: string, entry: HistoryLoadTarget) => void;
}) {
  const epoch = useRef(0);
  const base = projectPath(projectId) + '/query-history';
  const [items, setItems] = useState<Entry[]>([]),
    [cursor, setCursor] = useState<string | null>(null),
    [enabled, setEnabled] = useState(false),
    [busy, setBusy] = useState(false),
    [deleting, setDeleting] = useState<string | null>(null),
    [sharing, setSharing] = useState<Entry | null>(null),
    [naming, setNaming] = useState<Entry | null>(null),
    [notice, setNotice] = useState('');
  useEffect(() => {
    epoch.current++;
    let cancelled = false;
    setItems([]);
    setCursor(null);
    setNotice('');
    setDeleting(null);
    setSharing(null);
    setNaming(null);
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
  const canDelete = (entry: Entry) =>
    !!entry.finished_at && ['completed', 'failed', 'not_submitted'].includes(entry.outcome);
  async function remove(id: string) {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      await api(base + '/' + encodeURIComponent(id) + '?acknowledge_history_deletion=true', {
        method: 'DELETE',
      });
      if (current !== epoch.current) return;
      setItems((old) => old.filter((item) => item.id !== id));
      setDeleting(null);
      setNotice('已删除本人的历史记录；数据库执行结果和当前编辑器内容没有改变。');
    } catch (e) {
      if (current === epoch.current)
        setNotice(e instanceof Error ? e.message : '删除结果未确认；请重载历史核对原记录。');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
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
          {sharing && (
            <HistorySharingPanel
              key={sharing.id}
              projectId={projectId}
              historyId={sharing.id}
              onClose={() => setSharing(null)}
              onChanged={(value) =>
                setItems((old) =>
                  old.map((item) =>
                    item.id === value.history_id
                      ? {
                          ...item,
                          shared_at: value.shared_at,
                          sharing_version_token: value.version_token,
                        }
                      : item,
                  ),
                )
              }
            />
          )}
          {naming && (
            <HistoryNamePanel
              key={naming.id}
              projectId={projectId}
              entry={naming}
              onClose={() => setNaming(null)}
              onChanged={(value) =>
                setItems((old) =>
                  old.map((item) =>
                    item.id === value.history_id
                      ? {
                          ...item,
                          name: value.name,
                          name_version_token: value.version_token,
                          name_updated_at: value.updated_at,
                        }
                      : item,
                  ),
                )
              }
            />
          )}
          {deleting && (
            <div className="notice" role="group" aria-label="确认删除历史记录">
              <p>
                确认删除本人记录 <code>{deleting}</code>？这仅移除元数据库中的历史文本和详情，
                不撤销已执行 SQL，不清空编辑器。备份与 WAL 遵循各自保留策略。
              </p>
              <button className="button" disabled={busy} onClick={() => void remove(deleting)}>
                确认删除此历史
              </button>
              <button className="button" disabled={busy} onClick={() => setDeleting(null)}>
                取消删除历史
              </button>
            </div>
          )}
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
                      {entry.name && (
                        <strong data-testid={'history-name-' + entry.id}>
                          {entry.name}
                          <br />
                        </strong>
                      )}
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
                        disabled={busy || !canDelete(entry)}
                        aria-label={'管理历史名称 ' + entry.id}
                        onClick={() => {
                          setSharing(null);
                          setDeleting(null);
                          setNaming(entry);
                        }}
                      >
                        名称
                      </button>
                      <button
                        className="button"
                        disabled={busy}
                        aria-label={'加载历史 ' + entry.id}
                        onClick={() => void load(entry)}
                      >
                        加载到编辑器
                      </button>
                      <button
                        className="button"
                        disabled={busy || !canDelete(entry)}
                        aria-label={'管理历史共享 ' + entry.id}
                        onClick={() => {
                          setDeleting(null);
                          setSharing(entry);
                        }}
                      >
                        {entry.shared_at ? '已共享 · 管理' : '共享到项目'}
                      </button>
                      <button
                        className="button"
                        disabled={busy || !canDelete(entry)}
                        aria-label={'删除历史 ' + entry.id}
                        title={
                          canDelete(entry) ? '删除本人历史记录' : '保留进行中或结果未确认的记录'
                        }
                        onClick={() => setDeleting(entry.id)}
                      >
                        删除记录
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
      {enabled && <SharedHistory key={projectId} projectId={projectId} onLoad={onLoad} />}
    </section>
  );
}
