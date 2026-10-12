import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Page, Operation } from '../../api';
import { PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { route } from '../../api';
import './functions.css';
import './authoring.css';
import { RuntimePanel } from './RuntimePanel';

type FunctionDefinition = {
  id: string;
  slug: string;
  branch_id: string;
  database_name: string;
  sql_schema: string;
  state: string;
  version: number;
  version_token: string;
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

// Definition intent is metadata; an active definition is not an observed Node.
const definitionLabel: Record<string, string> = {
  provisioning: '定义创建中',
  active: '定义已激活',
  suspended: '定义已休眠',
  degraded: '定义需恢复',
  deleting: '定义删除中',
  deleted: '定义已删除',
};
const deploymentLabel: Record<string, string> = {
  pending: '部署待处理',
  building: '部署构建中',
  completed: '部署已完成',
  failed: '部署失败',
};

// Read-only branch metadata is useful independently of execution admission.
// Never manufacture an invocation URL, infer Node health or show env values.
// Customer response/code execution does not belong to the Console origin.
export function Functions({
  projectId,
  branches,
  canEdit,
}: {
  projectId: string;
  branches: Branch[];
  canEdit: boolean;
}) {
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
  const [driver, setDriver] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState('');
  const [operation, setOperation] = useState<Operation | null>(null);
  const [slug, setSlug] = useState('');
  const [database, setDatabase] = useState('neondb');
  const [schema, setSchema] = useState('app');
  const [zip, setZip] = useState<File | null>(null);
  const [environment, setEnvironment] = useState('');
  const [invokePath, setInvokePath] = useState('/');
  const [invokeMethod, setInvokeMethod] = useState('GET');
  const [invokeBody, setInvokeBody] = useState('');
  const [invokeResult, setInvokeResult] = useState('');
  const actionEpoch = useRef(0);
  const historyEpoch = useRef(0);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/functions`;
  useEffect(() => {
    setEnvironment('');
    setZip(null);
    setInvokeBody('');
    setOperation(null);
  }, [projectId, branch]);
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
    setDriver(false);
    setBusy(false);
    setActionError('');
    setInvokeResult('');
    actionEpoch.current++;
    if (!branch) {
      setLoading(false);
      return () => controller.abort();
    }
    api<Page<FunctionDefinition> & { driver_enabled: boolean }>(path + '?limit=100', {
      signal: controller.signal,
    })
      .then((data) => {
        if (disposed) return;
        setItems(data.items);
        setDriver(data.driver_enabled);
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
  async function waitOperation(op: Operation, epoch: number) {
    setOperation(op);
    for (let attempt = 0; attempt < 240 && actionEpoch.current === epoch; attempt++) {
      if (op.state === 'succeeded') return;
      if (op.state === 'failed' || op.state === 'cancelled')
        throw new Error(op.error_message || '操作失败，请查看原 Operation');
      await new Promise((resolve) => setTimeout(resolve, 2500));
      op = await api<Operation>(`${projectPath(projectId)}/operations/${op.id}`);
      if (actionEpoch.current === epoch) setOperation(op);
    }
    throw new Error('已受理，请在 Operations 查看原任务；不要重复提交');
  }
  async function mutate(action: () => Promise<{ operation: Operation }>) {
    const epoch = actionEpoch.current;
    setBusy(true);
    setActionError('');
    try {
      const accepted = await action();
      if (actionEpoch.current !== epoch) return;
      await waitOperation(accepted.operation, epoch);
      if (actionEpoch.current === epoch) setRefresh((v) => v + 1);
    } catch (e) {
      if (actionEpoch.current === epoch) setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      if (actionEpoch.current === epoch) setBusy(false);
    }
  }
  async function create() {
    await mutate(() =>
      api(path, {
        method: 'POST',
        headers: { 'Idempotency-Key': newRequestKey() },
        body: JSON.stringify({ slug, database_name: database, sql_schema: schema }),
      }),
    );
  }
  async function deploy() {
    if (!current) return;
    let values: unknown;
    try {
      if (environment.trim()) values = JSON.parse(environment);
    } catch {
      setActionError('环境变量须为 JSON 字符串字典');
      return;
    }
    const form = new FormData();
    form.set('runtime', 'nodejs24');
    if (values !== undefined) form.set('environment', JSON.stringify(values));
    if (zip) form.set('zip', zip);
    await mutate(() =>
      api(`${path}/${current.slug}/deployments`, {
        method: 'POST',
        headers: {
          'Idempotency-Key': newRequestKey(),
          'If-Match': `"${current.version_token}"`,
        },
        body: form,
      }),
    );
  }
  async function stop(deleting: boolean) {
    if (
      !current ||
      !window.confirm(
        `${deleting ? '删除' : '停止'} ${current.slug}？当前进程及后台任务将结束；数据与历史保留。`,
      )
    )
      return;
    await mutate(() =>
      api(`${path}/${current.slug}${deleting ? '' : '/suspend'}`, {
        method: deleting ? 'DELETE' : 'POST',
        headers: {
          'Idempotency-Key': newRequestKey(),
          'If-Match': `"${current.version_token}"`,
        },
        body: JSON.stringify({ acknowledge_background_work_stop: true }),
      }),
    );
  }
  async function invoke() {
    if (!current) return;
    const epoch = actionEpoch.current;
    setBusy(true);
    setActionError('');
    setInvokeResult('');
    try {
      const result = await api<{
        status_code: number;
        body: string;
        headers: Record<string, string[]>;
      }>(`${path}/${current.slug}/console-invoke`, {
        method: 'POST',
        body: JSON.stringify({
          method: invokeMethod,
          path: invokePath,
          headers: {},
          body: invokeBody,
        }),
      });
      const bytes = Uint8Array.from(atob(result.body), (c) => c.charCodeAt(0));
      if (epoch === actionEpoch.current)
        setInvokeResult(`HTTP ${result.status_code}\n${new TextDecoder().decode(bytes)}`);
    } catch (e) {
      if (epoch === actionEpoch.current) setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      if (epoch === actionEpoch.current) setBusy(false);
    }
  }
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
        <strong>{driver ? '原生 microVM 函数执行预览' : '此集群尚未启用函数部署与调用'}</strong>
        <p>
          {driver
            ? '支持不可变 ZIP、环境版本、受控调用和手动停止。公开 HTTP/SSE/WS、自动休眠与生产准入仍需独立验收。数据库 owner 须明确授权；此页面不代替授权修改共享 PUBLIC 权限。'
            : '当前可查看已保存的定义和历史。部署、公开访问、数据库授权及休眠唤醒需通过独立验收后开放。'}
        </p>
      </section>
      {driver && canEdit && (
        <section className="panel function-authoring">
          <h2>创建函数</h2>
          <div className="function-form-grid">
            <label>
              函数名称
              <input
                aria-label="函数名称"
                value={slug}
                onChange={(e) => setSlug(e.target.value)}
                maxLength={20}
                placeholder="hello"
              />
            </label>
            <label>
              函数数据库
              <input
                aria-label="函数数据库"
                value={database}
                onChange={(e) => setDatabase(e.target.value)}
              />
            </label>
            <label>
              授权 Schema
              <input
                aria-label="授权 Schema"
                value={schema}
                onChange={(e) => setSchema(e.target.value)}
              />
            </label>
          </div>
          <button
            className="button primary"
            disabled={busy || !slug || !branch}
            onClick={() => void create()}
          >
            创建函数定义
          </button>
        </section>
      )}
      {actionError && (
        <div className="error-box" role="alert">
          {actionError}
        </div>
      )}
      {operation && (
        <p role="status">
          Operation {operation.id} · {operation.state}{' '}
          <a href={route(projectId, 'operations')}>查看操作记录</a>
        </p>
      )}
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
                    {status(item.state, definitionLabel[item.state] || '未知定义状态')}
                  </button>
                ))}
              </div>
            </section>
            {current && (
              <section className="panel" data-testid="function-detail">
                <div className="function-detail-heading">
                  <h2>{current.slug}</h2>
                  {status(current.state, definitionLabel[current.state] || '未知定义状态')}
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
                <RuntimePanel
                  key={`${path}/${current.slug}/${current.version_token}`}
                  path={`${path}/${current.slug}`}
                  version={current.version_token}
                  disabled={busy}
                />
                {driver && canEdit && (
                  <section className="function-authoring">
                    <h3>代码与环境版本</h3>
                    <p className="muted">
                      ZIP 须包含 index.mjs 或 index.js。停止原实例后可更新部署；不选 ZIP
                      时复用已完成版本；首次部署失败须先停止并回收原实例，再复用保留代码恢复。
                      环境变量留空或 {} 保留上次版本；将某个变量设为 ""
                      可删除该变量。已保存密钥不回显。
                    </p>
                    <label>
                      函数 ZIP
                      <input
                        aria-label="函数 ZIP"
                        type="file"
                        accept=".zip"
                        onChange={(e) => setZip(e.target.files?.[0] || null)}
                      />
                    </label>
                    <label>
                      环境变量 JSON
                      <textarea
                        aria-label="环境变量 JSON"
                        value={environment}
                        onChange={(e) => setEnvironment(e.target.value)}
                        spellCheck={false}
                      />
                    </label>
                    <div className="function-actions">
                      <button
                        className="button primary"
                        disabled={
                          busy || current.state === 'active' || current.state === 'deleting'
                        }
                        onClick={() => void deploy()}
                      >
                        部署函数版本
                      </button>
                      <button
                        className="button"
                        disabled={busy || current.state === 'deleting'}
                        onClick={() => void stop(false)}
                      >
                        停止函数实例
                      </button>
                      <button
                        className="button danger"
                        disabled={busy || current.state === 'deleting'}
                        onClick={() => void stop(true)}
                      >
                        删除函数
                      </button>
                    </div>
                    <h3>受控调用</h3>
                    <div className="function-form-grid">
                      <label>
                        调用方法
                        <select
                          aria-label="调用方法"
                          value={invokeMethod}
                          onChange={(e) => setInvokeMethod(e.target.value)}
                        >
                          {['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'].map(
                            (method) => (
                              <option key={method}>{method}</option>
                            ),
                          )}
                        </select>
                      </label>
                      <label>
                        调用路径
                        <input
                          aria-label="调用路径"
                          value={invokePath}
                          onChange={(e) => setInvokePath(e.target.value)}
                        />
                      </label>
                    </div>
                    <label>
                      调用请求体
                      <textarea
                        aria-label="调用请求体"
                        value={invokeBody}
                        onChange={(e) => setInvokeBody(e.target.value)}
                      />
                    </label>
                    <button
                      className="button primary"
                      disabled={busy || current.state !== 'active'}
                      onClick={() => void invoke()}
                    >
                      调用函数
                    </button>
                    {invokeResult && (
                      <pre data-testid="function-invocation-result">{invokeResult}</pre>
                    )}
                  </section>
                )}
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
                        {status(d.state, deploymentLabel[d.state] || '未知部署状态')}
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
