import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';

type RuntimeObservation = {
  function_id: string;
  definition_state: string;
  definition_version_token: string;
  generation_token: string;
  observed: boolean;
  observation_source: 'metadata_only' | 'authenticated_supervisor';
  checked_at: string;
  instance_id?: string;
  deployment_id?: string;
  vm_phase?: string;
  node_ready?: boolean;
  draining?: boolean;
  active_invocations?: number;
  last_activity?: string;
  background_counters_trusted: false;
};

// Explicit observation only: mounting, polling metadata and changing selection
// never run customer code or wake SQL. A new key clears any older health state.
export function RuntimePanel({
  path,
  version,
  disabled,
}: {
  path: string;
  version: string;
  disabled: boolean;
}) {
  const [observation, setObservation] = useState<RuntimeObservation | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const active = useRef<AbortController | null>(null);
  useEffect(() => () => active.current?.abort(), []);
  async function observe() {
    if (busy || disabled) return;
    const controller = new AbortController();
    active.current?.abort();
    active.current = controller;
    setBusy(true);
    setError('');
    setObservation(null);
    try {
      const result = await api<RuntimeObservation>(path + '/runtime', {
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      if (result.definition_version_token !== version)
        throw new Error('定义已变化，请刷新函数后重新观察');
      setObservation(result);
    } catch (e) {
      if (!controller.signal.aborted) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <section className="function-runtime" data-testid="function-runtime-panel" aria-live="polite">
      <div className="function-detail-heading">
        <div>
          <h3>运行监控</h3>
          <p className="muted">显式观察原实例，不执行函数或唤醒数据库。</p>
        </div>
        <button className="button" disabled={busy || disabled} onClick={() => void observe()}>
          {busy ? '正在验证实例…' : '观察运行状态'}
        </button>
      </div>
      {error ? (
        <p role="alert">观察不可用：{error}</p>
      ) : !observation ? (
        <p className="muted">尚未读取运行状态，定义状态不代表实例健康。</p>
      ) : (
        <>
          <div className="function-runtime-grid">
            <div>
              <small>就绪状态</small>
              <strong data-testid="function-runtime-ready">
                {observation.observed
                  ? observation.node_ready
                    ? '已认证 · 就绪'
                    : '已认证 · 未就绪'
                  : '未运行观测'}
              </strong>
            </div>
            <div>
              <small>当前并发调用</small>
              <strong data-testid="function-runtime-inflight">
                {observation.observed ? observation.active_invocations : '—'}
              </strong>
            </div>
            <div>
              <small>最近调用活动</small>
              <strong>
                {observation.last_activity
                  ? new Date(observation.last_activity).toLocaleString()
                  : '—'}
              </strong>
            </div>
            <div>
              <small>排空状态</small>
              <strong>
                {observation.observed ? (observation.draining ? '正在排空' : '未排空') : '—'}
              </strong>
            </div>
          </div>
          <dl className="function-facts">
            <div>
              <dt>观测来源</dt>
              <dd>
                {observation.observed
                  ? '原 UID / 引导身份 / TLS / HMAC 验证'
                  : '仅元数据，无运行请求'}
              </dd>
            </div>
            <div>
              <dt>检查时间</dt>
              <dd>{new Date(observation.checked_at).toLocaleString()}</dd>
            </div>
            <div>
              <dt>执行代次</dt>
              <dd>{observation.generation_token}</dd>
            </div>
            {observation.instance_id && (
              <div>
                <dt>实例标识</dt>
                <dd>
                  <code>{observation.instance_id}</code>
                </dd>
              </div>
            )}
          </dl>
        </>
      )}
      <p className="muted">
        后台任务未计量；这些观测不能用于自动停止，也不代表 SQL、历史指标或计费验收。
      </p>
    </section>
  );
}
