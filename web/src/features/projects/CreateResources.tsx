import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Operation } from '../../api';
import { newRequestKey } from '../../shared/requestKey';
import { followOperation } from '../../shared/followOperation';

type Kind = 'project' | 'branch' | 'endpoint';
type Bounds = {
  min_cpu_milli: number;
  max_cpu_milli: number;
  min_memory_mib: number;
  max_memory_mib: number;
};

const initialBounds: Bounds = {
  min_cpu_milli: 1000,
  max_cpu_milli: 2000,
  min_memory_mib: 1024,
  max_memory_mib: 3072,
};

export function CreateResource({
  kind,
  organizationId = 'local',
  projectId,
  branches = [],
  branchId,
  writerExists = false,
  onDone,
  onClose,
}: {
  kind: Kind;
  organizationId?: string;
  projectId?: string;
  branches?: Branch[];
  branchId?: string;
  writerExists?: boolean;
  onDone: (resourceId: string) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState('');
  const [defaultBranch, setDefaultBranch] = useState('main');
  const [parent, setParent] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [withEndpoint, setWithEndpoint] = useState(true);
  const [endpointType, setEndpointType] = useState<'read_write' | 'read_only'>(
    writerExists ? 'read_only' : 'read_write',
  );
  const [password, setPassword] = useState('');
  const [bounds, setBounds] = useState<Bounds>(initialBounds);
  const [busy, setBusy] = useState(false);
  const [operation, setOperation] = useState<Operation | null>(null);
  const [resourceId, setResourceId] = useState('');
  const [operationProject, setOperationProject] = useState('');
  const [error, setError] = useState('');
  const [trackingNotice, setTrackingNotice] = useState('');
  const polling = useRef<AbortController | null>(null);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      polling.current?.abort();
    };
  }, []);
  const requestKey = useRef<string>('');
  const titles = {
    project: '创建项目',
    branch: '创建数据库分支',
    endpoint: '创建 Compute Endpoint',
  };
  const resetKey = () => {
    requestKey.current = '';
  };
  const setBound = (key: keyof Bounds, value: number) => {
    setBounds((current) => ({ ...current, [key]: value }));
    resetKey();
  };

  async function follow(project: string, id: string, resource: string) {
    polling.current?.abort();
    const controller = new AbortController();
    polling.current = controller;
    const current = await followOperation({
      id,
      signal: controller.signal,
      read: (signal) => api<Operation>(`${projectPath(project)}/operations/${id}`, { signal }),
      onValue: (value) => {
        setOperation(value);
        setTrackingNotice('');
      },
      onUnavailable: () => setTrackingNotice('服务暂时不可用，正在继续查询已受理的操作。'),
    });
    if (!current) {
      setError('状态查询暂未完成，可继续查询或在操作记录中跟踪。');
      return;
    }
    if (current.state === 'succeeded') {
      onDone(resource);
      return;
    }
    setError(
      current.state === 'cancelled'
        ? '操作已取消，可在操作记录中查看。'
        : current.error_message || '调谐失败。可在操作记录中查看步骤并重试。',
    );
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    const path =
      kind === 'project'
        ? `/api/v1/organizations/${encodeURIComponent(organizationId)}/projects`
        : kind === 'branch'
          ? `${projectPath(projectId || '')}/branches`
          : `${projectPath(projectId || '')}/endpoints`;
    const body =
      kind === 'project'
        ? {
            name,
            default_branch_name: defaultBranch,
            region_id: 'rke2-lab',
            postgres_version: 16,
            password,
            autoscaling: bounds,
          }
        : kind === 'branch'
          ? {
              name,
              parent_branch_id: parent,
              create_endpoint: withEndpoint,
              password: withEndpoint ? password : '',
              autoscaling: bounds,
            }
          : {
              branch_id: branchId,
              type: endpointType,
              ...(endpointType === 'read_write' ? { password } : {}),
              autoscaling: bounds,
            };
    try {
      if (!requestKey.current) requestKey.current = newRequestKey();
      const accepted = await api<{
        resource: { id: string; project_id?: string };
        operation: Operation;
      }>(path, {
        method: 'POST',
        headers: { 'Idempotency-Key': requestKey.current },
        body: JSON.stringify(body),
      });
      setPassword('');
      setResourceId(accepted.resource.id);
      setOperationProject(accepted.resource.project_id || accepted.resource.id);
      setOperation(accepted.operation);
      await follow(
        accepted.resource.project_id || accepted.resource.id,
        accepted.operation.id,
        accepted.resource.id,
      );
    } catch (reason) {
      if (mounted.current) setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  async function retry() {
    if (!operationProject || !operation) return;
    setBusy(true);
    setError('');
    try {
      await api(`${projectPath(operationProject)}/operations/${operation.id}/retry`, {
        method: 'POST',
      });
      await follow(operationProject, operation.id, resourceId);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(false);
    }
  }

  async function continueTracking() {
    if (!operation || !operationProject || busy) return;
    setBusy(true);
    setError('');
    try {
      await follow(operationProject, operation.id, resourceId);
    } catch (reason) {
      if (mounted.current) setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  return (
    <div
      className="modal-backdrop"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose();
      }}
    >
      <section
        className="modal create-modal"
        role="dialog"
        aria-modal="true"
        aria-label={titles[kind]}
      >
        <div className="create-modal-header">
          <div>
            <small>NEON / PROVISIONING</small>
            <h2>{titles[kind]}</h2>
          </div>
          <button
            className="icon-button"
            type="button"
            onClick={onClose}
            disabled={busy}
            aria-label="关闭创建窗口"
          >
            ×
          </button>
        </div>
        <p className="muted">
          {kind === 'branch' && !withEndpoint
            ? '请求会生成持久 Operation，创建独立 Timeline。稍后可为此分支单独创建 Compute Endpoint。'
            : '请求会生成持久 Operation，依次调谐 Storage Controller、NeonVM 和 Proxy，并在 SQL 探针成功后标记就绪。'}
        </p>
        <form onSubmit={submit}>
          {kind !== 'endpoint' && (
            <label className="create-field">
              {kind === 'project' ? '项目名称' : '分支名称'}
              <input
                required
                maxLength={kind === 'project' ? 64 : 63}
                value={name}
                onChange={(e) => {
                  setName(e.target.value);
                  resetKey();
                }}
                placeholder={kind === 'project' ? '例如 AI Workspace' : '例如 preview-feature'}
              />
            </label>
          )}
          {kind === 'project' && (
            <label className="create-field">
              默认分支
              <input
                required
                maxLength={63}
                value={defaultBranch}
                onChange={(e) => {
                  setDefaultBranch(e.target.value);
                  resetKey();
                }}
              />
            </label>
          )}
          {kind === 'branch' && (
            <>
              <label className="create-field">
                父分支
                <select
                  aria-label="父分支"
                  value={parent}
                  onChange={(e) => {
                    setParent(e.target.value);
                    resetKey();
                  }}
                >
                  {branches
                    .filter((b) => b.state === 'ready')
                    .map((b) => (
                      <option key={b.id} value={b.id}>
                        {b.name} · {b.id}
                      </option>
                    ))}
                </select>
              </label>
              <label className="create-check">
                <input
                  type="checkbox"
                  checked={withEndpoint}
                  onChange={(e) => {
                    setWithEndpoint(e.target.checked);
                    resetKey();
                  }}
                />
                同时创建读写 Compute Endpoint
              </label>
            </>
          )}
          {kind === 'endpoint' && (
            <>
              <p className="create-target">
                目标分支：<code>{branchId}</code>
              </p>
              <label className="create-field">
                Compute 类型
                <select
                  aria-label="Compute 类型"
                  value={endpointType}
                  onChange={(e) => {
                    setEndpointType(e.target.value as 'read_write' | 'read_only');
                    resetKey();
                  }}
                >
                  <option value="read_write" disabled={writerExists}>
                    读写主计算节点（每分支一个）
                  </option>
                  <option value="read_only">只读计算节点（可创建多个）</option>
                </select>
                <small>
                  只读节点共享分支存储，使用主计算节点的数据库角色密码，通过自己的 Endpoint Selector
                  连接。
                </small>
              </label>
            </>
          )}
          {(kind !== 'branch' || withEndpoint) &&
            (kind !== 'endpoint' || endpointType === 'read_write') && (
              <label className="create-field">
                数据库角色密码
                <input
                  type="password"
                  aria-label="数据库角色密码"
                  required
                  minLength={12}
                  maxLength={256}
                  autoComplete="new-password"
                  value={password}
                  onChange={(e) => {
                    setPassword(e.target.value);
                    resetKey();
                  }}
                />
                <small>
                  创建后请保存。控制数据库和 Operation 不保存明文，页面在请求受理后清空输入。
                </small>
              </label>
            )}
          {(kind !== 'branch' || withEndpoint) && (
            <div className="form-grid create-bounds">
              <label>
                最小 CPU
                <select
                  aria-label="最小 CPU"
                  value={bounds.min_cpu_milli}
                  onChange={(e) => setBound('min_cpu_milli', Number(e.target.value))}
                >
                  <option value={1000}>1000m</option>
                  <option value={2000}>2000m</option>
                </select>
              </label>
              <label>
                最大 CPU
                <select
                  aria-label="最大 CPU"
                  value={bounds.max_cpu_milli}
                  onChange={(e) => setBound('max_cpu_milli', Number(e.target.value))}
                >
                  <option value={1000}>1000m</option>
                  <option value={2000}>2000m</option>
                </select>
              </label>
              <label>
                最小内存
                <select
                  aria-label="最小内存"
                  value={bounds.min_memory_mib}
                  onChange={(e) => setBound('min_memory_mib', Number(e.target.value))}
                >
                  {[1024, 2048, 3072].map((n) => (
                    <option key={n} value={n}>
                      {n} MiB
                    </option>
                  ))}
                </select>
              </label>
              <label>
                最大内存
                <select
                  aria-label="最大内存"
                  value={bounds.max_memory_mib}
                  onChange={(e) => setBound('max_memory_mib', Number(e.target.value))}
                >
                  {[1024, 2048, 3072].map((n) => (
                    <option key={n} value={n}>
                      {n} MiB
                    </option>
                  ))}
                </select>
              </label>
            </div>
          )}
          <div className="create-actions">
            <button className="button" type="button" onClick={onClose} disabled={busy}>
              关闭
            </button>
            <button className="button primary" disabled={busy || !!operation}>
              {busy ? '正在调谐…' : '创建并验证'}
            </button>
          </div>
        </form>
        {operation && (
          <div className="create-progress" aria-live="polite">
            <strong>
              操作 {operation.id} · {operation.state}
            </strong>
            {operation.steps?.map((step) => (
              <div key={step.ordinal}>
                <span>
                  {step.state === 'succeeded' ? '✓' : step.state === 'failed' ? '!' : '·'}
                </span>{' '}
                {step.name}
              </div>
            ))}
            {trackingNotice && <p role="status">{trackingNotice}</p>}
            {!busy && ['queued', 'running', 'retry_wait'].includes(operation.state) && (
              <button className="button" onClick={continueTracking}>
                继续查询操作
              </button>
            )}
            {operation.state === 'failed' && operation.retryable && (
              <button className="button" onClick={retry} disabled={busy}>
                重试原操作
              </button>
            )}
          </div>
        )}
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
      </section>
    </div>
  );
}
