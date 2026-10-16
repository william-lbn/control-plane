import { useState } from 'react';
import { queryPlan, type QueryPlan } from './planModel';
import { comparePlanModels } from './planComparison';

type Current = { rows: unknown[][]; analyzed: boolean; sequence: number };
type Snapshot = { plan: QueryPlan; analyzed: boolean; sequence: number };
const format = (value: number) => value.toLocaleString(undefined, { maximumFractionDigits: 3 });
export function PlanComparison({ current }: { current: Current | null }) {
  const [baseline, setBaseline] = useState<Snapshot | null>(null);
  const parsed = current ? queryPlan(current.rows) : null;
  const comparison =
    baseline && parsed && current
      ? comparePlanModels(baseline.plan, parsed, baseline.analyzed, current.analyzed)
      : null;
  return (
    <section
      className="panel query-plan"
      aria-label="SQL 执行计划比较"
      data-testid="plan-comparison"
    >
      <div className="panel-heading">
        <h3>执行计划比较</h3>
        <span>浏览器内存 · 不自动执行</span>
      </div>
      <p>
        先取得 Explain/Analyze
        结果并设为基线，再手动取得另一份计划。基线仅在当前页面内存中保存；切换项目、Endpoint、角色或数据库及重载会清除。成本不是毫秒，单次耗时差不能证明性能提升。
      </p>
      <button
        className="button"
        disabled={!parsed || !current}
        onClick={() => {
          if (parsed && current)
            setBaseline({ plan: parsed, analyzed: current.analyzed, sequence: current.sequence });
        }}
      >
        设为比较基线
      </button>
      <button className="button" disabled={!baseline} onClick={() => setBaseline(null)}>
        清除比较基线
      </button>
      {baseline && (
        <p data-testid="plan-baseline">
          基线 #{baseline.sequence} · {baseline.analyzed ? 'Analyze' : 'Explain'}
        </p>
      )}
      {baseline && !current && <p>基线已保留，请手动取得当前计划；此面板不会提交 SQL。</p>}
      {comparison && current && (
        <>
          <p data-testid="plan-comparison-versions">
            基线 #{baseline!.sequence} → 当前 #{current.sequence}
            {baseline!.sequence === current.sequence ? '（当前即基线）' : ''}
          </p>
          {!comparison.actualComparable && (
            <p role="status">
              两次均为 Analyze 时才比较实际指标；当前仅比较双方都有的估算与规划信息。
            </p>
          )}
          {comparison.limited && (
            <p role="status">
              计划已触及节点/深度限制，比较只覆盖可见的有界部分；请结合原始 JSON 检查。
            </p>
          )}
          <div className="table-scroll">
            <table aria-label="计划指标差异">
              <thead>
                <tr>
                  <th>指标</th>
                  <th>基线</th>
                  <th>当前</th>
                  <th>差值（当前−基线）</th>
                  <th>变化</th>
                </tr>
              </thead>
              <tbody>
                {comparison.metrics.map((metric) => (
                  <tr
                    key={metric.label}
                    data-testid={'plan-delta-' + metric.unit + '-' + metric.label}
                  >
                    <th>{metric.label}</th>
                    <td>
                      {format(metric.before)} {metric.unit}
                    </td>
                    <td>
                      {format(metric.after)} {metric.unit}
                    </td>
                    <td>
                      {format(metric.delta)} {metric.unit}
                    </td>
                    <td>
                      {metric.percent === null ? '—（基线为 0）' : format(metric.percent) + '%'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p>
            以下按树路径对齐，路径相同不保证是同一物理操作；节点可能改写或重排。父子成本和耗时存在包含关系，不做简单相加或自动优化结论。
          </p>
          <div className="table-scroll">
            <table aria-label="计划结构差异">
              <thead>
                <tr>
                  <th>路径</th>
                  <th>基线节点</th>
                  <th>当前节点</th>
                  <th>结构观察</th>
                </tr>
              </thead>
              <tbody>
                {comparison.nodes.map((node) => (
                  <tr key={node.path}>
                    <td>
                      <code>{node.path}</code>
                    </td>
                    <td>
                      {node.before
                        ? [
                            node.before.label,
                            node.before.relation,
                            node.before.metrics['Index Name'],
                          ]
                            .filter(Boolean)
                            .join(' · ')
                        : '—'}
                    </td>
                    <td>
                      {node.after
                        ? [node.after.label, node.after.relation, node.after.metrics['Index Name']]
                            .filter(Boolean)
                            .join(' · ')
                        : '—'}
                    </td>
                    <td>
                      {
                        {
                          same_operator: '同路径同算子',
                          changed_operator: '同路径算子/关系变化',
                          added: '新增路径',
                          removed: '移除路径',
                        }[node.change]
                      }
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}
