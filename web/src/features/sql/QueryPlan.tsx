import { useState } from 'react';
import { queryPlan, type PlanNode } from './planModel';
import './query-plan.css';

function Node({ node, analyzed }: { node: PlanNode; analyzed: boolean }) {
  const [open, setOpen] = useState(true);
  const m = node.metrics;
  return (
    <li className="query-plan-node">
      <article>
        <header>
          {node.children.length > 0 ? (
            <button
              className="button"
              aria-expanded={open}
              aria-controls={'plan-' + node.id}
              aria-label={(open ? '折叠 ' : '展开 ') + node.label}
              onClick={() => setOpen(!open)}
            >
              {open ? '−' : '+'}
            </button>
          ) : (
            <span className="query-plan-leaf">•</span>
          )}
          <strong>{node.label}</strong>
          {node.relation && <code>{node.relation}</code>}
        </header>
        <dl>
          {m['Total Cost'] !== undefined && (
            <div>
              <dt>累计估算成本</dt>
              <dd>
                {m['Startup Cost'] ?? '—'} → {m['Total Cost']}
              </dd>
            </div>
          )}
          {m['Plan Rows'] !== undefined && (
            <div>
              <dt>估算行数</dt>
              <dd>{m['Plan Rows']}</dd>
            </div>
          )}
          {analyzed && m['Actual Rows'] !== undefined && (
            <div>
              <dt>实际行/循环</dt>
              <dd>{m['Actual Rows']}</dd>
            </div>
          )}
          {analyzed && m['Actual Loops'] !== undefined && (
            <div>
              <dt>实际循环</dt>
              <dd>{m['Actual Loops']}</dd>
            </div>
          )}
          {analyzed && m['Actual Total Time'] !== undefined && (
            <div>
              <dt>实际 ms/循环</dt>
              <dd>
                {m['Actual Startup Time'] ?? '—'} → {m['Actual Total Time']}
              </dd>
            </div>
          )}
          {Object.entries(m)
            .filter(
              ([key]) =>
                ![
                  'Startup Cost',
                  'Total Cost',
                  'Plan Rows',
                  'Plan Width',
                  'Actual Rows',
                  'Actual Loops',
                  'Actual Startup Time',
                  'Actual Total Time',
                ].includes(key),
            )
            .map(([key, value]) => (
              <div key={key}>
                <dt>{key}</dt>
                <dd>{value}</dd>
              </div>
            ))}
        </dl>
      </article>
      {node.children.length > 0 && open && (
        <ul id={'plan-' + node.id}>
          {node.children.map((child) => (
            <Node key={child.id} node={child} analyzed={analyzed} />
          ))}
        </ul>
      )}
    </li>
  );
}

export function QueryPlan({ rows, analyzed }: { rows: unknown[][]; analyzed: boolean }) {
  const plan = queryPlan(rows);
  if (!plan) return <p role="status">计划格式无法可视化，请查看下面的原始 JSON。</p>;
  return (
    <section className="query-plan" aria-label="SQL 执行计划树">
      <div className="panel-heading">
        <h3>{analyzed ? '实际执行计划' : '估算执行计划'}</h3>
        <span>
          {plan.planningMs === undefined ? '' : `规划 ${plan.planningMs} ms`}
          {plan.executionMs === undefined ? '' : ` · 执行 ${plan.executionMs} ms`}
        </span>
      </div>
      <p>
        {analyzed
          ? 'Analyze 已实际执行语句。实际行数和耗时按每次循环报告，父子节点耗时有包含关系，不能简单相加。'
          : 'Explain 未执行目标语句。成本是规划器估算单位，不是毫秒；没有实际执行指标。'}
      </p>
      {plan.limited && (
        <p role="status">计划树仅展示前 200 个节点及 32 层深度；原始 JSON 保留完整返回值。</p>
      )}
      <ul>
        <Node key={plan.root.id} node={plan.root} analyzed={analyzed} />
      </ul>
    </section>
  );
}
