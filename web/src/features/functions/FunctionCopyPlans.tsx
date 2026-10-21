import { useEffect, useRef, useState } from 'react';
import { api, projectPath, route } from '../../api';

type Plan = {
  slug: string;
  source_branch_id: string;
  source_function_id: string;
  source_deployment_id: string | null;
  source_version_token: string;
  preparation_state: string;
  function_state: string | null;
  function_id: string | null;
  operation_id: string | null;
  snapshot_at: string;
  last_error_code: string | null;
};
const labels: Record<string, string> = {
  operator_disabled: '等待启用准备服务',
  waiting_for_writer: '等待此分支的 Writer',
  pending: '等待准备',
  blocked: '准备前置条件未满足',
  definition_only: '定义已复制，等待首次部署',
  queued: '准备已排队',
  running: '正在准备',
  failed: '准备失败，查看原操作',
  succeeded: '已准备暂停版本',
};
export function FunctionCopyPlans({
  projectId,
  branch,
  onRefreshed,
}: {
  projectId: string;
  branch: string;
  onRefreshed: () => void;
}) {
  const [items, setItems] = useState<Plan[]>([]),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const epoch = useRef(0);
  async function refresh() {
    const current = ++epoch.current;
    setBusy(true);
    setError('');
    try {
      const data = await api<{ items: Plan[] }>(
        `${projectPath(projectId)}/branches/${branch}/function-copy-plans`,
      );
      if (current === epoch.current) setItems(data.items);
    } catch (e) {
      if (current === epoch.current) setError(String(e));
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  useEffect(() => {
    void refresh();
    return () => {
      ++epoch.current;
    };
  }, [projectId, branch]);
  return (
    <section className="panel function-authoring" aria-label="分支函数准备">
      <h2>分支继承的函数</h2>
      <p className="muted">
        来源版本在创建分支时固定。SQL
        分支准备完成与函数准备分别显示；函数准备完成后仍暂停，公开访问、日志采集和自动执行需要单独授权。历史
        SQL 数据点不会被当作代码版本。
      </p>
      <button
        disabled={busy}
        onClick={() => {
          void refresh().then(onRefreshed);
        }}
      >
        刷新分支函数准备
      </button>
      {error && <p role="alert">{error}</p>}
      {!items.length && !busy && !error && <p>没有此分支的函数继承记录。</p>}
      {items.map((p) => (
        <article key={p.source_function_id} data-testid="function-copy-plan">
          <h3>{p.slug}</h3>
          <p>
            {p.function_state === 'deleted'
              ? '已退休的复制记录'
              : labels[p.preparation_state] || p.preparation_state}
          </p>
          <p>
            原分支 <code>{p.source_branch_id}</code> · 元数据版本 {p.source_version_token}
          </p>
          <p>
            固定代码 <code>{p.source_deployment_id || '尚无已发布代码，仅复制定义'}</code>
          </p>
          <p>快照时间 {new Date(p.snapshot_at).toLocaleString()}</p>
          {p.function_id && (
            <p>
              目标函数 <code>{p.function_id}</code>
            </p>
          )}
          {p.last_error_code && (
            <p role="status">{p.last_error_code}。修复配额或前置条件后会继续；原分支数据保留。</p>
          )}
          {p.operation_id && (
            <a href={route(projectId, 'operations')}>查看原操作 {p.operation_id}</a>
          )}
        </article>
      ))}
    </section>
  );
}
