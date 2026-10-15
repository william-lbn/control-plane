import { useEffect, useRef, useState } from 'react';
import { api, endpointPath } from '../../api';
import type { Endpoint, QueryResult } from '../../api';
import { PageHeading } from '../../shared/ui';
import { useEndpointSelection } from '../../shared/useEndpointSelection';
import { ScriptResults, type ScriptResult } from './ScriptResults';
import { ResultExports } from './ResultExports';
import { SavedQueries } from './SavedQueries';
import { QueryHistory } from './QueryHistory';
import { QueryPlan } from './QueryPlan';

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
  const [planResult, setPlanResult] = useState<{
    result: QueryResult;
    analyzed: boolean;
    sequence: number;
  } | null>(null);
  const planSequence = useRef(0);
  const [scriptResult, setScriptResult] = useState<ScriptResult | null>(null);
  const [mode, setMode] = useState<'single' | 'script'>('single');
  const [queryAction, setQueryAction] = useState<'query' | 'explain' | 'analyze'>('query');
  const scopeEpoch = useRef(0);
  const historyTarget = useRef<{
    endpoint_id: string;
    role_name: string;
    database_name: string;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [historyRevision, setHistoryRevision] = useState(0);
  const endpoint = endpoints.find((e) => e.id === selected) || endpoints[0];
  useEffect(() => {
    scopeEpoch.current++;
    setResult(null);
    setScriptResult(null);
    setPassword('');
    setQueryAction('query');
    const target =
      historyTarget.current?.endpoint_id === endpoint?.id ? historyTarget.current : null;
    setRole(target?.role_name || endpoint?.role_name || '');
    setDatabase(target?.database_name || endpoint?.database_name || '');
    historyTarget.current = null;
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
          body: JSON.stringify({
            password,
            sql:
              mode === 'single' && queryAction !== 'query'
                ? `EXPLAIN (${queryAction === 'analyze' ? 'ANALYZE, BUFFERS, ' : ''}FORMAT JSON) ${sql}`
                : sql,
            role,
            database,
          }),
        },
      );
      if (epoch !== scopeEpoch.current) return;
      if (mode === 'script') setScriptResult(data as ScriptResult);
      else {
        const received = data as QueryResult;
        setResult(received);
        setPlanResult(
          queryAction === 'query'
            ? null
            : {
                result: received,
                analyzed: queryAction === 'analyze',
                sequence: ++planSequence.current,
              },
        );
      }
    } catch (e) {
      if (epoch === scopeEpoch.current) showError(e);
    } finally {
      // Passwords are single-request input, including rejected SQL/network
      // errors. Preserve a different value entered while the request waited.
      if (epoch === scopeEpoch.current) {
        setPassword((current) => (current === submittedPassword ? '' : current));
        setBusy(false);
        setHistoryRevision((value) => value + 1);
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
              setQueryAction('query');
              setResult(null);
              setScriptResult(null);
            }}
          >
            <option value="single">单条 SQL</option>
            <option value="script">SQL 脚本 · 逐条结果</option>
          </select>
        </label>
        {mode === 'single' && (
          <label>
            查询操作
            <select
              aria-label="查询操作"
              value={queryAction}
              disabled={busy}
              onChange={(e) => {
                setQueryAction(e.target.value as 'query' | 'explain' | 'analyze');
                setResult(null);
              }}
            >
              <option value="query">执行 SQL</option>
              <option value="explain">Explain · 估算计划</option>
              <option value="analyze">Analyze · 实际执行并分析</option>
            </select>
          </label>
        )}
        {mode === 'single' && queryAction === 'analyze' && (
          <p role="alert">
            Analyze 会实际执行当前语句，包括 INSERT / UPDATE / DELETE 的写入。提交后不会自动重试。
          </p>
        )}
        <SavedQueries
          projectId={projectId}
          sql={sql}
          onLoad={(value) => {
            setSQL(value);
            setMode('script');
            setPassword('');
            setResult(null);
            setScriptResult(null);
          }}
        />
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
            {busy
              ? '执行中…'
              : mode === 'script'
                ? '▶ 执行脚本'
                : queryAction === 'explain'
                  ? '▶ Explain'
                  : queryAction === 'analyze'
                    ? '▶ Analyze'
                    : '▶ 执行 SQL'}
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
          {planResult?.result === result && (
            <QueryPlan
              key={planResult.sequence}
              rows={result.rows}
              analyzed={planResult.analyzed}
            />
          )}
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
                      <td key={col}>
                        {value === null ? (
                          <em>NULL</em>
                        ) : typeof value === 'object' ? (
                          <pre>{JSON.stringify(value, null, 2)}</pre>
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
          <ResultExports result={result} />
        </div>
      )}
      {scriptResult && <ScriptResults result={scriptResult} />}
      <QueryHistory
        projectId={projectId}
        revision={historyRevision}
        onLoad={(text, entry) => {
          if (!endpoints.some((value) => value.id === entry.endpoint_id))
            throw new Error('原 Endpoint 已删除，不能自动恢复原连接目标。');
          setSQL(text);
          setMode(entry.mode);
          setQueryAction('query');
          setPassword('');
          setResult(null);
          setScriptResult(null);
          if (endpoint?.id !== entry.endpoint_id) historyTarget.current = entry;
          setSelected(entry.endpoint_id);
          setRole(entry.role_name);
          setDatabase(entry.database_name);
        }}
      />
      <div className="subtle-note">
        脚本按 PostgreSQL 原生语义执行，最多显示 16 组结果及 200 行，单元格 JSON 预算 1
        MiB。显示截断不会跳过后续命令。提交后连接中断时结果可能未知，请核验原数据，不要盲目重发。生产并发准入与独立
        Query Gateway 仍需验收。
      </div>
    </>
  );
}
