import type { QueryResult } from '../../api';
import { ResultExports } from './ResultExports';

export type ScriptResult = {
  outcome: 'completed' | 'failed';
  results: (QueryResult & { ordinal: number })[];
  statements_completed: number;
  results_truncated: boolean;
  open_transaction_discarded: boolean;
  error?: { code: string; sqlstate: string };
};

// Only data is rendered. Script text, results and server error values never
// become HTML or an executable Console origin response.
export function ScriptResults({ result }: { result: ScriptResult }) {
  return (
    <section className="panel result-panel" data-testid="sql-script-results">
      <div className="panel-heading">
        <h2>脚本结果</h2>
        <span>
          {result.outcome === 'completed' ? '已完成' : '未全部完成'} · {result.statements_completed}{' '}
          条命令完成{result.results_truncated ? ' · 显示已截断' : ''}
        </span>
      </div>
      {result.error && (
        <p role="alert">
          {result.error.code}
          {result.error.sqlstate ? ` · SQLSTATE ${result.error.sqlstate}` : ''}。此前显式 COMMIT
          的内容可能已生效；请核验数据，不要盲目重发。
        </p>
      )}
      {result.open_transaction_discarded && (
        <p role="alert">脚本留下未关闭事务，本次连接关闭时会回滚该事务；此前显式提交不受影响。</p>
      )}
      {result.results.map((statement) => (
        <div key={statement.ordinal} data-testid={`sql-statement-${statement.ordinal}`}>
          <h3>
            结果 {statement.ordinal} · {statement.command || '语句结果'} · {statement.rows.length}{' '}
            行{statement.truncated ? ' · 已截断' : ''}
          </h3>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  {statement.columns.map((name, index) => (
                    <th key={index}>{name}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {statement.rows.map((row, index) => (
                  <tr key={index}>
                    {row.map((value, column) => (
                      <td key={column}>
                        {value === null ? (
                          <em>NULL</em>
                        ) : typeof value === 'object' ? (
                          JSON.stringify(value)
                        ) : (
                          String(value)
                        )}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <ResultExports result={statement} />
        </div>
      ))}
    </section>
  );
}
