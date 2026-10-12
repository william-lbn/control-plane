import { useEffect, useRef, useState } from 'react';
import { api, endpointPath } from '../../api';
import type { Endpoint, QueryResult } from '../../api';
import { PageHeading } from '../../shared/ui';
import { useEndpointSelection } from '../../shared/useEndpointSelection';
import { ScriptResults, type ScriptResult } from './ScriptResults';

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
  const [scriptResult, setScriptResult] = useState<ScriptResult | null>(null);
  const [mode, setMode] = useState<'single' | 'script'>('single');
  const scopeEpoch = useRef(0);
  const [busy, setBusy] = useState(false);
  const endpoint = endpoints.find((e) => e.id === selected) || endpoints[0];
  useEffect(() => {
    scopeEpoch.current++;
    setResult(null);
    setScriptResult(null);
    setPassword('');
    setRole(endpoint?.role_name || '');
    setDatabase(endpoint?.database_name || '');
    setBusy(false);
    return () => {
      scopeEpoch.current++;
    };
  }, [projectId, endpoint?.id]);
  async function execute() {
    if (!endpoint) return;
    const epoch = scopeEpoch.current;
    const submittedPassword = password;
    setBusy(true);
    setResult(null);
    setScriptResult(null);
    try {
      const data = await api<QueryResult | ScriptResult>(
        endpointPath(projectId, endpoint.id) + (mode === 'script' ? '/query-script' : '/query'),
        {
          method: 'POST',
          body: JSON.stringify({ password, sql, role, database }),
        },
      );
      if (epoch !== scopeEpoch.current) return;
      if (mode === 'script') setScriptResult(data as ScriptResult);
      else setResult(data as QueryResult);
    } catch (e) {
      if (epoch === scopeEpoch.current) showError(e);
    } finally {
      // Passwords are single-request input, including rejected SQL/network
      // errors. Preserve a different value entered while the request waited.
      if (epoch === scopeEpoch.current) {
        setPassword((current) => (current === submittedPassword ? '' : current));
        setBusy(false);
      }
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
              setScriptResult(null);
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
              setScriptResult(null);
            }}
          />
        </label>
      </div>
      <div className="panel query-panel">
        <label>
          执行模式
          <select
            aria-label="执行模式"
            value={mode}
            disabled={busy}
            onChange={(e) => {
              setMode(e.target.value as 'single' | 'script');
              setResult(null);
              setScriptResult(null);
            }}
          >
            <option value="single">单条 SQL</option>
            <option value="script">SQL 脚本 · 逐条结果</option>
          </select>
        </label>
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
            {busy ? '执行中…' : mode === 'script' ? '▶ 执行脚本' : '▶ 执行 SQL'}
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
      {scriptResult && <ScriptResults result={scriptResult} />}
      <div className="subtle-note">
        脚本按 PostgreSQL 原生语义执行，最多显示 16 组结果及 200 行，单元格 JSON 预算 1
        MiB。显示截断不会跳过后续命令。提交后连接中断时结果可能未知，请核验原数据，不要盲目重发。生产并发准入与独立
        Query Gateway 仍需验收。
      </div>
    </>
  );
}
