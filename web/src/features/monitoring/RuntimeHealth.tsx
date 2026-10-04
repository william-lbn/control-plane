import { useEffect, useState } from 'react';
import { api } from '../../api';
import type { Capabilities, RuntimeStatus } from '../../api';
import { fmt } from '../../shared/ui';

export function RuntimeHealth() {
  const [runtime, setRuntime] = useState<RuntimeStatus | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let alive = true;
    let pending: AbortController | null = null;
    const load = async () => {
      pending?.abort();
      pending = new AbortController();
      const request = pending;
      try {
        const status = await api<Capabilities>('/api/v1/capabilities', { signal: request.signal });
        if (alive && !request.signal.aborted) {
          setRuntime(status.runtime || null);
          setFailed(false);
        }
      } catch {
        if (alive && !request.signal.aborted) setFailed(true);
      }
    };
    void load();
    const timer = setInterval(() => void load(), 15000);
    return () => {
      alive = false;
      clearInterval(timer);
      pending?.abort();
    };
  }, []);
  const active = !failed && runtime?.controller_status === 'active';
  return (
    <section className="notice compact" aria-label="控制面运行状态">
      <span>{active ? '◉' : '◌'}</span>
      <div>
        <strong>
          {runtime?.separated ? '独立 API / Worker' : '控制面运行状态'} ·{' '}
          {active
            ? 'Worker 心跳正常'
            : failed
              ? '状态读取失败'
              : runtime?.controller_status === 'stale'
                ? 'Worker 心跳已过期'
                : 'Worker 状态未知'}
        </strong>
        <p>
          {runtime?.last_heartbeat_at
            ? `最后心跳 ${fmt(runtime.last_heartbeat_at)} · 代次 ${runtime.epoch}`
            : '正在读取控制器状态'}{' '}
          · 领导租约不能代替全栈 HA 与跨实例栅栏验收。
        </p>
      </div>
    </section>
  );
}
