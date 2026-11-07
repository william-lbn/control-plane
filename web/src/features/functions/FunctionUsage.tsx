import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';

type Counter = number | null;
type Usage = {
  function_id: string;
  state: string;
  invocations: {
    channel: string;
    admitted_invocations: Counter;
    completed_invocations: Counter;
    in_progress_invocations: Counter;
    unknown_invocations: Counter;
    observed_response_bytes: Counter;
  }[];
  edge: {
    dispatch_attempts: Counter;
    unconfirmed_dispatches: Counter;
    completion_receipts: Counter;
    admitted_request_bytes: Counter;
    observed_response_bytes: Counter;
    observed_websocket_request_bytes: Counter;
  };
  resource_status: 'observed' | 'historical_only' | 'unavailable';
  resource_observations: {
    instance_id: string;
    deployment_id: string;
    boot_id: string;
    observed_cpu_usec: Counter;
    observed_memory_current_bytes: Counter;
    observed_memory_peak_bytes: Counter;
    sample_count: Counter;
    last_observed_at: string;
    fresh: boolean;
  }[];
};

const number = (value: unknown) =>
  typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
    ? value.toLocaleString()
    : '不可用';
const timestamp = (value: string) => {
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : '观测时间不可用';
};
const channels: Record<string, string> = {
  console: 'Console',
  public_http: '公开 HTTP / SSE / WebSocket',
  schedule: '定时任务',
  object_storage: '对象事件',
};

function validateUsage(value: Usage, functionId: string) {
  if (!value || value.function_id !== functionId) throw new Error('函数用量身份不匹配，请刷新');
  if (
    !value.edge ||
    !Array.isArray(value.invocations) ||
    !Array.isArray(value.resource_observations)
  )
    throw new Error('函数用量响应不完整，请重新读取');
  const seen = new Set<string>();
  for (const row of value.resource_observations) {
    const key = `${row.instance_id}/${row.boot_id}`;
    if (
      !/^fni_[a-f0-9]{16}$/.test(row.instance_id) ||
      !/^fdp_[a-f0-9]{16}$/.test(row.deployment_id) ||
      !/^[a-f0-9]{32}$/.test(row.boot_id) ||
      seen.has(key)
    )
      throw new Error('原实例资源记录身份不一致，请重新读取');
    seen.add(key);
  }
}

export function FunctionUsage({ path, functionId }: { path: string; functionId: string }) {
  const scope = `${path}/${functionId}`;
  const [snapshot, setSnapshot] = useState<{ scope: string; value: Usage; readAt: string } | null>(
    null,
  );
  const [loadingScope, setLoadingScope] = useState('');
  const [failure, setFailure] = useState<{ scope: string; message: string } | null>(null);
  const epoch = useRef(0);
  const pending = useRef<AbortController | null>(null);
  useEffect(() => {
    epoch.current++;
    pending.current?.abort();
    setSnapshot(null);
    setFailure(null);
    setLoadingScope('');
    return () => {
      epoch.current++;
      pending.current?.abort();
    };
  }, [scope]);
  const usage = snapshot?.scope === scope ? snapshot.value : null;
  const busy = loadingScope === scope;
  const error = failure?.scope === scope ? failure.message : '';
  async function load() {
    pending.current?.abort();
    const controller = new AbortController();
    pending.current = controller;
    const request = ++epoch.current;
    setLoadingScope(scope);
    setFailure(null);
    setSnapshot(null);
    try {
      const value = await api<Usage>(`${path}/usage`, { signal: controller.signal });
      validateUsage(value, functionId);
      if (request === epoch.current && !controller.signal.aborted)
        setSnapshot({ scope, value, readAt: new Date().toLocaleString() });
    } catch (e) {
      if (request === epoch.current && !controller.signal.aborted)
        setFailure({ scope, message: e instanceof Error ? e.message : String(e) });
    } finally {
      if (request === epoch.current) setLoadingScope('');
    }
  }
  // This is a manual snapshot, not a live runtime status. Even a fresh sample
  // from a retired boot must remain a historical resource observation.
  const resourceLabel = usage
    ? usage.resource_status === 'observed' && usage.state === 'active'
      ? '读取时当前实例有最近有效观测'
      : usage.resource_status === 'historical_only' || usage.resource_observations.length > 0
        ? '仅有历史资源观测，不能代表当前实例'
        : '资源观测不可用，不能按零计算'
    : '';
  return (
    <section
      className="backend-panel function-usage"
      data-testid="function-usage"
      aria-busy={busy}
      aria-labelledby={`function-usage-heading-${functionId}`}
    >
      <h3 id={`function-usage-heading-${functionId}`}>调用与资源用量</h3>
      <p>读取保留期内的服务端记录，不唤醒函数。观测缺失和结果未知不会按零或成功计算。</p>
      <button className="button" disabled={busy} onClick={() => void load()}>
        {busy ? '加载用量中…' : '加载函数用量'}
      </button>
      {error && <p role="alert">{error}</p>}
      {usage && (
        <div aria-live="polite" data-testid="function-usage-snapshot">
          <p className="muted">用量读取于 {snapshot?.readAt}，不会自动刷新。</p>
          <p data-testid="function-resource-status">{resourceLabel}</p>
          <dl className="function-facts">
            <div>
              <dt>Edge 完成回执</dt>
              <dd data-testid="function-edge-receipts">{number(usage.edge.completion_receipts)}</dd>
            </div>
            <div>
              <dt>Edge 派发尝试</dt>
              <dd>{number(usage.edge.dispatch_attempts)}</dd>
            </div>
            <div>
              <dt>Edge 派发未确认</dt>
              <dd data-testid="function-edge-unconfirmed">
                {number(usage.edge.unconfirmed_dispatches)}
              </dd>
            </div>
            <div>
              <dt>准入请求 Body 字节</dt>
              <dd>{number(usage.edge.admitted_request_bytes)}</dd>
            </div>
            <div>
              <dt>已观测响应 / WS 下行帧字节</dt>
              <dd>{number(usage.edge.observed_response_bytes)}</dd>
            </div>
            <div>
              <dt>WebSocket 上行帧字节</dt>
              <dd>{number(usage.edge.observed_websocket_request_bytes)}</dd>
            </div>
          </dl>
          <div
            className="function-usage-table"
            tabIndex={0}
            role="region"
            aria-label="按入口统计调用表格"
          >
            <table data-testid="function-invocation-usage">
              <caption>按入口统计调用</caption>
              <thead>
                <tr>
                  <th scope="col">入口</th>
                  <th scope="col">准入</th>
                  <th scope="col">完成传输</th>
                  <th scope="col">执行中</th>
                  <th scope="col">结果未知</th>
                  <th scope="col">已观测响应字节</th>
                </tr>
              </thead>
              <tbody>
                {usage.invocations.map((row) => (
                  <tr key={row.channel} data-channel={row.channel}>
                    <th scope="row">{channels[row.channel] || row.channel}</th>
                    <td>{number(row.admitted_invocations)}</td>
                    <td>{number(row.completed_invocations)}</td>
                    <td>{number(row.in_progress_invocations)}</td>
                    <td data-testid="function-unknown-invocations">
                      {number(row.unknown_invocations)}
                    </td>
                    <td>{number(row.observed_response_bytes)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {usage.invocations.length === 0 && <p className="muted">暂无保留的准入调用记录。</p>}
          <p className="muted">
            准入不代表业务成功；完成传输不包含结果未知，也不表示应用返回成功状态。
          </p>
          {usage.resource_observations.length > 0 && (
            <div
              className="function-usage-table"
              tabIndex={0}
              role="region"
              aria-label="原实例资源观测表格"
            >
              <table data-testid="function-resource-observations">
                <caption>原实例资源观测 · 按 Boot 分列</caption>
                <thead>
                  <tr>
                    <th scope="col">部署 / 实例 / Boot</th>
                    <th scope="col">原 Boot 累计 CPU μs</th>
                    <th scope="col">最近采样内存 / 峰值 bytes</th>
                    <th scope="col">样本数</th>
                    <th scope="col">最近观测</th>
                  </tr>
                </thead>
                <tbody>
                  {usage.resource_observations.map((row) => (
                    <tr
                      key={`${row.instance_id}/${row.boot_id}`}
                      data-instance={row.instance_id}
                      data-boot={row.boot_id}
                      data-deployment={row.deployment_id}
                    >
                      <th scope="row">
                        <code>{row.deployment_id}</code>
                        <br />
                        <code>{row.instance_id}</code>
                        <br />
                        <code>{row.boot_id}</code>
                      </th>
                      <td data-testid="function-boot-cpu">{number(row.observed_cpu_usec)}</td>
                      <td>
                        {number(row.observed_memory_current_bytes)} /{' '}
                        {number(row.observed_memory_peak_bytes)}
                      </td>
                      <td>{number(row.sample_count)}</td>
                      <td>
                        {timestamp(row.last_observed_at)}
                        <br />
                        {row.fresh ? '读取时采样未过时' : '历史样本'}
                        <br />
                        <span className="muted">不代表当前运行状态</span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <p className="muted">
            CPU 是每个原 Boot
            截至最近有效采样的累计下界，不按调用重复相加；内存为最近采样值和峰值。这些观测不自动扣费。
          </p>
        </div>
      )}
    </section>
  );
}
