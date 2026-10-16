import type { PlanNode, QueryPlan } from './planModel';

export type MetricDelta = {
  label: string;
  unit: string;
  before: number;
  after: number;
  delta: number;
  percent: number | null;
};
export type NodeDelta = {
  path: string;
  before: PlanNode | null;
  after: PlanNode | null;
  change: 'same_operator' | 'changed_operator' | 'added' | 'removed';
};
const number = (value: unknown): number | undefined =>
  typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : undefined;

// Compare already bounded, parsed JSON plans. A structural path is navigation,
// not a claim that differently ordered/rewritten nodes are the same operation.
export function comparePlanModels(
  before: QueryPlan,
  after: QueryPlan,
  beforeAnalyzed: boolean,
  afterAnalyzed: boolean,
) {
  const metrics: MetricDelta[] = [];
  function metric(label: string, unit: string, left: unknown, right: unknown) {
    const a = number(left),
      b = number(right);
    if (a === undefined || b === undefined) return;
    metrics.push({
      label,
      unit,
      before: a,
      after: b,
      delta: b - a,
      percent: a === 0 ? null : ((b - a) / a) * 100,
    });
  }
  metric(
    '根节点累计估算成本',
    'cost',
    before.root.metrics['Total Cost'],
    after.root.metrics['Total Cost'],
  );
  metric(
    '根节点估算行数',
    'rows',
    before.root.metrics['Plan Rows'],
    after.root.metrics['Plan Rows'],
  );
  metric('规划耗时', 'ms', before.planningMs, after.planningMs);
  const actualComparable = beforeAnalyzed && afterAnalyzed;
  if (actualComparable) {
    metric('实际执行耗时', 'ms', before.executionMs, after.executionMs);
    metric(
      '根节点实际行/循环',
      'rows/loop',
      before.root.metrics['Actual Rows'],
      after.root.metrics['Actual Rows'],
    );
    metric(
      '根节点实际循环',
      'loops',
      before.root.metrics['Actual Loops'],
      after.root.metrics['Actual Loops'],
    );
    metric(
      '根节点实际 ms/循环',
      'ms/loop',
      before.root.metrics['Actual Total Time'],
      after.root.metrics['Actual Total Time'],
    );
  }
  function flatten(plan: QueryPlan) {
    const values = new Map<string, PlanNode>();
    const visit = (node: PlanNode) => {
      if (values.size >= 200) return;
      values.set(node.id, node);
      for (const child of node.children) visit(child);
    };
    visit(plan.root);
    return values;
  }
  const left = flatten(before),
    right = flatten(after),
    paths = [...new Set([...left.keys(), ...right.keys()])];
  const nodes: NodeDelta[] = paths.slice(0, 200).map((path) => {
    const a = left.get(path) ?? null,
      b = right.get(path) ?? null;
    return {
      path,
      before: a,
      after: b,
      change: !a
        ? 'added'
        : !b
          ? 'removed'
          : a.label === b.label &&
              a.relation === b.relation &&
              a.metrics['Index Name'] === b.metrics['Index Name']
            ? 'same_operator'
            : 'changed_operator',
    };
  });
  return {
    metrics,
    nodes,
    actualComparable,
    limited: before.limited || after.limited || paths.length > 200,
  };
}
