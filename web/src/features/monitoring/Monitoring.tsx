import { useEffect, useRef, useState } from 'react';
import { api, endpointPath } from '../../api';
import type { Endpoint, Metric, MetricHistory } from '../../api';
import { fmt, PageHeading } from '../../shared/ui';
import { useEndpointSelection } from '../../shared/useEndpointSelection';

function Sparkline({
  items,
  field,
  color,
  unit,
}: {
  items: Metric[];
  field: keyof Metric;
  color: string;
  unit: string;
}) {
  // Missing observations are not zero and must not connect two separated
  // runs. Use actual timestamps so slow/failed sampling is visible as a gap.
  type Point = { x: number; y: number };
  const segments: Point[][] = [];
  let segment: Point[] | null = null;
  let previousTime: number | null = null;
  for (const item of items) {
    const sampled = new Date(item.sampled_at).getTime();
    const value = item[field];
    if (typeof value !== 'number' || !Number.isFinite(value) || !Number.isFinite(sampled)) {
      segment = null;
      previousTime = null;
      continue;
    }
    if (previousTime !== null && (sampled <= previousTime || sampled - previousTime > 90000)) {
      segment = null;
    }
    if (segment === null) {
      segment = [];
      segments.push(segment);
    }
    segment.push({ x: sampled, y: value });
    previousTime = sampled;
  }
  const points = segments.flat();
  if (points.length < 2) return <div className="chart-empty">采样不足，趋势稍后显示</div>;
  const max = Math.max(...points.map((p) => p.y), 1);
  const min = Math.min(...points.map((p) => p.y), 0);
  const height = 170;
  const width = 760;
  const times = items.map((item) => new Date(item.sampled_at).getTime()).filter(Number.isFinite);
  const firstTime = Math.min(...times);
  const lastTime = Math.max(...times);
  const x = (p: Point) => ((p.x - firstTime) / Math.max(lastTime - firstTime, 1)) * width;
  const y = (p: Point) => height - 14 - ((p.y - min) / Math.max(max - min, 1)) * (height - 34);
  return (
    <div className="chart-wrap">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={`${field} 趋势，单位 ${unit}`}
        preserveAspectRatio="none"
      >
        <desc>按真实采样时间排列；缺测时段保留断点。</desc>
        <line x1="0" y1="20" x2={width} y2="20" className="grid-line" />
        <line x1="0" y1="85" x2={width} y2="85" className="grid-line" />
        <line x1="0" y1="150" x2={width} y2="150" className="grid-line" />
        {segments.map((run, index) =>
          run.length === 1 ? (
            <circle key={index} cx={x(run[0])} cy={y(run[0])} r="3" fill={color} />
          ) : (
            <polyline
              key={index}
              points={run.map((p) => `${x(p)},${y(p)}`).join(' ')}
              fill="none"
              stroke={color}
              strokeWidth="3"
              vectorEffect="non-scaling-stroke"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          ),
        )}
      </svg>
      <div className="chart-axis">
        <span>{items[0] ? fmt(items[0].sampled_at) : ''}</span>
        <span>
          {unit} · max {max.toFixed(1)}
        </span>
        <span>{items.at(-1) ? fmt(items.at(-1)!.sampled_at) : ''}</span>
      </div>
    </div>
  );
}

export function Monitoring({
  projectId,
  endpoints,
  branchName,
  showError,
}: {
  projectId: string;
  endpoints: Endpoint[];
  branchName: (id: string) => string;
  showError: (error: unknown) => void;
}) {
  const [selected, setSelected] = useEndpointSelection(projectId, endpoints);
  const [period, setPeriod] = useState('1h');
  const [history, setHistory] = useState<MetricHistory | null>(null);
  const pending = useRef<AbortController | null>(null);
  const endpoint = endpoints.find((e) => e.id === selected) || endpoints[0];
  const load = () => {
    if (!endpoint) return;
    // A slow response for a previous Endpoint must not overwrite its successor.
    pending.current?.abort();
    const controller = new AbortController();
    pending.current = controller;
    api<MetricHistory>(endpointPath(projectId, endpoint.id) + `/metrics?period=${period}`, {
      signal: controller.signal,
    })
      .then((data) => {
        if (!controller.signal.aborted && data.endpoint_id === endpoint.id) setHistory(data);
      })
      .catch((error) => {
        if (!controller.signal.aborted) showError(error);
      });
  };
  useEffect(() => {
    setHistory(null);
    load();
    const timer = setInterval(load, 30000);
    return () => {
      clearInterval(timer);
      pending.current?.abort();
    };
  }, [endpoint?.id, period]);
  const latest = history?.items.at(-1);
  return (
    <>
      <PageHeading
        kicker="PROJECT / OBSERVABILITY"
        title="监控与运行洞察"
        description="实时采样的资源、数据库活动与数据质量。"
        action={
          <button className="button" onClick={load}>
            ↻ 刷新
          </button>
        }
      />
      <div className="monitor-toolbar">
        <label>
          分支 / Endpoint
          <select value={endpoint?.id || ''} onChange={(e) => setSelected(e.target.value)}>
            {endpoints.map((e) => (
              <option key={e.id} value={e.id}>
                {branchName(e.branch_id)} · {e.selector}
              </option>
            ))}
          </select>
        </label>
        <div className="segmented" role="group" aria-label="时间范围">
          {['15m', '1h', '6h', '24h'].map((value) => (
            <button
              key={value}
              className={period === value ? 'selected' : ''}
              onClick={() => setPeriod(value)}
            >
              {value}
            </button>
          ))}
        </div>
        <span className={history?.fresh ? 'fresh' : 'stale'}>
          ● {history?.fresh ? '实时采集中' : '暂无新鲜样本'}
        </span>
      </div>
      <div className="stats-grid">
        <div className="stat-card">
          <small>CPU 实际用量</small>
          <strong>
            {latest?.cpu_used_milli == null ? '—' : `${latest.cpu_used_milli.toFixed(1)}m`}
          </strong>
          <span>Pod · 已分配 {latest?.cpu_allocated_milli ?? '—'}m</span>
        </div>
        <div className="stat-card">
          <small>内存实际用量</small>
          <strong>
            {latest?.memory_used_mib == null ? '—' : `${latest.memory_used_mib.toFixed(1)} MiB`}
          </strong>
          <span>Pod · 已分配 {latest?.memory_allocated_mib ?? '—'} MiB</span>
        </div>
        <div className="stat-card">
          <small>数据库连接</small>
          <strong>{latest?.connections ?? '—'}</strong>
          <span>活跃 {latest?.active_connections ?? '—'}</span>
        </div>
        <div className="stat-card">
          <small>数据库大小</small>
          <strong>
            {latest?.database_size_bytes == null
              ? '—'
              : `${(latest.database_size_bytes / 1048576).toFixed(1)} MiB`}
          </strong>
          <span>PostgreSQL</span>
        </div>
      </div>
      <div className="two-col charts">
        <section className="panel pad">
          <div className="panel-heading">
            <h2>CPU 分配趋势</h2>
            <small>mCPU</small>
          </div>
          <Sparkline
            items={history?.items || []}
            field="cpu_allocated_milli"
            color="#4fe3bc"
            unit="mCPU"
          />
        </section>
        <section className="panel pad">
          <div className="panel-heading">
            <h2>Pod 实际 CPU</h2>
            <small>mCPU</small>
          </div>
          <Sparkline
            items={history?.items || []}
            field="cpu_used_milli"
            color="#7ba7ff"
            unit="mCPU"
          />
        </section>
        <section className="panel pad">
          <div className="panel-heading">
            <h2>内存分配趋势</h2>
            <small>MiB</small>
          </div>
          <Sparkline
            items={history?.items || []}
            field="memory_allocated_mib"
            color="#a29afa"
            unit="MiB"
          />
        </section>
        <section className="panel pad">
          <div className="panel-heading">
            <h2>数据库连接</h2>
            <small>连接数</small>
          </div>
          <Sparkline
            items={history?.items || []}
            field="connections"
            color="#f9b468"
            unit="connections"
          />
        </section>
      </div>
      <div className="panel pad">
        <div className="panel-heading">
          <h2>指标质量与来源</h2>
          <span>
            {history?.sample_count ?? 0} 个采样点 · 最新 {history?.latest_age_seconds ?? '—'} 秒前
          </span>
        </div>
        <div className="source-grid">
          <div>
            <small>Compute 状态</small>
            <strong>{latest?.observed_state || '—'}</strong>
          </div>
          <div>
            <small>资源分配</small>
            <strong>{history?.sources.allocation || '—'}</strong>
          </div>
          <div>
            <small>Pod 实际用量</small>
            <strong>{history?.sources.usage || '—'}</strong>
          </div>
          <div>
            <small>Postgres 指标</small>
            <strong>{history?.sources.postgres || '—'}</strong>
          </div>
        </div>
        {latest?.errors && Object.keys(latest.errors).length > 0 && (
          <div className="notice compact">
            部分来源缺测：
            {Object.entries(latest.errors)
              .map(([key, value]) => `${key}: ${value}`)
              .join(' · ')}
          </div>
        )}
        <p className="muted">
          Pod 用量包含 VM 开销，不等于 Guest 内 PostgreSQL 进程。缺测显示“—”，Idle Compute
          不会因采样被唤醒。
        </p>
      </div>
    </>
  );
}
