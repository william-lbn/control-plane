import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';
import { usdToMicros } from './protocols';

type Account = {
  org_id: string;
  version: number;
  enabled: boolean;
  currency: string;
  credited_micros: number;
  spent_micros: number;
  reserved_micros: number;
  balance_micros: number;
  available_micros: number;
};
type CreditState = { account: Account; can_manage: boolean };
type Entry = {
  id: string;
  kind: string;
  request_id: string | null;
  credit_delta: number;
  spent_delta: number;
  reserved_delta: number;
  source_event_id: string | null;
  reason: string | null;
  created_at: string;
};
type PendingRequest = {
  request_id: string;
  branch_id: string;
  project_id: string;
  model_id: string;
  state: string;
  reserved_micros: number;
  needs_reconciliation: boolean;
};
type History<T> = { items: T[]; next_cursor: string };
type Mutation = { path: string; method: string; body: string; key: string };
const money = (value: number) => (value / 1000000).toFixed(6) + ' USD';

export function AICredits({ organizationId }: { organizationId: string }) {
  const [data, setData] = useState<CreditState | null>(null);
  const [ledger, setLedger] = useState<History<Entry>>({ items: [], next_cursor: '' });
  const [requests, setRequests] = useState<History<PendingRequest>>({ items: [], next_cursor: '' });
  const [enabled, setEnabled] = useState(false);
  const [kind, setKind] = useState('grant');
  const [amount, setAmount] = useState('');
  const [source, setSource] = useState('');
  const [reason, setReason] = useState('');
  const [evidence, setEvidence] = useState('');
  const [selected, setSelected] = useState('');
  const [usage, setUsage] = useState('');
  const [reconcileEvidence, setReconcileEvidence] = useState('');
  const [notAccepted, setNotAccepted] = useState(false);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const pending = useRef<Mutation | null>(null);
  const activeOrg = useRef(organizationId);
  activeOrg.current = organizationId;
  const readPath = '/api/v1/organizations/' + encodeURIComponent(organizationId) + '/ai-credits';
  const writePath =
    '/api/v1/platform/ai-credits/organizations/' + encodeURIComponent(organizationId);
  async function refresh() {
    const [state, entries, reserved] = await Promise.all([
      api<CreditState>(readPath),
      api<History<Entry>>(readPath + '/ledger'),
      api<History<PendingRequest>>(readPath + '/requests'),
    ]);
    if (activeOrg.current !== organizationId) return;
    setData(state);
    setEnabled(state.account.enabled);
    setLedger(entries);
    setRequests(reserved);
  }
  useEffect(() => {
    setData(null);
    pending.current = null;
    setError('');
    setNotice('');
    setUncertain(false);
    void refresh().catch((e) => setError(String(e)));
  }, [organizationId]);
  async function send(request: Mutation) {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      await api(request.path, {
        method: request.method,
        body: request.body,
        headers: { 'Idempotency-Key': request.key },
      });
      pending.current = null;
      setUncertain(false);
      await refresh();
      setNotice('额度操作已确认，账本已更新。');
      return true;
    } catch (e) {
      const definitive = e instanceof ApiError && e.status >= 400 && e.status < 500;
      if (definitive) {
        pending.current = null;
        setUncertain(false);
      } else {
        pending.current = request;
        setUncertain(true);
      }
      setError(e instanceof ApiError ? `${e.message} · ${e.code}` : String(e));
      return false;
    } finally {
      setBusy(false);
    }
  }
  async function mutate(path: string, method: string, value: unknown) {
    if (busy || uncertain) return false;
    const request = { path, method, body: JSON.stringify(value), key: newRequestKey() };
    pending.current = request;
    return send(request);
  }
  async function entry(event: React.FormEvent) {
    event.preventDefault();
    if (!data) return;
    try {
      const negative = amount.startsWith('-');
      const micros = usdToMicros(negative ? amount.slice(1) : amount) * (negative ? -1 : 1);
      if (micros === 0 || (kind === 'grant' && micros < 0))
        throw Error('授信必须为正数，更正金额不能为零。');
      if (
        await mutate(writePath + '/entries', 'POST', {
          version: data.account.version,
          kind,
          amount_micros: micros,
          source_event_id: source,
          reason,
          evidence_sha256: evidence,
        })
      ) {
        setAmount('');
        setSource('');
        setReason('');
        setEvidence('');
      }
    } catch (e) {
      setError(String(e));
    }
  }
  async function reconcile(event: React.FormEvent) {
    event.preventDefault();
    try {
      const value = {
        confirmed_not_accepted: notAccepted,
        evidence_sha256: reconcileEvidence,
        ...(notAccepted ? {} : { usage: JSON.parse(usage) }),
      };
      if (
        await mutate(
          '/api/v1/platform/ai-credits/requests/' + encodeURIComponent(selected) + '/reconcile',
          'POST',
          value,
        )
      ) {
        setSelected('');
        setUsage('');
        setReconcileEvidence('');
      }
    } catch (e) {
      setError(String(e));
    }
  }
  async function moreLedger() {
    setBusy(true);
    try {
      const next = await api<History<Entry>>(
        readPath + '/ledger?before=' + encodeURIComponent(ledger.next_cursor),
      );
      setLedger({ items: [...ledger.items, ...next.items], next_cursor: next.next_cursor });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function moreRequests() {
    setBusy(true);
    try {
      const next = await api<History<PendingRequest>>(
        readPath + '/requests?before=' + encodeURIComponent(requests.next_cursor),
      );
      setRequests({ items: [...requests.items, ...next.items], next_cursor: next.next_cursor });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel" aria-label="组织 AI 额度">
      <div className="panel-heading">
        <h2>组织 AI 额度</h2>
        <button disabled={busy} onClick={() => refresh().catch((e) => setError(String(e)))}>
          刷新 AI 额度
        </button>
      </div>
      <p>平台运营方分配的 AI 使用额度，按已发布模型价格结算。结果未确认的调用继续保留预留额度。</p>
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {uncertain && (
        <div>
          <p>提交结果尚未确认，保留了原请求。可用相同提交恢复结果。</p>
          <button disabled={busy} onClick={() => pending.current && send(pending.current)}>
            重试原额度提交
          </button>
        </div>
      )}
      {data ? (
        <>
          <div className="stats-grid">
            <div className="stat-card">
              <small>可用额度</small>
              <strong data-testid="ai-credit-available">
                {money(data.account.available_micros)}
              </strong>
            </div>
            <div className="stat-card">
              <small>已确认费用</small>
              <strong data-testid="ai-credit-spent">{money(data.account.spent_micros)}</strong>
            </div>
            <div className="stat-card">
              <small>预留额度</small>
              <strong data-testid="ai-credit-reserved">
                {money(data.account.reserved_micros)}
              </strong>
            </div>
            <div className="stat-card">
              <small>累计授信</small>
              <strong>{money(data.account.credited_micros)}</strong>
            </div>
          </div>
          <p>额度控制：{data.account.enabled ? '已启用' : '未启用'}</p>
          {data.can_manage && (
            <fieldset disabled={busy || uncertain}>
              <legend>平台运营额度管理</legend>
              <label>
                <input
                  aria-label="启用组织 AI 额度"
                  type="checkbox"
                  checked={enabled}
                  onChange={(e) => setEnabled(e.target.checked)}
                />
                启用组织额度控制
              </label>
              <button
                onClick={() => mutate(writePath, 'PUT', { version: data.account.version, enabled })}
              >
                保存组织额度策略
              </button>
              <form className="identity-form" onSubmit={entry}>
                <label>
                  额度操作
                  <select
                    aria-label="AI 额度操作"
                    value={kind}
                    onChange={(e) => setKind(e.target.value)}
                  >
                    <option value="grant">分配额度</option>
                    <option value="adjustment">凭证更正</option>
                  </select>
                </label>
                <label>
                  金额（USD）
                  <input
                    aria-label="AI 授信金额"
                    value={amount}
                    required
                    inputMode="decimal"
                    maxLength={24}
                    onChange={(e) => setAmount(e.target.value)}
                  />
                </label>
                <label>
                  来源凭证编号
                  <input
                    aria-label="AI 授信来源"
                    value={source}
                    required
                    minLength={8}
                    maxLength={128}
                    onChange={(e) => setSource(e.target.value)}
                  />
                </label>
                <label>
                  操作原因
                  <input
                    aria-label="AI 授信原因"
                    value={reason}
                    required
                    minLength={8}
                    maxLength={500}
                    onChange={(e) => setReason(e.target.value)}
                  />
                </label>
                <label>
                  凭证 SHA-256
                  <input
                    aria-label="AI 授信凭证"
                    value={evidence}
                    required
                    pattern="[a-f0-9]{64}"
                    maxLength={64}
                    onChange={(e) => setEvidence(e.target.value)}
                  />
                </label>
                <button className="button primary">提交 AI 额度记录</button>
              </form>
            </fieldset>
          )}
          <h3>额度账本</h3>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>时间</th>
                  <th>操作</th>
                  <th>授信变动</th>
                  <th>费用变动</th>
                  <th>预留变动</th>
                  <th>来源 / 请求</th>
                  <th>原因</th>
                </tr>
              </thead>
              <tbody>
                {ledger.items.map((item) => (
                  <tr key={item.id}>
                    <td>{item.created_at}</td>
                    <td>{item.kind}</td>
                    <td>{money(item.credit_delta)}</td>
                    <td>{money(item.spent_delta)}</td>
                    <td>{money(item.reserved_delta)}</td>
                    <td>
                      <code>{item.source_event_id || item.request_id || '—'}</code>
                    </td>
                    <td>{item.reason || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {ledger.next_cursor && (
            <button disabled={busy} onClick={() => void moreLedger()}>
              加载更早额度记录
            </button>
          )}
          <h3>未确认调用</h3>
          {requests.items.length ? (
            <ul>
              {requests.items.map((item) => (
                <li key={item.request_id}>
                  <code>{item.request_id}</code> · {item.model_id} · {item.state} ·{' '}
                  {money(item.reserved_micros)}
                </li>
              ))}
            </ul>
          ) : (
            <p>没有未确认的额度预留。</p>
          )}
          {requests.next_cursor && (
            <button disabled={busy} onClick={() => void moreRequests()}>
              加载更多预留
            </button>
          )}
          {data.can_manage && (
            <form className="identity-form" onSubmit={reconcile}>
              <fieldset disabled={busy || uncertain}>
                <legend>运营凭证对账</legend>
                <label>
                  待核对请求
                  <select
                    aria-label="AI 额度对账请求"
                    value={selected}
                    required
                    onChange={(e) => setSelected(e.target.value)}
                  >
                    <option value="">请选择</option>
                    {requests.items
                      .filter((item) => item.needs_reconciliation)
                      .map((item) => (
                        <option key={item.request_id} value={item.request_id}>
                          {item.request_id}
                        </option>
                      ))}
                  </select>
                </label>
                <label>
                  <input
                    aria-label="AI 额度供应商未接受"
                    type="checkbox"
                    checked={notAccepted}
                    onChange={(e) => setNotAccepted(e.target.checked)}
                  />
                  凭证已确认供应商未接受该调用
                </label>
                <label>
                  已确认的 Token 用量
                  <textarea
                    aria-label="AI 额度对账用量"
                    value={usage}
                    required={!notAccepted}
                    disabled={notAccepted}
                    maxLength={2000}
                    rows={3}
                    onChange={(e) => setUsage(e.target.value)}
                  />
                </label>
                <label>
                  对账凭证 SHA-256
                  <input
                    aria-label="AI 额度对账凭证"
                    value={reconcileEvidence}
                    required
                    pattern="[a-f0-9]{64}"
                    maxLength={64}
                    onChange={(e) => setReconcileEvidence(e.target.value)}
                  />
                </label>
                <button disabled={!selected}>确认 AI 额度对账</button>
              </fieldset>
            </form>
          )}
        </>
      ) : (
        <p>正在读取组织额度…</p>
      )}
    </section>
  );
}
