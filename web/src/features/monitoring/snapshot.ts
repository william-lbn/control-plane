import type { Endpoint, MetricHistory } from '../../api.ts';

// Historical samples survive suspension and cold start. They must never be
// presented as the current VM's measurements after a generation/state change.
export function monitoringSnapshot(
  history: MetricHistory | null,
  endpoint: Endpoint | null,
  now = Date.now(),
) {
  const state = endpoint?.runtime.observed_state || 'unknown';
  const sample = history?.items.at(-1);
  const observedAt = Date.parse(endpoint?.runtime.observed_at || '');
  const sampledAt = Date.parse(sample?.sampled_at || '');
  const born = Date.parse(endpoint?.runtime.workload_created_at || '');
  const fresh = Boolean(
    history?.fresh &&
      endpoint?.id === history.endpoint_id &&
      state === 'active' &&
      sample?.observed_state === 'active' &&
      Number.isFinite(observedAt) &&
      observedAt <= now &&
      now - observedAt <= 45000 &&
      Number.isFinite(sampledAt) &&
      sampledAt <= now &&
      now - sampledAt <= 90000 &&
      (endpoint.workload_kind !== 'neonvm' ||
        (endpoint.runtime.workload_uid && Number.isFinite(born) && sampledAt >= born)),
  );
  return { state, fresh, current: fresh ? sample : undefined };
}
