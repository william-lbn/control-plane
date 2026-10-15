export type PlanNode = {
  id: string;
  label: string;
  relation: string;
  metrics: Record<string, number | string>;
  children: PlanNode[];
};
export type QueryPlan = {
  root: PlanNode;
  planningMs?: number;
  executionMs?: number;
  limited: boolean;
};

function record(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}
const numeric = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : undefined;

// Render only known fields of an actual Explain response. Do not execute SQL,
// infer actual statistics from estimates, or expand an unbounded plan tree.
export function queryPlan(rows: unknown[][]): QueryPlan | null {
  const top =
    rows.length === 1 && rows[0].length === 1 && Array.isArray(rows[0][0])
      ? record(rows[0][0][0])
      : null;
  if (!top) return null;
  let count = 0,
    limited = false;
  const node = (value: unknown, id: string, depth: number): PlanNode | null => {
    const data = record(value);
    if (!data || typeof data['Node Type'] !== 'string') return null;
    if (++count > 200 || depth > 32) {
      limited = true;
      return null;
    }
    const metrics: Record<string, number | string> = {};
    for (const field of [
      'Startup Cost',
      'Total Cost',
      'Plan Rows',
      'Plan Width',
      'Actual Startup Time',
      'Actual Total Time',
      'Actual Rows',
      'Actual Loops',
      'Shared Hit Blocks',
      'Shared Read Blocks',
      'Shared Dirtied Blocks',
      'Shared Written Blocks',
      'Temp Read Blocks',
      'Temp Written Blocks',
      'Rows Removed by Filter',
      'Rows Removed by Join Filter',
      'Sort Space Used',
    ]) {
      const n = numeric(data[field]);
      if (n !== undefined) metrics[field] = n;
    }
    for (const field of ['Join Type', 'Sort Method', 'Sort Space Type', 'Index Name'])
      if (typeof data[field] === 'string') metrics[field] = data[field].slice(0, 256);
    const children: PlanNode[] = [];
    if (Array.isArray(data.Plans))
      for (let n = 0; n < data.Plans.length; n++) {
        if (count >= 200) {
          limited = true;
          break;
        }
        const child = node(data.Plans[n], id + '.' + n, depth + 1);
        if (child) children.push(child);
      }
    return {
      id,
      label: data['Node Type'].slice(0, 256),
      relation:
        typeof data['Relation Name'] === 'string' ? data['Relation Name'].slice(0, 256) : '',
      metrics,
      children,
    };
  };
  const root = node(top.Plan, '0', 0);
  return root
    ? {
        root,
        planningMs: numeric(top['Planning Time']),
        executionMs: numeric(top['Execution Time']),
        limited,
      }
    : null;
}
