import { useEffect, useState } from 'react';
import { api, endpointPath } from '../../api';
import type { Endpoint, QueryResult } from '../../api';
import { PageHeading } from '../../shared/ui';
import { useEndpointSelection } from '../../shared/useEndpointSelection';

export function Workbench({
  projectId,
  endpoints,
  branchName,
  showError,
}: {
  projectId: string;
  endpoints: Endpoint[];
  branchName: (id: string) => string;
  showError: (error: unknown) => void;
}) {
  const [selected, setSelected] = useEndpointSelection(projectId, endpoints);
  const [password, setPassword] = useState('');
  const [role, setRole] = useState('');
  const [database, setDatabase] = useState('');
  const [sql, setSQL] = useState('SELECT current_database(), current_user, now();');
  const [result, setResult] = useState<QueryResult | null>(null);
  const [busy, setBusy] = useState(false);
  const endpoint = endpoints.find((e) => e.id === selected) || endpoints[0];
  useEffect(() => {
    setResult(null);
    setPassword('');
    setRole(endpoint?.role_name || '');
    setDatabase(endpoint?.database_name || '');
  }, [endpoint?.id]);
  async function execute() {
    if (!endpoint) return;
    const submittedPassword = password;
    setBusy(true);
    setResult(null);
    try {
      const data = await api<QueryResult>(endpointPath(projectId, endpoint.id) + '/query', {
        method: 'POST',
        body: JSON.stringify({ password, sql, role, database }),
      });
      setResult(data);
    } catch (e) {
      showError(e);
    } finally {
      // Passwords are single-request input, including rejected SQL/network
      // errors. Preserve a different value entered while the request waited.
      setPassword((current) => (current === submittedPassword ? '' : current));
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeading
        kicker="PROJECT / SQL EDITOR"
        title="SQL 工作台"
        description="请求通过 Neon Proxy 路由到当前分支的 Compute。"
      />
      <div className="selector-row">
        <label>
          目标 Endpoint
          <select
            disabled={busy}
            value={endpoint?.id || ''}
            onChange={(e) => setSelected(e.target.value)}
          >
            {endpoints.map((e) => (
              <option key={e.id} value={e.id}>
                {branchName(e.branch_id)} · {e.endpoint_type === 'read_only' ? '只读' : '读写'} ·{' '}
                {e.selector}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div className="selector-row">
        <label>
          数据库角色
          <input
            disabled={busy}
            autoComplete="off"
            value={role}
            onChange={(e) => {
              setRole(e.target.value);
              setPassword('');
              setResult(null);
            }}
          />
        </label>
        <label>
          目标数据库
          <input
            disabled={busy}
            autoComplete="off"
            value={database}
            onChange={(e) => {
              setDatabase(e.target.value);
              setPassword('');
              setResult(null);
            }}
          />
        </label>
      </div>
      <div className="panel query-panel">
        <div className="query-toolbar">
          <span>SQL QUERY</span>
          <span>最多返回 200 行</span>
        </div>
        <textarea
          spellCheck={false}
          value={sql}
          onChange={(e) => setSQL(e.target.value)}
          aria-label="SQL 查询"
        />
        <div className="query-actions">
          <label>
            数据库角色密码
            <input
              type="password"
              autoComplete="off"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="仅用于本次查询"
            />
          </label>
          <button
            className="button primary"
            disabled={busy || !password || !sql.trim()}
            onClick={execute}
          >
            {busy ? '执行中…' : '▶ 执行 SQL'}
          </button>
        </div>
      </div>
      {result && (
        <div className="panel result-panel">
          <div className="panel-heading">
            <h2>查询结果</h2>
            <span>
              {result.command} · {result.rows.length} 行{result.truncated ? ' · 已截断' : ''}
            </span>
          </div>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  {result.columns.map((col, index) => (
                    <th key={index}>{col}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {result.rows.map((row, index) => (
                  <tr key={index}>
                    {row.map((value, col) => (
                      <td key={col}>{value === null ? <em>NULL</em> : String(value)}</td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      <div className="subtle-note">
        此工作台是受控实验室能力。生产版将增加独立 Query Gateway、语句/字节/并发限制和细粒度授权。
      </div>
    </>
  );
}
