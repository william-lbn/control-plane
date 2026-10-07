import { useEffect, useRef, useState } from 'react';
import { api, projectPath, route } from '../../api';
import type { Operation } from '../../api';
import { followOperation } from '../../shared/followOperation';
import { newRequestKey } from '../../shared/requestKey';
import { fmt, status, PageHeading } from '../../shared/ui';
import { Operations } from '../operations/Operations';

type Resource = {
  id: string;
  name: string;
  state: string;
  protected: boolean;
  version: number;
  deleted_at: string | null;
};
type Snapshot = {
  project: Resource & { source: string; recover_until: string | null };
  branches: (Resource & {
    is_default: boolean;
    parent_branch_id: string | null;
    child_count: number;
    endpoint_count: number;
  })[];
  tombstones: {
    operation_id: string;
    resource_type: string;
    resource_id: string;
    deleted_at: string;
    recover_until: string | null;
    restored_at: string | null;
    physical_gc_state: string;
  }[];
  can_admin: boolean;
  can_edit: boolean;
  operations: Operation[];
};
type Action = {
  resource: Resource;
  kind: 'project' | 'branch';
  action: 'delete' | 'protect' | 'unprotect' | 'recover';
};
const labels = { delete: '删除', protect: '开启保护', unprotect: '解除保护', recover: '恢复项目' };

export function Lifecycle({ projectId }: { projectId: string }) {
  const [data, setData] = useState<Snapshot | null>(null);
  const [action, setAction] = useState<Action | null>(null);
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [operation, setOperation] = useState<Operation | null>(null);
  const abort = useRef<AbortController | null>(null);
  const base = projectPath(projectId);
  async function refresh() {
    setData(await api<Snapshot>(base + '/lifecycle'));
  }
  useEffect(() => {
    setData(null);
    setOperation(null);
    setError('');
    void refresh().catch((e: unknown) => setError(String(e)));
    const timer = setInterval(
      () => void refresh().catch((e: unknown) => setError(String(e))),
      15_000,
    );
    return () => {
      clearInterval(timer);
      abort.current?.abort();
    };
  }, [projectId]);
  function choose(resource: Resource, kind: Action['kind'], next: Action['action']) {
    setAction({ resource, kind, action: next });
    setConfirm('');
    setError('');
  }
  async function observe(op: Operation) {
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    const final = await followOperation<Operation>({
      id: op.id,
      signal: controller.signal,
      read: (signal) => api(base + '/operations/' + op.id, { signal }),
      onValue: setOperation,
      onUnavailable: () => setError('操作观察暂不可用，正在重试读取；请求没有重发。'),
    });
    await refresh();
    if (final?.state === 'succeeded') {
      setAction(null);
      setError('');
    } else if (final) setError(`${final.error_code}: ${final.error_message}`);
    else setError('观察超时，请从操作记录继续读取既有操作。');
  }
  async function submit() {
    if (!action || confirm !== action.resource.name) return;
    setBusy(true);
    setError('');
    setOperation(null);
    try {
      const path =
        base +
        (action.kind === 'branch' ? '/branches/' + encodeURIComponent(action.resource.id) : '');
      const protection = action.action === 'protect' || action.action === 'unprotect';
      const reply = await api<{ operation?: Operation }>(
        path + (protection ? '/protection' : action.action === 'recover' ? '/recover' : ''),
        {
          method: protection ? 'PATCH' : action.action === 'recover' ? 'POST' : 'DELETE',
          headers: {
            'If-Match': `"${action.resource.version}"`,
            'Idempotency-Key': newRequestKey(),
          },
          body: JSON.stringify({
            confirm_name: confirm,
            ...(protection ? { protected: action.action === 'protect' } : {}),
          }),
        },
      );
      if (reply.operation) await observe(reply.operation);
      else {
        await refresh();
        setAction(null);
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function retry() {
    if (!operation) return;
    setBusy(true);
    try {
      await api(base + '/operations/' + operation.id + '/retry', { method: 'POST' });
      await observe(operation);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeading
        kicker="PROJECT / LIFECYCLE"
        title="保护与删除"
        description="管理依赖关系、关闭访问与运行资源回收。项目删除后可在七天内恢复。"
      />
      {error && (
        <div className="inline-error" role="alert">
          {error}
        </div>
      )}
      {!data ? (
        <div className="skeleton" />
      ) : (
        <>
          <div className="notice">
            <span>◈</span>
            <div>
              <strong>保留数据的删除流程</strong>
              <p>
                删除会断开 SQL 和 Data API 连接并回收
                Compute，应用凭据永久撤销。项目可恢复，已单独删除的分支不会随项目恢复。原始
                Timeline、WAL、对象和审计记录保留；物理清除与跨实例连接栅栏仍待独立验收。
              </p>
            </div>
          </div>
          <section className="panel lifecycle-project" data-testid="project-lifecycle">
            <div className="panel-heading">
              <h2>{data.project.name}</h2>
              {status(data.project.state)}
            </div>
            <p>{data.project.protected ? '项目保护已开启' : '项目保护已解除'}</p>
            {data.project.recover_until && <p>恢复期限：{fmt(data.project.recover_until)}</p>}
            {data.project.state === 'ready' &&
              data.can_admin &&
              data.project.source === 'managed' && (
                <div className="form-actions">
                  <button
                    className="button"
                    onClick={() =>
                      choose(
                        data.project,
                        'project',
                        data.project.protected ? 'unprotect' : 'protect',
                      )
                    }
                  >
                    {data.project.protected ? '解除项目保护' : '保护项目'}
                  </button>
                  <button
                    className="button danger"
                    disabled={
                      data.project.protected ||
                      data.branches.some((b) => !b.deleted_at && b.protected)
                    }
                    onClick={() => choose(data.project, 'project', 'delete')}
                  >
                    删除项目
                  </button>
                </div>
              )}
            {data.project.state === 'deleted' && data.can_admin && (
              <button
                className="button primary"
                disabled={
                  !data.project.recover_until ||
                  Date.parse(data.project.recover_until) <= Date.now()
                }
                onClick={() => choose(data.project, 'project', 'recover')}
              >
                恢复项目
              </button>
            )}
            {data.project.state === 'ready' && (
              <a className="button" href={route(projectId)}>
                打开项目 ↗
              </a>
            )}
          </section>
          <section className="panel">
            <div className="panel-heading">
              <h2>分支依赖图</h2>
              <span>子分支须先删除 · 根分支不可单独删除</span>
            </div>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>分支</th>
                    <th>父分支</th>
                    <th>运行与依赖</th>
                    <th>状态</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {data.branches.map((b) => (
                    <tr key={b.id} data-testid={`lifecycle-${b.id}`}>
                      <td>
                        <strong>{b.name}</strong>
                        <small>
                          {b.id}
                          {b.is_default ? ' · 根分支' : ''}
                        </small>
                      </td>
                      <td>{data.branches.find((x) => x.id === b.parent_branch_id)?.name || '—'}</td>
                      <td>
                        {b.endpoint_count} Compute · {b.child_count} 子分支
                        {b.protected ? ' · 已保护' : ''}
                      </td>
                      <td>{status(b.state)}</td>
                      <td>
                        {!b.deleted_at && b.state === 'ready' && data.project.state === 'ready' && (
                          <div className="form-actions">
                            {data.can_admin && (
                              <button
                                className="button"
                                onClick={() =>
                                  choose(b, 'branch', b.protected ? 'unprotect' : 'protect')
                                }
                              >
                                {b.protected ? '解除分支保护' : '保护分支'}
                              </button>
                            )}
                            <button
                              className="button danger"
                              disabled={
                                !data.can_edit || b.is_default || b.protected || b.child_count > 0
                              }
                              onClick={() => choose(b, 'branch', 'delete')}
                            >
                              删除分支
                            </button>
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>
          <section className="panel">
            <div className="panel-heading">
              <h2>删除与恢复记录</h2>
              <span>数据保留 · GC held</span>
            </div>
            {data.tombstones.length === 0 ? (
              <p className="muted">尚无删除记录。</p>
            ) : (
              data.tombstones.map((t) => (
                <div className="list-row" key={t.operation_id}>
                  <div>
                    <strong>
                      {t.resource_type} · {t.resource_id}
                    </strong>
                    <small>
                      {fmt(t.deleted_at)} · {t.restored_at ? '已恢复' : '已关闭'} · 物理数据保留
                    </small>
                  </div>
                  <button
                    className="button"
                    onClick={() =>
                      void api<Operation>(base + '/operations/' + t.operation_id)
                        .then(setOperation)
                        .catch((e) => setError(String(e)))
                    }
                  >
                    {t.operation_id}
                  </button>
                </div>
              ))
            )}
          </section>
          <Operations
            projectId={projectId}
            operations={data.operations}
            canEdit={data.can_admin || (data.project.state === 'ready' && data.can_edit)}
            onChanged={refresh}
            showError={(e) => setError(String(e))}
          />
        </>
      )}
      {operation && (
        <section className="panel lifecycle-operation" aria-live="polite">
          <div className="panel-heading">
            <h2>生命周期操作</h2>
            {status(operation.state)}
          </div>
          <small>{operation.id}</small>
          {operation.steps.map((step) => (
            <div className="list-row" key={step.ordinal}>
              <span>{step.name}</span>
              {status(step.state)}
            </div>
          ))}
          {operation.state === 'failed' && operation.retryable && (
            <button className="button" disabled={busy} onClick={() => void retry()}>
              重试既有操作
            </button>
          )}
        </section>
      )}
      {action && (
        <div className="modal-backdrop">
          <section
            className="modal lifecycle-modal"
            role="dialog"
            aria-modal="true"
            aria-label="确认生命周期操作"
          >
            <h2>
              {labels[action.action]}：{action.resource.name}
            </h2>
            <p>
              {action.action === 'delete'
                ? '确认后将关闭访问并断开此资源的连接。请先停止应用写入。'
                : '请核对资源名称和当前状态。'}
            </p>
            <label>
              输入完整名称确认
              <input
                aria-label="资源名称确认"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                disabled={busy}
                autoComplete="off"
              />
            </label>
            <div className="form-actions">
              <button className="button" disabled={busy} onClick={() => setAction(null)}>
                取消
              </button>
              <button
                className="button primary"
                disabled={busy || confirm !== action.resource.name}
                onClick={() => void submit()}
              >
                {busy ? '操作进行中…' : '确认执行'}
              </button>
            </div>
          </section>
        </div>
      )}
    </>
  );
}
