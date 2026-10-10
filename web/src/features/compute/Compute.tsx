import { useEffect, useState } from 'react';
import { api, endpointPath, projectPath } from '../../api';
import type { Endpoint, Operation } from '../../api';
import { fmt, stateLabel, status, PageHeading } from '../../shared/ui';
import { useEndpointSelection } from '../../shared/useEndpointSelection';
import { newRequestKey } from '../../shared/requestKey';
import { DeleteEndpoint } from './DeleteEndpoint';

export function Compute({
  projectId,
  endpoints,
  branchName,
  onChanged,
  showError,
  readOnly = false,
}: {
  projectId: string;
  endpoints: Endpoint[];
  branchName: (id: string) => string;
  onChanged: () => Promise<void>;
  showError: (error: unknown) => void;
  readOnly?: boolean;
}) {
  const [selected, setSelected] = useEndpointSelection(projectId, endpoints);
  const endpoint = endpoints.find((e) => e.id === selected) || endpoints[0];
  const [minCPU, setMinCPU] = useState(1000);
  const [maxCPU, setMaxCPU] = useState(2000);
  const [minMem, setMinMem] = useState(1024);
  const [maxMem, setMaxMem] = useState(3072);
  const [scaleToZero, setScaleToZero] = useState(false);
  const [idleTimeout, setIdleTimeout] = useState(300);
  const [busy, setBusy] = useState(false);
  const [deleting, setDeleting] = useState<Endpoint | null>(null);
  useEffect(() => {
    if (!endpoint) return;
    setMinCPU(endpoint.min_cpu_milli || 1000);
    setMaxCPU(endpoint.max_cpu_milli || 2000);
    setMinMem(endpoint.min_memory_mib || 1024);
    setMaxMem(endpoint.max_memory_mib || 3072);
    setScaleToZero(endpoint.scale_to_zero || false);
    setIdleTimeout(endpoint.idle_timeout_seconds || 300);
  }, [endpoint?.id, endpoint?.version]);
  async function save() {
    if (!endpoint) return;
    setBusy(true);
    try {
      const result = await api<{ operation: Operation }>(endpointPath(projectId, endpoint.id), {
        method: 'PATCH',
        headers: { 'Idempotency-Key': newRequestKey(), 'If-Match': `"${endpoint.version}"` },
        body: JSON.stringify({
          autoscaling: {
            min_cpu_milli: minCPU,
            max_cpu_milli: maxCPU,
            min_memory_mib: minMem,
            max_memory_mib: maxMem,
          },
        }),
      });
      for (let i = 0; i < 25; i++) {
        await new Promise((resolve) => setTimeout(resolve, 1000));
        const op = await api<Operation>(
          `${projectPath(projectId)}/operations/${result.operation.id}`,
        );
        if (op.state === 'succeeded') {
          await onChanged();
          return;
        }
        if (op.state === 'failed') throw new Error(`${op.error_code}: ${op.error_message}`);
      }
      throw new Error('操作仍在执行，请查看操作记录');
    } catch (e) {
      showError(e);
    } finally {
      setBusy(false);
    }
  }
  async function saveLifecycle() {
    if (!endpoint) return;
    setBusy(true);
    try {
      await api(endpointPath(projectId, endpoint.id) + '/lifecycle', {
        method: 'PATCH',
        headers: { 'If-Match': `"${endpoint.version}"` },
        body: JSON.stringify({ scale_to_zero: scaleToZero, idle_timeout_seconds: idleTimeout }),
      });
      await onChanged();
    } catch (e) {
      showError(e);
    } finally {
      setBusy(false);
    }
  }
  async function suspend() {
    if (!endpoint) return;
    setBusy(true);
    try {
      const accepted = await api<{ operation: Operation }>(
        endpointPath(projectId, endpoint.id) + '/suspend',
        { method: 'POST' },
      );
      for (let i = 0; i < 130; i++) {
        await new Promise((resolve) => setTimeout(resolve, 1000));
        const op = await api<Operation>(
          `${projectPath(projectId)}/operations/${accepted.operation.id}`,
        );
        if (op.state === 'succeeded') {
          await onChanged();
          return;
        }
        if (op.state === 'failed') throw new Error(`${op.error_code}: ${op.error_message}`);
      }
      throw new Error('缩零操作仍在运行，请查看操作记录');
    } catch (e) {
      showError(e);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeading
        kicker="PROJECT / COMPUTE"
        title="Compute"
        description="查看真实 NeonVM 状态，调整自动纵向伸缩边界。"
      />
      <div className="selector-row">
        <label>
          选择 Endpoint
          <select value={endpoint?.id || ''} onChange={(e) => setSelected(e.target.value)}>
            {endpoints.map((e) => (
              <option key={e.id} value={e.id}>
                {branchName(e.branch_id)} · {e.endpoint_type === 'read_only' ? '只读' : '读写'} ·{' '}
                {e.selector}
              </option>
            ))}
          </select>
        </label>
      </div>
      {!endpoint && (
        <section className="panel pad">
          <h2>分支数据已保留，当前没有 Compute</h2>
          <p className="muted">
            添加 Compute 后可以重新连接数据库。新的 Compute 使用新的连接地址。
          </p>
        </section>
      )}
      {endpoint && (
        <>
          <div className="stats-grid">
            <div className="stat-card">
              <small>观测状态</small>
              <strong>{stateLabel(endpoint.observed_state)}</strong>
              <span>{endpoint.runtime.phase || endpoint.workload_kind}</span>
            </div>
            <div className="stat-card">
              <small>当前 CPU</small>
              <strong>
                {endpoint.runtime.cpu_milli == null ? '—' : `${endpoint.runtime.cpu_milli}m`}
              </strong>
              <span>Guest 已分配</span>
            </div>
            <div className="stat-card">
              <small>当前内存</small>
              <strong>
                {endpoint.runtime.memory_mib == null ? '—' : `${endpoint.runtime.memory_mib} MiB`}
              </strong>
              <span>Guest 已分配</span>
            </div>
            <div className="stat-card">
              <small>配置版本</small>
              <strong>v{endpoint.version}</strong>
              <span>{endpoint.runtime.node_name || '节点未知'}</span>
            </div>
          </div>
          <div className="two-col">
            <section className="panel pad">
              <div className="panel-heading">
                <h2>自动伸缩边界</h2>
                {status(endpoint.observed_state)}
              </div>
              <p className="muted">
                agent 与 scheduler 决定实际 CPU/RAM；控制面只修改允许的有效范围。
              </p>
              <div className="form-grid">
                <label>
                  最小 CPU
                  <select
                    aria-label="最小 CPU"
                    value={minCPU}
                    onChange={(e) => setMinCPU(Number(e.target.value))}
                  >
                    <option value={1000}>1000m · 1 vCPU</option>
                    <option value={2000}>2000m · 2 vCPU</option>
                  </select>
                </label>
                <label>
                  最大 CPU
                  <select
                    aria-label="最大 CPU"
                    value={maxCPU}
                    onChange={(e) => setMaxCPU(Number(e.target.value))}
                  >
                    <option value={1000}>1000m · 1 vCPU</option>
                    <option value={2000}>2000m · 2 vCPU</option>
                  </select>
                </label>
                <label>
                  最小内存
                  <select
                    aria-label="最小内存"
                    value={minMem}
                    onChange={(e) => setMinMem(Number(e.target.value))}
                  >
                    {[1024, 2048, 3072].map((v) => (
                      <option key={v} value={v}>
                        {v} MiB
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  最大内存
                  <select
                    aria-label="最大内存"
                    value={maxMem}
                    onChange={(e) => setMaxMem(Number(e.target.value))}
                  >
                    {[1024, 2048, 3072].map((v) => (
                      <option key={v} value={v}>
                        {v} MiB
                      </option>
                    ))}
                  </select>
                </label>
              </div>
              <button
                className="button primary"
                disabled={
                  readOnly ||
                  busy ||
                  endpoint.workload_kind !== 'neonvm' ||
                  minCPU > maxCPU ||
                  minMem > maxMem
                }
                onClick={save}
              >
                {busy ? '正在调谐…' : '保存伸缩范围'}
              </button>
            </section>
            <section className="panel pad">
              <div className="panel-heading">
                <h2>运行信息</h2>
              </div>
              <div className="detail-row">
                <span>Endpoint Selector</span>
                <code>{endpoint.selector}</code>
              </div>
              <div className="detail-row">
                <span>计算类型</span>
                <strong>
                  {endpoint.endpoint_type === 'read_only' ? '只读副本' : '读写主节点'}
                </strong>
              </div>
              <div className="detail-row">
                <span>Workload</span>
                <code>{endpoint.workload_kind}</code>
              </div>
              <div className="detail-row">
                <span>VM Pod</span>
                <code>{endpoint.runtime.pod_name || '—'}</code>
              </div>
              <div className="detail-row">
                <span>最近观测</span>
                <span>{fmt(endpoint.runtime.observed_at)}</span>
              </div>
              <div className="lifecycle-panel">
                <label className="create-check">
                  <input
                    type="checkbox"
                    checked={scaleToZero}
                    onChange={(e) => setScaleToZero(e.target.checked)}
                  />
                  空闲自动缩到 0（实验室控制器）
                </label>
                <label>
                  空闲秒数
                  <input
                    type="number"
                    min={60}
                    max={3600}
                    step={30}
                    value={idleTimeout}
                    onChange={(e) => setIdleTimeout(Number(e.target.value))}
                  />
                </label>
                <p>按真实会话与监控采样判定空闲；连接栅栏和多副本 HA 尚未完成生产验收。</p>
                <button
                  className="button"
                  disabled={readOnly || busy || idleTimeout < 60 || idleTimeout > 3600}
                  onClick={saveLifecycle}
                >
                  保存生命周期策略
                </button>
                <button
                  className="button"
                  disabled={readOnly || busy || endpoint.observed_state !== 'active'}
                  onClick={suspend}
                >
                  现在缩到 0
                </button>
              </div>
            </section>
          </div>
          <section className="panel pad">
            <div className="panel-heading">
              <div>
                <h2>删除 Compute</h2>
                <p className="muted">关闭此计算地址并回收运行资源，保留分支数据和其他 Compute。</p>
              </div>
              <button
                className="button danger"
                disabled={
                  readOnly ||
                  busy ||
                  endpoint.state !== 'active' ||
                  endpoint.workload_kind !== 'neonvm'
                }
                onClick={() => setDeleting(endpoint)}
              >
                删除此 Compute
              </button>
            </div>
          </section>
        </>
      )}
      {deleting && (
        <DeleteEndpoint
          projectId={projectId}
          endpoint={deleting}
          onDeleted={onChanged}
          onClose={() => setDeleting(null)}
        />
      )}
    </>
  );
}
