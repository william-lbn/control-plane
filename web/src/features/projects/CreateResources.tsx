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
  allowHistorical = false,
  restoreOnly = false,
  onDone,
  onClose,
}: {
  kind: Kind;
  organizationId?: string;
  projectId?: string;
  branches?: Branch[];
  branchId?: string;
  writerExists?: boolean;
  allowHistorical?: boolean;
  restoreOnly?: boolean;
  onDone: (resourceId: string) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState('');
  const [defaultBranch, setDefaultBranch] = useState('main');
  const [parent, setParent] = useState(
    branchId || branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [pointKind, setPointKind] = useState<'current' | 'timestamp' | 'lsn'>(
    restoreOnly ? 'timestamp' : 'current',
  );
  const [point, setPoint] = useState('');
  const [pointPreview, setPointPreview] = useState<{
    branch_id: string;
    source_timeline_id: string;
    lsn: string;
    retention_pinned: false;
    input: string;
    kind: string;
  } | null>(null);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState('');
  const previewEpoch = useRef(0);
  useEffect(() => {
    previewEpoch.current++;
    setPointPreview(null);
    setPreviewError('');
    setPreviewBusy(false);
  }, [parent, pointKind, point]);
  const selectedPreview =
    pointPreview?.branch_id === parent &&
    pointPreview.input === point &&
    pointPreview.kind === pointKind
      ? pointPreview
      : null;
  async function previewPoint() {
    const epoch = ++previewEpoch.current;
    setPreviewBusy(true);
    setPreviewError('');
    try {
      const query = new URLSearchParams({ [pointKind]: point });
      const value = await api<{
        branch_id: string;
        source_timeline_id: string;
        lsn: string;
        retention_pinned: false;
      }>(
        projectPath(projectId || '') +
          '/branches/' +
          encodeURIComponent(parent) +
          '/restore-point?' +
          query.toString(),
      );
      if (epoch === previewEpoch.current) {
        if (value.branch_id !== parent || value.retention_pinned !== false)
          throw new Error('恢复来源观察不匹配');
        setPointPreview({ ...value, input: point, kind: pointKind });
      }
    } catch (reason) {
      if (epoch === previewEpoch.current) {
        setPointPreview(null);
        setPreviewError(reason instanceof Error ? reason.message : String(reason));
      }
    } finally {
      if (epoch === previewEpoch.current) setPreviewBusy(false);
    }
  }
  const [window, setWindow] = useState<{ min_readable_lsn: string; latest_lsn: string } | null>(
    null,
  );
  const [windowError, setWindowError] = useState('');
  const [functionPolicy, setFunctionPolicy] = useState<'inherit_paused' | 'omit'>('inherit_paused');
  const [functionConsent, setFunctionConsent] = useState(false);
  const [historicalFunctionConsent, setHistoricalFunctionConsent] = useState(false);
  const [parentFunctions, setParentFunctions] = useState<{
    enabled: boolean;
    count: number;
  } | null>(null);
  const [functionPolicyError, setFunctionPolicyError] = useState('');
  useEffect(() => {
    setParentFunctions(null);
    setFunctionPolicyError('');
    setFunctionConsent(false);
    setHistoricalFunctionConsent(false);
    if (kind !== 'branch' || !parent || !projectId) return;
    const controller = new AbortController();
    void api<{ branch_copy_enabled: boolean; items: { state: string }[] }>(
      `${projectPath(projectId)}/branches/${encodeURIComponent(parent)}/functions?limit=100`,
      { signal: controller.signal },
    )
      .then((value) => {
        if (!controller.signal.aborted)
          setParentFunctions({
            enabled: value.branch_copy_enabled === true,
            count: value.items.filter((f) => !['deleting', 'deleted'].includes(f.state)).length,
          });
      })
      .catch((reason) => {
        if (!controller.signal.aborted)
          setFunctionPolicyError(reason instanceof Error ? reason.message : String(reason));
      });
    return () => controller.abort();
  }, [kind, parent, projectId]);
  useEffect(() => {
    setWindow(null);
    setWindowError('');
    if (kind !== 'branch' || pointKind === 'current' || !parent) return;
    const controller = new AbortController();
    void api<{ min_readable_lsn: string; latest_lsn: string }>(
      `${projectPath(projectId || '')}/branches/${encodeURIComponent(parent)}/restore-window`,
      { signal: controller.signal },
    )
      .then((value) => {
        if (!controller.signal.aborted) setWindow(value);
      })
      .catch((reason) => {
        if (!controller.signal.aborted)
          setWindowError(reason instanceof Error ? reason.message : String(reason));
      });
    return () => controller.abort();
  }, [kind, parent, pointKind, projectId]);
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
    branch: restoreOnly ? '恢复到新分支' : '创建数据库分支',
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
    if (busy || operation) return;
    if (
      kind === 'branch' &&
      (!parentFunctions ||
        functionPolicyError ||
        (parentFunctions.enabled &&
          parentFunctions.count > 0 &&
          functionPolicy === 'inherit_paused' &&
          (!functionConsent || (pointKind !== 'current' && !historicalFunctionConsent))))
    )
      return;
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
              ...(parentFunctions?.enabled
                ? {
                    function_copy_policy: functionPolicy,
                    acknowledge_function_environment_copy: functionConsent,
                    acknowledge_current_functions_for_historical_data: historicalFunctionConsent,
                  }
                : {}),
              ...(selectedPreview
                ? {
                    parent_lsn: selectedPreview.lsn,
                    expected_parent_timeline_id: selectedPreview.source_timeline_id,
                  }
                : pointKind === 'timestamp'
                  ? { parent_timestamp: point }
                  : pointKind === 'lsn'
                    ? { parent_lsn: point }
                    : {}),
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
                    .filter((b) => b.state === 'ready' && !b.expires_at)
                    .map((b) => (
                      <option key={b.id} value={b.id}>
                        {b.name} · {b.id}
                      </option>
                    ))}
                </select>
              </label>
              {(allowHistorical || restoreOnly) && (
                <>
                  <label className="create-field">
                    分支起点
                    <select
                      aria-label="分支起点"
                      value={pointKind}
                      onChange={(e) => {
                        setPointKind(e.target.value as typeof pointKind);
                        setPoint('');
                        resetKey();
                      }}
                    >
                      {!restoreOnly && <option value="current">当前状态</option>}
                      <option value="timestamp">历史时间（UTC）</option>
                      <option value="lsn">指定 LSN</option>
                    </select>
                  </label>
                  {pointKind !== 'current' && (
                    <>
                      <label className="create-field">
                        {pointKind === 'timestamp' ? '恢复时间（RFC3339，含时区）' : '恢复 LSN'}
                        <input
                          aria-label={pointKind === 'timestamp' ? '恢复时间' : '恢复 LSN'}
                          value={point}
                          required
                          maxLength={64}
                          placeholder={
                            pointKind === 'timestamp' ? '2026-10-07T06:00:00.000Z' : '0/1A2B3C8'
                          }
                          onChange={(e) => {
                            setPoint(e.target.value);
                            resetKey();
                          }}
                        />
                      </label>
                      <button
                        type="button"
                        className="button"
                        disabled={busy || !!operation || previewBusy || !point || !parent}
                        onClick={() => void previewPoint()}
                      >
                        {previewBusy ? '正在预览…' : '预览时间点'}
                      </button>
                      {previewError && <p role="alert">{previewError}</p>}
                      {selectedPreview && (
                        <p role="status">
                          已解析 LSN：{selectedPreview.lsn}
                          。确认后使用该精确点；历史尚未固定，受理时重新校验原 Timeline 和保留范围。
                        </p>
                      )}
                      <div className="subtle-note" role="status">
                        {windowError ||
                          (window
                            ? `可恢复 LSN：${window.min_readable_lsn} 至 ${window.latest_lsn}。提交时重新校验；恢复会创建独立分支和连接入口。`
                            : '正在读取存储历史边界…')}
                      </div>
                    </>
                  )}
                </>
              )}
              {functionPolicyError && (
                <p role="alert">
                  无法读取父分支函数策略：{functionPolicyError}。请重新打开创建窗口后重试。
                </p>
              )}
              {!parentFunctions && !functionPolicyError && (
                <p role="status">正在读取父分支函数策略…</p>
              )}
              {parentFunctions?.enabled && (
                <>
                  <label className="create-field">
                    函数策略
                    <select
                      aria-label="函数策略"
                      value={functionPolicy}
                      onChange={(e) => {
                        setFunctionPolicy(e.target.value as typeof functionPolicy);
                        setFunctionConsent(false);
                        setHistoricalFunctionConsent(false);
                        resetKey();
                      }}
                    >
                      <option value="inherit_paused">继承父分支函数，准备后保持暂停</option>
                      <option value="omit">仅创建数据库分支，不复制函数</option>
                    </select>
                  </label>
                  {functionPolicy === 'inherit_paused' && (
                    <>
                      <p className="subtle-note">
                        固定受理时的父分支函数版本。若暂不创建 Writer，函数等待此分支的 Writer
                        准备；触发器默认停用，公开访问和日志采集需要另行授权。
                      </p>
                      {parentFunctions.count > 0 && (
                        <label className="create-check">
                          <input
                            type="checkbox"
                            aria-label="确认复制函数环境"
                            checked={functionConsent}
                            onChange={(e) => {
                              setFunctionConsent(e.target.checked);
                              resetKey();
                            }}
                          />
                          确认将父分支 {parentFunctions.count}{' '}
                          个函数的环境变量复制到独立的子分支环境。
                        </label>
                      )}
                      {parentFunctions.count > 0 && pointKind !== 'current' && (
                        <label className="create-check">
                          <input
                            type="checkbox"
                            aria-label="确认历史 SQL 使用当前函数代码"
                            checked={historicalFunctionConsent}
                            onChange={(e) => {
                              setHistoricalFunctionConsent(e.target.checked);
                              resetKey();
                            }}
                          />
                          确认历史 SQL
                          数据点使用本次受理时的函数代码与环境；这不代表历史时间点的代码版本。
                        </label>
                      )}
                    </>
                  )}
                </>
              )}
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
                  {kind === 'endpoint'
                    ? '重建已有分支的读写 Compute 时，请使用原数据库角色密码。创建计算不会轮换数据库密码。'
                    : '创建后请保存。'}
                  控制数据库和 Operation 不保存明文，页面在请求受理后清空输入。
                </small>
              </label>
            )}
          {(kind !== 'branch' || withEndpoint) && (
            <div className="form-grid create-bounds">
              <label>
                资源预设
                <select
                  aria-label="计算资源预设"
                  value=""
                  onChange={(e) => {
                    if (e.target.value === 'fractional') {
                      setBounds({
                        min_cpu_milli: 250,
                        max_cpu_milli: 500,
                        min_memory_mib: 1024,
                        max_memory_mib: 2048,
                      });
                      resetKey();
                    } else if (e.target.value === 'integer') {
                      setBounds(initialBounds);
                      resetKey();
                    }
                  }}
                >
                  <option value="">选择配置</option>
                  <option value="fractional">0.25–0.5 vCPU / 1–2 GiB</option>
                  <option value="integer">整数 CPU 兼容配置</option>
                </select>
              </label>
              <p className="subtle-note">
                小数 CPU 使用 agent 的 250m / 1Gi 比例；新 Endpoint 可设 250m。已有 VM
                不可越过创建时的硬件下限。逻辑 CPU 预算与 QEMU overcommit 硬配额不同。
              </p>
              <label>
                最小 CPU
                <select
                  aria-label="最小 CPU"
                  value={bounds.min_cpu_milli}
                  onChange={(e) => setBound('min_cpu_milli', Number(e.target.value))}
                >
                  <option value={250}>250m · 0.25 vCPU</option>
                  <option value={500}>500m · 0.5 vCPU</option>
                  <option value={750}>750m · 0.75 vCPU</option>
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
                  <option value={250}>250m · 0.25 vCPU</option>
                  <option value={500}>500m · 0.5 vCPU</option>
                  <option value={750}>750m · 0.75 vCPU</option>
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
            <button
              className="button primary"
              disabled={
                busy ||
                !!operation ||
                (kind === 'branch' &&
                  (!parentFunctions ||
                    !!functionPolicyError ||
                    (parentFunctions.enabled &&
                      parentFunctions.count > 0 &&
                      functionPolicy === 'inherit_paused' &&
                      (!functionConsent ||
                        (pointKind !== 'current' && !historicalFunctionConsent))))) ||
                (kind === 'branch' &&
                  pointKind !== 'current' &&
                  (!window || !point || !!windowError))
              }
            >
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
