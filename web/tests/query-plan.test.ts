import assert from 'node:assert/strict';
import { test } from 'node:test';
import { queryPlan } from '../src/features/sql/planModel.ts';

test('plan tree keeps real inclusive metrics and does not invent actual values', () => {
  const plan = queryPlan([
    [
      [
        {
          Plan: {
            'Node Type': 'Limit',
            'Total Cost': 2,
            'Plan Rows': 7,
            Plans: [
              {
                'Node Type': 'Sort',
                'Actual Rows': 7,
                'Actual Loops': 1,
                'Actual Total Time': 0,
                'Shared Hit Blocks': 0,
                'Sort Method': 'quicksort',
              },
            ],
          },
          'Execution Time': 0,
        },
      ],
    ],
  ]);
  assert(plan);
  assert.equal(plan.executionMs, 0);
  assert.equal(plan.root.metrics['Actual Rows'], undefined);
  assert.equal(plan.root.children[0].metrics['Actual Total Time'], 0);
  assert.equal(plan.root.children[0].metrics['Shared Hit Blocks'], 0);
  assert.equal(plan.root.children[0].id, '0.0');
  assert.equal(queryPlan([['ordinary text']]), null);
  assert.equal(queryPlan([[[{ Plan: null }]]]), null);
});
test('plan renderer input is bounded and rejects invalid metrics and child shapes', () => {
  const plan = queryPlan([
    [
      [
        {
          Plan: {
            'Node Type': 'Root',
            'Actual Rows': -1,
            'Total Cost': Infinity,
            Plans: Array.from({ length: 250 }, () => ({ 'Node Type': 'Child', Plans: ['bad'] })),
          },
        },
      ],
    ],
  ]);
  assert(plan?.limited);
  assert.equal(plan.root.children.length, 199);
  assert.equal(plan.root.metrics['Actual Rows'], undefined);
  assert.equal(plan.root.metrics['Total Cost'], undefined);
  let deep: unknown = { 'Node Type': 'Leaf' };
  for (let n = 0; n < 40; n++) deep = { 'Node Type': 'Parent', Plans: [deep] };
  assert(queryPlan([[[{ Plan: deep }]]])?.limited);
});
