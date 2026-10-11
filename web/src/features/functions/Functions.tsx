import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Page } from '../../api';
import { PageHeading, status } from '../../shared/ui';
import { route } from '../../api';
import './functions.css';

type FunctionDefinition = {
  id: string;
  slug: string;
  branch_id: string;
  database_name: string;
  sql_schema: string;
  state: string;
  version: number;
  generation: number;
  active_deployment_id: string | null;
  target_deployment_id: string | null;
  invocation_url: string | null;
  runtime_observed: boolean;
};
type Deployment = {
  id: string;
  runtime: string;
  state: string;
  bundle_digest: string;
  bundle_bytes: number;
  environment_names: string[];
  operation_id: string;
  failure_code: string | null;
  created_at: string;
  finished_at: string | null;
};

// Read-only branch metadata is useful independently of execution admission.
// Never manufacture an invocation URL, infer Node health or show env values.
// Customer response/code execution does not belong to the Console origin.
export function Functions({ projectId, branches }: { projectId: string; branches: Branch[] }) {
  const [branch, setBranch] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [items, setItems] = useState<FunctionDefinition[]>([]);
  const [selected, setSelected] = useState('');
  const [deployments, setDeployments] = useState<Deployment[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [historyLoading, setHistoryLoading] = useState(false);
  const [error, setError] = useState('');
  const [historyError, setHistoryError] = useState('');
  const [refresh, setRefresh] = useState(0);
  const [observedAt, setObservedAt] = useState('');
  const historyEpoch = useRef(0);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/functions`;
  useEffect(() => {
    const controller = new AbortController();
    let disposed = false;
    setItems([]);
    setSelected('');
    setDeployments([]);
    setCursor(null);
    setError('');
    setHistoryError('');
    setLoading(true);
    setObservedAt('');
    if (!branch) {
      setLoading(false);
      return () => controller.abort();
    }
    api<Page<FunctionDefinition>>(path + '?limit=100', { signal: controller.signal })
      .then((data) => {
        if (disposed) return;
        setItems(data.items);
        setObservedAt(new Date().toLocaleTimeString());
        setSelected(data.items[0]?.slug || '');
      })
      .catch((e) => {
        if (!disposed) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!disposed) setLoading(false);
      });
    return () => {
      disposed = true;
      controller.abort();
      historyEpoch.current++;
    };
  }, [projectId, branch, refresh]);
  useEffect(() => {
    const epoch = ++historyEpoch.current;
    const controller = new AbortController();
    setDeployments([]);
    setCursor(null);
    setHistoryError('');
    setHistoryLoading(!!selected);
    if (selected)
      api<Page<Deployment>>(`${path}/${selected}/deployments?limit=20`, {
        signal: controller.signal,
      })
        .then((data) => {
          if (epoch === historyEpoch.current) {
            setDeployments(data.items);
            setCursor(data.next_cursor);
          }
        })
        .catch((e) => {
          if (epoch === historyEpoch.current)
            setHistoryError(e instanceof Error ? e.message : String(e));
        })
        .finally(() => {
          if (epoch === historyEpoch.current) setHistoryLoading(false);
        });
    return () => {
      controller.abort();
      historyEpoch.current++;
    };
  }, [path, selected]);
  async function more() {
    if (!cursor || historyLoading) return;
    const epoch = historyEpoch.current;
    setHistoryLoading(true);
    setHistoryError('');
    try {
      const data = await api<Page<Deployment>>(
        `${path}/${selected}/deployments?limit=20&after=${encodeURIComponent(cursor)}`,
      );
      if (epoch === historyEpoch.current) {
        setDeployments((old) => [...old, ...data.items]);
        setCursor(data.next_cursor);
      }
    } catch (e) {
      if (epoch === historyEpoch.current)
        setHistoryError(e instanceof Error ? e.message : String(e));
    } finally {
      if (epoch === historyEpoch.current) setHistoryLoading(false);
    }
  }
  const current = items.find((item) => item.slug === selected);
  return (
    <div data-testid="functions-page">
      <PageHeading
        kicker="BRANCH BACKEND"
        title="Functions"
        description="查看分支函数及不可变部署历史。运行观测与部署状态分别展示。"
      />
      <section className="panel function-scope">
        <label>
          Functions 分支
          <select
            aria-label="Functions 分支"
            value={branch}
            onChange={(e) => setBranch(e.target.value)}
          >
            {branches.map((b) => (
              <option key={b.id} value={b.id}>
                {b.name}
              </option>
            ))}
          </select>
        </label>
        <button
          className="button"
          disabled={loading || !branch}
          onClick={() => setRefresh((v) => v + 1)}
        >
          刷新函数状态
        </button>
        {observedAt && <span className="muted">元数据读取于 {observedAt}</span>}
      </section>
      <section className="function-admission" role="status">
        <strong>此集群尚未启用函数部署与调用</strong>
        <p>
          当前可查看已保存的定义和历史。部署、公开访问、数据库授权及休眠唤醒需通过独立验收后开放。
        </p>
      </section>
      {error && (
        <div role="alert" className="error-box">
          {error} · 请刷新后重试
        </div>
      )}
      {loading ? (
        <div className="skeleton" />
      ) : !error && !items.length ? (
        <section className="panel function-empty">
          <span>ƒ</span>
          <h2>这个分支还没有函数</h2>
          <p>函数属于指定分支。当前页面不会创建实例或唤醒数据库。</p>
        </section>
      ) : (
        !error && (
          <div className="function-workspace">
            <section className="panel">
              <h2>函数 · {items.length}</h2>
              <div className="function-list">
                {items.map((item) => (
                  <button
                    key={item.id}
                    className={item.slug === selected ? 'selected' : ''}
                    aria-label={`查看函数 ${item.slug}`}
                    aria-pressed={item.slug === selected}
                    onClick={() => setSelected(item.slug)}
                  >
                    <span className="function-icon">ƒ</span>
                    <span>
                      <strong>{item.slug}</strong>
                      <small>
                        {item.database_name} / {item.sql_schema}
                      </small>
                    </span>
                    {status(item.state)}
                  </button>
                ))}
              </div>
            </section>
            {current && (
              <section className="panel" data-testid="function-detail">
                <div className="function-detail-heading">
                  <h2>{current.slug}</h2>
                  {status(current.state)}
                </div>
                <dl className="function-facts">
                  <div>
                    <dt>定义版本</dt>
                    <dd>{current.version}</dd>
                  </div>
                  <div>
                    <dt>调谐代次</dt>
                    <dd>{current.generation}</dd>
                  </div>
                  <div>
                    <dt>运行状态</dt>
                    <dd>{current.runtime_observed ? '已有观测' : '尚未观测'}</dd>
                  </div>
                  <div>
                    <dt>公开地址</dt>
                    <dd>{current.invocation_url || '尚未开放'}</dd>
                  </div>
                </dl>
                <h3>部署历史</h3>
                <p className="muted">
                  密钥只显示变量名称。历史按稳定部署 ID 分页；创建时间单独列出。
                </p>
                {historyError && <p role="alert">{historyError}</p>}
                <div className="function-deployments">
                  {deployments.map((d) => (
                    <article key={d.id} data-testid="function-deployment">
                      <div>
                        <code>{d.id}</code>
                        {status(d.state)}
                      </div>
                      <p>
                        {d.runtime} · {(d.bundle_bytes / 1024).toFixed(1)} KiB ·{' '}
                        {new Date(d.created_at).toLocaleString()}
                      </p>
                      <div className="function-digest" title={d.bundle_digest}>
                        SHA256 {d.bundle_digest}
                      </div>
                      <p>环境变量：{d.environment_names.join(', ') || '无'}</p>
                      <p>
                        {d.id === current.active_deployment_id && <span>当前接受版本 · </span>}
                        {d.id === current.target_deployment_id && <span>目标版本 · </span>}
                        <a href={route(projectId, 'operations')}>查看 Operation {d.operation_id}</a>
                      </p>
                      {d.failure_code && <p role="alert">错误码：{d.failure_code}</p>}
                    </article>
                  ))}
                </div>
                {historyLoading && <p role="status">正在读取部署历史…</p>}
                {cursor && (
                  <button className="button" disabled={historyLoading} onClick={() => void more()}>
                    加载更多部署
                  </button>
                )}
                {!historyLoading && !historyError && !deployments.length && (
                  <p className="muted">暂无部署记录</p>
                )}
              </section>
            )}
          </div>
        )
      )}
    </div>
  );
}
