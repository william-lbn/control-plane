import assert from 'node:assert/strict';
import test from 'node:test';
import type { Endpoint, MetricHistory } from '../src/api.ts';
import { monitoringSnapshot } from '../src/features/monitoring/snapshot.ts';

const now = Date.parse('2026-10-04T08:00:00Z');
const endpoint = {
  id: 'ep_test',
  workload_kind: 'neonvm',
  runtime: {
    observed_state: 'active',
    observed_at: new Date(now).toISOString(),
    workload_uid: 'vm-new',
    workload_created_at: new Date(now - 20000).toISOString(),
  },
} as Endpoint;
const history = {
  endpoint_id: 'ep_test',
  fresh: true,
  items: [
    {
      sampled_at: new Date(now - 10000).toISOString(),
      observed_state: 'active',
      cpu_used_milli: 300,
    },
  ],
} as MetricHistory;

test('current active generation may present a fresh matching sample', () => {
  assert.equal(monitoringSnapshot(history, endpoint, now).current?.cpu_used_milli, 300);
});
test('suspended runtime cannot display historical active CPU as current usage', () => {
  const view = monitoringSnapshot(
    history,
    { ...endpoint, runtime: { ...endpoint.runtime, observed_state: 'suspended' } },
    now,
  );
  assert.equal(view.state, 'suspended');
  assert.equal(view.fresh, false);
  assert.equal(view.current, undefined);
  assert.equal(history.items[0].cpu_used_milli, 300, 'historical evidence is retained');
});
test('cold VM cannot inherit samples collected before its birth', () => {
  const cold = {
    ...endpoint,
    runtime: { ...endpoint.runtime, workload_created_at: new Date(now - 1000).toISOString() },
  };
  assert.equal(monitoringSnapshot(history, cold, now).current, undefined);
});
test('runtime failures and stale or future observations fail closed', () => {
  assert.equal(monitoringSnapshot(history, null, now).current, undefined);
  for (const observed_at of [
    new Date(now - 45001).toISOString(),
    new Date(now + 1).toISOString(),
    'invalid',
  ]) {
    assert.equal(
      monitoringSnapshot(
        history,
        { ...endpoint, runtime: { ...endpoint.runtime, observed_at } },
        now,
      ).current,
      undefined,
    );
  }
});
test('endpoint switches, missing VM identity and invalid sample time cannot reuse measurements', () => {
  assert.equal(
    monitoringSnapshot({ ...history, endpoint_id: 'ep_other' }, endpoint, now).current,
    undefined,
  );
  assert.equal(
    monitoringSnapshot(
      history,
      { ...endpoint, runtime: { ...endpoint.runtime, workload_uid: undefined } },
      now,
    ).current,
    undefined,
  );
  for (const sampled_at of [
    new Date(now + 1).toISOString(),
    new Date(now - 90001).toISOString(),
    'invalid',
  ]) {
    assert.equal(
      monitoringSnapshot(
        { ...history, items: [{ ...history.items[0], sampled_at }] },
        endpoint,
        now,
      ).current,
      undefined,
    );
  }
});
