import { useEffect, useState } from 'react';
import { api, projectPath } from '../../api';
type Saved = { id: string; name: string; generation: number; updated_at: string };
export function SavedQueries({
  projectId,
  sql,
  onLoad,
}: {
  projectId: string;
  sql: string;
  onLoad: (sql: string) => void;
}) {
  const base = projectPath(projectId) + '/saved-queries';
  const [items, setItems] = useState<Saved[]>([]);
  const [selected, setSelected] = useState('');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function refresh() {
    const result = await api<{ items: Saved[] }>(base);
    setItems(result.items);
  }
  useEffect(() => {
    let cancelled = false;
    setItems([]);
    setSelected('');
    setName('');
    setError('');
    api<{ items: Saved[] }>(base)
      .then((v) => {
        if (!cancelled) setItems(v.items);
      })
      .catch((e) => {
        if (!cancelled) setError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [base]);
  async function change(action: 'create' | 'update' | 'delete' | 'load') {
    setBusy(true);
    setError('');
    const current = items.find((v) => v.id === selected);
    try {
      if (action === 'load') {
        const v = await api<Saved & { sql: string }>(base + '/' + selected);
        onLoad(v.sql);
        setName(v.name);
        return;
      }
      if (action === 'delete' && !confirm('删除这条个人保存查询？不会执行 SQL 或删除数据库数据。'))
        return;
      const path = action === 'create' ? base : base + '/' + selected;
      const result = await api<{ query: Saved }>(path, {
        method: action === 'create' ? 'POST' : action === 'update' ? 'PATCH' : 'DELETE',
        headers: {
          'Idempotency-Key': crypto.randomUUID(),
          ...(current ? { 'If-Match': `"${current.generation}"` } : {}),
        },
        ...(action !== 'delete' ? { body: JSON.stringify({ name, sql }) } : {}),
      });
      setSelected(action === 'delete' ? '' : result.query.id);
      if (action === 'delete') setName('');
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="saved-query-panel" aria-label="个人保存查询">
      <div className="panel-heading">
        <h2>保存查询</h2>
        <span className="tag">个人 · 加密存储</span>
      </div>
      <div className="saved-query-controls">
        <label>
          已保存
          <select
            aria-label="已保存查询"
            disabled={busy}
            value={selected}
            onChange={(e) => {
              setSelected(e.target.value);
              setName(items.find((v) => v.id === e.target.value)?.name || '');
            }}
          >
            <option value="">选择个人查询</option>
            {items.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          名称
          <input
            aria-label="保存查询名称"
            value={name}
            maxLength={100}
            disabled={busy}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
      </div>
      <div className="saved-query-actions">
        <button className="button" disabled={busy || !selected} onClick={() => void change('load')}>
          加载到编辑器
        </button>
        <button
          className="button primary"
          disabled={busy || !name.trim() || !sql.trim()}
          onClick={() => void change('create')}
        >
          另存为新查询
        </button>
        <button
          className="button"
          disabled={busy || !selected || !name.trim() || !sql.trim()}
          onClick={() => void change('update')}
        >
          更新保存查询
        </button>
        <button
          className="button"
          disabled={busy || !selected}
          onClick={() => void change('delete')}
        >
          删除保存查询
        </button>
      </div>
      {error && (
        <p role="alert" data-testid="saved-query-error">
          {error}
        </p>
      )}
      <p className="subtle-note">
        只保存 SQL；连接密码与结果不会保存。加载不会执行或唤醒 Compute。每个项目最多 50 条个人查询。
      </p>
    </section>
  );
}
