import { useEffect, useRef, useState } from 'react';
import { api, projectPath, route } from '../../api';
import type { Branch } from '../../api';
import { PageHeading, Empty, fmt } from '../../shared/ui';
import {
  playgroundRequest,
  protocolAnswer,
  protocolDelta,
  protocolNames,
  usdToMicros,
} from './protocols';
import type { AIProtocol } from './protocols';
import { newRequestKey } from '../../shared/requestKey';

type Model = { id: string; name: string; max_output_tokens: number; protocols: string[] };
type Catalog = { enabled: boolean; models: Model[]; public_endpoint: string };
type Budget = {
  policy: {
    version: number;
    enabled: boolean;
    limit_micros: number;
    spent_micros: number;
    reserved_micros: number;
  };
  reservations: {
    request_id: string;
    state: string;
    reserved_micros: number;
    actual_micros: number | null;
    needs_reconciliation: boolean;
  }[];
  can_configure: boolean;
  can_reconcile: boolean;
};
type Usage = {
  items: {
    id: string;
    model_id: string;
    state: string;
    total_tokens: number | null;
    duration_ms: number | null;
    started_at: string;
  }[];
  summary: {
    requests: number;
    completed: number;
    unknown_usage: number;
    known_total_tokens: number;
  };
};

export function AIGateway({
  projectId,
  branches,
  canEdit,
}: {
  projectId: string;
  branches: Branch[];
  canEdit: boolean;
}) {
  const [branch, setBranch] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [usage, setUsage] = useState<Usage | null>(null);
  const [model, setModel] = useState('');
  const [prompt, setPrompt] = useState('');
  const [answer, setAnswer] = useState('');
  const [stream, setStream] = useState(true);
  const [protocol, setProtocol] = useState<AIProtocol>('chat_completions');
  const [advanced, setAdvanced] = useState(false);
  const [requestJSON, setRequestJSON] = useState('');
  const [budget, setBudget] = useState<Budget | null>(null);
  const [budgetEnabled, setBudgetEnabled] = useState(false);
  const [budgetUSD, setBudgetUSD] = useState('0');
  const [budgetBusy, setBudgetBusy] = useState(false);
  const [reconcileRequest, setReconcileRequest] = useState('');
  const [reconcileEvidence, setReconcileEvidence] = useState('');
  const [reconcileUsage, setReconcileUsage] = useState(
    '{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}',
  );
  const [notAccepted, setNotAccepted] = useState(false);
  const pendingBudget = useRef<{ path: string; body: string; key: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [requestId, setRequestId] = useState('');
  const abort = useRef<AbortController | null>(null);
  const prefix = projectPath(projectId) + `/branches/${encodeURIComponent(branch)}/ai-gateway`;
  useEffect(() => {
    let cancelled = false;
    setCatalog(null);
    setUsage(null);
    setBudget(null);
    setReconcileRequest('');
    pendingBudget.current = null;
    setPrompt('');
    setAnswer('');
    setError('');
    setRequestId('');
    Promise.all([
      api<Catalog>(prefix),
      api<Usage>(prefix + '/usage'),
      api<Budget>(prefix + '/budget'),
    ])
      .then(([c, u, b]) => {
        if (cancelled) return;
        setCatalog(c);
        setModel(c.models[0]?.id || '');
        setUsage(u);
        setBudget(b);
        setBudgetEnabled(b.policy.enabled);
        setBudgetUSD((b.policy.limit_micros / 1000000).toFixed(6));
      })
      .catch((e) => {
        if (!cancelled) setError(String(e));
      });
    return () => {
      cancelled = true;
      abort.current?.abort();
    };
  }, [prefix]);
  async function budgetMutation(path: string, method: string, value: unknown) {
    const body = JSON.stringify(value);
    if (
      !pendingBudget.current ||
      pendingBudget.current.path !== path ||
      pendingBudget.current.body !== body
    )
      pendingBudget.current = { path, body, key: newRequestKey() };
    await api(path, { method, body, headers: { 'Idempotency-Key': pendingBudget.current.key } });
    pendingBudget.current = null;
    const b = await api<Budget>(prefix + '/budget');
    setBudget(b);
    setBudgetEnabled(b.policy.enabled);
    setBudgetUSD((b.policy.limit_micros / 1000000).toFixed(6));
  }
  async function saveBudget() {
    if (!budget) return;
    setBudgetBusy(true);
    setError('');
    try {
      await budgetMutation(prefix + '/budget', 'PUT', {
        version: budget.policy.version,
        enabled: budgetEnabled,
        limit_micros: usdToMicros(budgetUSD),
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBudgetBusy(false);
    }
  }
  async function reconcileBudget() {
    setBudgetBusy(true);
    setError('');
    try {
      await budgetMutation(
        prefix + '/requests/' + encodeURIComponent(reconcileRequest) + '/reconcile',
        'POST',
        {
          confirmed_not_accepted: notAccepted,
          ...(notAccepted ? {} : { usage: JSON.parse(reconcileUsage) }),
          evidence_sha256: reconcileEvidence,
        },
      );
      setReconcileRequest('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBudgetBusy(false);
    }
  }
  useEffect(() => {
    const selected = catalog?.models.find((m) => m.id === model);
    if (selected && !selected.protocols.includes(protocol))
      setProtocol((selected.protocols[0] || 'chat_completions') as AIProtocol);
    setAdvanced(false);
    setRequestJSON('');
  }, [catalog, model, protocol]);
  async function invoke() {
    setBusy(true);
    setError('');
    setAnswer('');
    setRequestId('');
    const controller = new AbortController();
    abort.current = controller;
    try {
      const csrf =
        document.cookie
          .split('; ')
          .find((v) => v.startsWith('neon_v2_csrf='))
          ?.slice('neon_v2_csrf='.length) || '';
      const selected = catalog?.models.find((m) => m.id === model);
      const streaming = stream && protocol !== 'embeddings' && protocol !== 'gemini';
      const payload = advanced
        ? JSON.parse(requestJSON)
        : playgroundRequest(protocol, model, prompt, selected?.max_output_tokens || 128, streaming);
      if (payload.model !== model) throw new Error('请求 JSON 的 model 必须与当前模型一致');
      if (Boolean(payload.stream) !== streaming)
        throw new Error('请求 JSON 的 stream 必须与流式选项一致');
      const response = await fetch(
        prefix + '/request' + (protocol === 'chat_completions' ? '' : '/' + protocol),
        {
          method: 'POST',
          credentials: 'same-origin',
          signal: controller.signal,
          headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': decodeURIComponent(csrf) },
          body: JSON.stringify(payload),
        },
      );
      if (!response.ok) {
        const data = await response.json();
        throw new Error(data.error?.message || `HTTP ${response.status}`);
      }
      setRequestId(response.headers.get('X-Request-ID') || '');
      if (!streaming) {
        const data = await response.json();
        setAnswer(protocolAnswer(protocol, data));
      } else {
        const reader = response.body?.getReader();
        if (!reader) throw new Error('响应流不可用');
        const decoder = new TextDecoder();
        let pending = '';
        let done = false;
        let text = '';
        while (!done) {
          const chunk = await reader.read();
          if (chunk.done) break;
          pending += decoder.decode(chunk.value, { stream: true });
          let end: number;
          while ((end = pending.indexOf('\n\n')) >= 0) {
            const event = pending.slice(0, end);
            pending = pending.slice(end + 2);
            const data = event
              .split('\n')
              .find((v) => v.startsWith('data:'))
              ?.slice(5)
              .trim();
            if (!data) continue;
            if (data === '[DONE]') {
              done = true;
              break;
            }
            const value = JSON.parse(data);
            const delta = protocolDelta(protocol, value);
            text += delta.text;
            done = delta.done;
            setAnswer(text);
          }
        }
        if (!done) throw new Error('推理流未正常结束；不会自动重试');
      }
      setUsage(await api<Usage>(prefix + '/usage'));
      setBudget(await api<Budget>(prefix + '/budget'));
    } catch (e) {
      if (!controller.signal.aborted) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <div className="ai-workspace">
      <PageHeading
        kicker="BACKEND / AI GATEWAY"
        title="AI Gateway"
        description="统一分支入口 · 真实模型推理 · 应用凭据与用量记录"
      />
      <section className="panel">
        <label>
          AI 分支
          <select
            aria-label="AI 分支"
            value={branch}
            disabled={busy}
            onChange={(e) => setBranch(e.target.value)}
          >
            {branches
              .filter((b) => b.state === 'ready')
              .map((b) => (
                <option key={b.id} value={b.id}>
                  {b.name}
                </option>
              ))}
          </select>
        </label>
      </section>
      {error && (
        <div role="alert" className="subtle-note" data-testid="ai-error">
          {error}
        </div>
      )}
      {catalog && !catalog.enabled && (
        <Empty
          title="尚未配置模型上游"
          message="平台运营方配置真实供应商或自建模型后，模型目录和 Playground 将可用。"
        />
      )}
      {catalog?.enabled && (
        <>
          <div className="hero-panel">
            <div>
              <span className="eyebrow">BRANCH INFERENCE</span>
              <h2>从分支直接调用模型</h2>
              <p>模型调用独立于数据库 Compute。提示词和回复不写入平台日志或用量表。</p>
              <code>
                {location.origin}
                {catalog.public_endpoint}
              </code>
              <p>
                <a href={route(projectId, 'credentials')}>创建应用凭据 ↗</a>
              </p>
            </div>
          </div>
          <div className="stats-grid">
            <div className="stat-card">
              <small>24h 请求</small>
              <strong>{usage?.summary.requests || 0}</strong>
            </div>
            <div className="stat-card">
              <small>完成请求</small>
              <strong>{usage?.summary.completed || 0}</strong>
            </div>
            <div className="stat-card">
              <small>已知 Token</small>
              <strong>{usage?.summary.known_total_tokens || 0}</strong>
            </div>
            <div className="stat-card">
              <small>用量未确定</small>
              <strong>{usage?.summary.unknown_usage || 0}</strong>
            </div>
          </div>
          <section className="panel ai-playground">
            <div className="panel-heading">
              <h2>Playground</h2>
              <span className="tag">{protocolNames[protocol]}</span>
            </div>
            <label>
              模型
              <select
                aria-label="AI 模型"
                value={model}
                disabled={busy}
                onChange={(e) => setModel(e.target.value)}
              >
                {catalog.models.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name} · {m.id}
                  </option>
                ))}
              </select>
            </label>
            <label>
              AI 协议
              <select
                aria-label="AI 协议"
                value={protocol}
                disabled={busy}
                onChange={(e) => setProtocol(e.target.value as AIProtocol)}
              >
                {(
                  catalog.models.find((m) => m.id === model)?.protocols || ['chat_completions']
                ).map((p) => (
                  <option key={p} value={p}>
                    {protocolNames[p as AIProtocol] || p}
                  </option>
                ))}
              </select>
            </label>
            <label>
              输入
              <textarea
                aria-label="AI 输入"
                rows={4}
                maxLength={4000}
                value={prompt}
                disabled={busy || advanced}
                onChange={(e) => setPrompt(e.target.value)}
              />
            </label>
            <label className="ai-stream-mode">
              <input
                aria-label="AI 流式输出"
                type="checkbox"
                checked={stream}
                disabled={busy || protocol === 'embeddings' || protocol === 'gemini'}
                onChange={(e) => setStream(e.target.checked)}
              />
              流式输出
            </label>
            <label>
              <input
                aria-label="AI 高级 JSON"
                type="checkbox"
                checked={advanced}
                disabled={busy}
                onChange={(e) => {
                  setAdvanced(e.target.checked);
                  setRequestJSON(
                    JSON.stringify(
                      playgroundRequest(
                        protocol,
                        model,
                        prompt,
                        catalog.models.find((m) => m.id === model)?.max_output_tokens || 128,
                        stream && protocol !== 'embeddings' && protocol !== 'gemini',
                      ),
                      null,
                      2,
                    ),
                  );
                }}
              />
              编辑原生请求 JSON（工具、内联图片等）
            </label>
            {advanced && (
              <textarea
                aria-label="AI 请求 JSON"
                rows={12}
                maxLength={32768}
                value={requestJSON}
                disabled={busy}
                onChange={(e) => setRequestJSON(e.target.value)}
              />
            )}
            <button
              className="button primary"
              disabled={!canEdit || !model || (!advanced && !prompt.trim()) || busy}
              onClick={() => void invoke()}
            >
              {busy ? '模型生成中…' : '发送到模型'}
            </button>
            {busy && (
              <button
                className="button"
                onClick={() => {
                  abort.current?.abort();
                  setBusy(false);
                }}
              >
                取消请求
              </button>
            )}
            <pre className="ai-response" data-testid="ai-answer">
              {answer || '模型输出将显示在这里'}
            </pre>
            {requestId && <small data-testid="ai-request-id">Request ID · {requestId}</small>}
          </section>
          <section className="table-card">
            <div className="panel-heading">
              <h2>最近请求</h2>
              <button
                className="button"
                onClick={() =>
                  void api<Usage>(prefix + '/usage')
                    .then(setUsage)
                    .catch((e) => setError(String(e)))
                }
              >
                刷新用量
              </button>
            </div>
            <table>
              <thead>
                <tr>
                  <th>请求</th>
                  <th>模型</th>
                  <th>状态</th>
                  <th>Token</th>
                  <th>耗时</th>
                  <th>时间</th>
                </tr>
              </thead>
              <tbody>
                {usage?.items.map((item) => (
                  <tr key={item.id}>
                    <td>{item.id}</td>
                    <td>{item.model_id}</td>
                    <td>{item.state}</td>
                    <td>{item.total_tokens ?? '未确定'}</td>
                    <td>{item.duration_ms === null ? '—' : `${item.duration_ms} ms`}</td>
                    <td>{fmt(item.started_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
          {budget && (
            <section className="panel" aria-label="AI 预算">
              <h2>累计预算（USD）</h2>
              <p>
                已结算 {(budget.policy.spent_micros / 1000000).toFixed(6)} · 已预留{' '}
                {(budget.policy.reserved_micros / 1000000).toFixed(6)}
                。累计额度不自动清零；未确定请求保留预留金额，等待运营方对账。
              </p>
              <label>
                <input
                  aria-label="启用 AI 预算"
                  type="checkbox"
                  checked={budgetEnabled}
                  disabled={!budget.can_configure || budgetBusy}
                  onChange={(e) => setBudgetEnabled(e.target.checked)}
                />
                启用累计预算
              </label>
              <label>
                额度（USD）
                <input
                  aria-label="AI 预算额度"
                  inputMode="decimal"
                  value={budgetUSD}
                  disabled={!budget.can_configure || budgetBusy}
                  onChange={(e) => setBudgetUSD(e.target.value)}
                />
              </label>
              <button
                className="button"
                disabled={!budget.can_configure || budgetBusy}
                onClick={() => void saveBudget()}
              >
                保存 AI 预算
              </button>
              <button
                className="button"
                disabled={budgetBusy}
                onClick={() =>
                  void api<Budget>(prefix + '/budget')
                    .then((b) => {
                      setBudget(b);
                      setBudgetEnabled(b.policy.enabled);
                      setBudgetUSD((b.policy.limit_micros / 1000000).toFixed(6));
                    })
                    .catch((e) => setError(String(e)))
                }
              >
                刷新预算
              </button>
              <table>
                <thead>
                  <tr>
                    <th>请求</th>
                    <th>结算状态</th>
                    <th>预留 USD</th>
                    <th>实际 USD</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {budget.reservations.map((item) => (
                    <tr key={item.request_id}>
                      <td>{item.request_id}</td>
                      <td>{item.state}</td>
                      <td>{(item.reserved_micros / 1000000).toFixed(6)}</td>
                      <td>
                        {item.actual_micros === null
                          ? '未确定'
                          : (item.actual_micros / 1000000).toFixed(6)}
                      </td>
                      <td>
                        {budget.can_reconcile && item.needs_reconciliation && (
                          <button
                            onClick={() => {
                              setReconcileRequest(item.request_id);
                              setReconcileEvidence('');
                              setNotAccepted(false);
                            }}
                          >
                            对账 {item.request_id}
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {reconcileRequest && (
                <fieldset>
                  <legend>运营方确认用量 · {reconcileRequest}</legend>
                  <p>
                    仅填写已由供应商记录确认的结果。证据原文保存在运营方受保护目录，此处只记录
                    SHA-256。
                  </p>
                  <label>
                    证据 SHA-256
                    <input
                      aria-label="AI 对账证据"
                      maxLength={64}
                      value={reconcileEvidence}
                      onChange={(e) => setReconcileEvidence(e.target.value)}
                    />
                  </label>
                  <label>
                    <input
                      aria-label="确认供应商未接受请求"
                      type="checkbox"
                      checked={notAccepted}
                      onChange={(e) => setNotAccepted(e.target.checked)}
                    />
                    确认供应商未接受请求
                  </label>
                  {!notAccepted && (
                    <label>
                      已确认 Token 用量 JSON
                      <textarea
                        aria-label="AI 对账用量"
                        value={reconcileUsage}
                        onChange={(e) => setReconcileUsage(e.target.value)}
                      />
                    </label>
                  )}
                  <button
                    className="button"
                    disabled={budgetBusy || !/^[a-f0-9]{64}$/.test(reconcileEvidence)}
                    onClick={() => void reconcileBudget()}
                  >
                    提交已确认对账
                  </button>
                </fieldset>
              )}
              <small>
                这是自托管费用控制，不代表已经接入支付或供应商账单系统。启用后只接受已配置价格版本的模型。
              </small>
            </section>
          )}
          <div className="subtle-note">
            仅可调用模型目录明确发布的协议。工具返回显示原始对象，由应用决定是否执行。
            内联图片受请求大小限制；供应商侧会话、文件引用和托管工具尚未开放。
          </div>
        </>
      )}
    </div>
  );
}
