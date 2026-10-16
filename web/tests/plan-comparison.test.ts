import assert from 'node:assert/strict';
import { test } from 'node:test';
import { queryPlan } from '../src/features/sql/planModel.ts';
import { comparePlanModels } from '../src/features/sql/planComparison.ts';

function plan(value: Record<string, unknown>) {
  const parsed = queryPlan([[[value]]]);
  assert(parsed);
  return parsed;
}
test('comparison keeps cost units separate and does not infer actuals from Explain', () => {
  const before = plan({
    Plan: { 'Node Type': 'Limit', 'Total Cost': 2, 'Plan Rows': 7, 'Actual Rows': 7 },
    'Execution Time': 1,
  });
  const after = plan({
    Plan: { 'Node Type': 'Limit', 'Total Cost': 1, 'Plan Rows': 3, 'Actual Rows': 3 },
    'Execution Time': 0.5,
  });
  const value = comparePlanModels(before, after, false, true);
  assert.equal(value.actualComparable, false);
  assert.equal(value.metrics.length, 2);
  assert.deepEqual(
    value.metrics.find((m) => m.unit === 'rows'),
    {
      label: '根节点估算行数',
      unit: 'rows',
      before: 7,
      after: 3,
      delta: -4,
      percent: (-4 / 7) * 100,
    },
  );
  assert(!value.metrics.some((m) => m.label.startsWith('实际')));
});
test('both Analyze plans compare available actuals without zero division or summing child time', () => {
  const before = plan({
    Plan: {
      'Node Type': 'Limit',
      'Total Cost': 0,
      'Actual Rows': 7,
      'Actual Loops': 1,
      'Actual Total Time': 2,
      Plans: [{ 'Node Type': 'Seq Scan', 'Actual Total Time': 100 }],
    },
    'Execution Time': 2,
  });
  const after = plan({
    Plan: {
      'Node Type': 'Limit',
      'Total Cost': 1,
      'Actual Rows': 3,
      'Actual Loops': 1,
      'Actual Total Time': 1,
    },
    'Execution Time': 1,
  });
  const value = comparePlanModels(before, after, true, true);
  assert.equal(value.actualComparable, true);
  assert.equal(value.metrics.find((m) => m.unit === 'cost')?.percent, null);
  assert.equal(value.metrics.find((m) => m.label === '根节点实际 ms/循环')?.before, 2);
  assert.equal(value.metrics.find((m) => m.label === '实际执行耗时')?.delta, -1);
  assert.equal(value.nodes.find((n) => n.path === '0.0')?.change, 'removed');
});
test('structural paths are labelled changes instead of guessed physical matches', () => {
  const before = plan({
    Plan: { 'Node Type': 'Index Scan', 'Relation Name': 'a', 'Index Name': 'idx_a' },
  });
  const after = plan({
    Plan: {
      'Node Type': 'Index Scan',
      'Relation Name': 'a',
      'Index Name': 'idx_b',
      Plans: [{ 'Node Type': 'Result' }],
    },
  });
  const value = comparePlanModels(before, after, false, false);
  assert.equal(value.nodes[0].change, 'changed_operator');
  assert.equal(value.nodes[1].change, 'added');
});
test('bounded plan parsing limits comparisons and excludes invalid numeric metrics', () => {
  const before = plan({
    Plan: {
      'Node Type': 'Append',
      'Total Cost': Number.POSITIVE_INFINITY,
      Plans: Array.from({ length: 300 }, () => ({ 'Node Type': 'Result' })),
    },
  });
  const after = plan({ Plan: { 'Node Type': 'Result', 'Total Cost': -1 } });
  const value = comparePlanModels(before, after, false, false);
  assert.equal(value.limited, true);
  assert(value.nodes.length <= 200);
  assert.equal(value.metrics.length, 0);
});
