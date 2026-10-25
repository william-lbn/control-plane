import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Offer = {
  id: string;
  name: string;
  revision: string;
  amount_minor: number;
  credit_micros: number;
};
type Catalog = {
  enabled: boolean;
  provider_mode: string;
  credit_account_ready: boolean;
  can_manage: boolean;
  offers: Offer[];
};
type Invoice = {
  invoice_id: string;
  offer_name: string;
  version: number;
  currency: string;
  amount_minor: number;
  credit_micros: number;
  checkout_state: string;
  paid: boolean;
  refunded_minor: number;
  dispute_state: string | null;
  applied_credit_micros: number;
  can_retry_checkout: boolean;
  checkout_dispatched: boolean;
  created_at: string;
};
type History = { items: Invoice[]; next_cursor: string };
type Receipt = {
  inbox_id: string;
  event_type: string;
  state: string;
  error_code: string | null;
  source_kind: string;
  created_at: string;
};
type Pending = { path: string; body: string; key: string };
const usd = (cents: number) => (cents / 100).toFixed(2) + ' USD';
const credit = (micros: number) => (micros / 1000000).toFixed(6) + ' USD';

export function Billing({ organizationId }: { organizationId: string }) {
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [history, setHistory] = useState<History>({ items: [], next_cursor: '' });
  const [offerId, setOfferId] = useState('');
  const [invoiceId, setInvoiceId] = useState('');
  const [kind, setKind] = useState('session');
  const [objectId, setObjectId] = useState('');
  const [reason, setReason] = useState('');
  const [evidence, setEvidence] = useState('');
  const [receipts, setReceipts] = useState<Receipt[]>([]);
  const [link, setLink] = useState<{ invoice: string; url: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const pending = useRef<Pending | null>(null);
  const root = '/api/v1/organizations/' + encodeURIComponent(organizationId) + '/billing';
  const operator =
    '/api/v1/platform/billing/organizations/' + encodeURIComponent(organizationId) + '/invoices';
  async function refresh() {
    const [c, h] = await Promise.all([api<Catalog>(root), api<History>(root + '/invoices')]);
    setCatalog(c);
    setHistory(h);
    setLink((previous) =>
      previous &&
      h.items.some(
        (v) => v.invoice_id === previous.invoice && !v.paid && v.checkout_state === 'ready',
      )
        ? previous
        : null,
    );
  }
  useEffect(() => {
    void refresh().catch((e) => setError(String(e)));
  }, [organizationId]);
  const awaiting = history.items.some(
    (v) => !v.paid && (v.checkout_state === 'dispatching' || v.checkout_state === 'ready'),
  );
  useEffect(() => {
    if (!awaiting || !catalog?.enabled) return;
    const timer = window.setInterval(() => void refresh().catch((e) => setError(String(e))), 10000);
    return () => window.clearInterval(timer);
  }, [awaiting, catalog?.enabled, organizationId]);
  async function send(request: Pending) {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const result = await api<{ invoice_id: string }>(request.path, {
        method: 'POST',
        body: request.body,
        headers: { 'Idempotency-Key': request.key },
      });
      pending.current = null;
      setUncertain(false);
      setInvoiceId(result.invoice_id);
      setReceipts([]);
      setNotice('原请求已确认。到账和退款状态以支付方核验回执为准。');
    } catch (e) {
      const definitive = e instanceof ApiError && e.status >= 400 && e.status < 500;
      pending.current = definitive ? null : request;
      setUncertain(!definitive);
      setError(e instanceof ApiError ? `${e.message} · ${e.code}` : String(e));
    } finally {
      setBusy(false);
    }
    try {
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  }
  async function mutate(path: string, value: unknown) {
    if (busy || pending.current) return;
    const request = { path, body: JSON.stringify(value), key: newRequestKey() };
    pending.current = request;
    await send(request);
  }
  async function openCheckout(invoice: Invoice) {
    setBusy(true);
    setError('');
    setLink(null);
    try {
      const value = await api<{ invoice_id: string; checkout_url: string }>(
        root + '/invoices/' + invoice.invoice_id + '/checkout',
      );
      const url = new URL(value.checkout_url);
      if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password)
        throw Error('收银台链接格式无效');
      setLink({ invoice: value.invoice_id, url: value.checkout_url });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function readReceipts(id: string) {
    setBusy(true);
    setError('');
    try {
      const value = await api<{ items: Receipt[] }>(root + '/invoices/' + id + '/receipts');
      setInvoiceId(id);
      setReceipts(value.items);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function more() {
    setBusy(true);
    try {
      const next = await api<History>(
        root + '/invoices?before=' + encodeURIComponent(history.next_cursor),
      );
      setHistory({ items: [...history.items, ...next.items], next_cursor: next.next_cursor });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const selected = history.items.find((v) => v.invoice_id === invoiceId);
  const offer = catalog?.offers.find((v) => v.id === offerId);
  return (
    <section className="panel" aria-label="组织付款与账单">
      <div className="panel-heading">
        <h2>组织付款与账单</h2>
        <button disabled={busy} onClick={() => refresh().catch((e) => setError(String(e)))}>
          刷新付款账单
        </button>
      </div>
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {catalog && (
        <p>
          支付模式：
          {{
            disabled: '未启用',
            synthetic: '本地合成验收',
            test: '支付方测试模式',
            live: '正式收款',
          }[catalog.provider_mode] || catalog.provider_mode}
          。{!catalog.credit_account_ready && '组织额度账户尚未启用。'}
          创建订单、打开收银台或返回控制台均不代表到账。
        </p>
      )}
      {uncertain && (
        <div role="alert">
          <p>原请求结果暂未确认，请保留当前页面并查询原订单。恢复会复用同一请求键。</p>
          <button disabled={busy} onClick={() => pending.current && void send(pending.current)}>
            恢复原付款请求
          </button>
        </div>
      )}
      {catalog?.enabled && (
        <form
          className="identity-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (offer)
              void mutate(root + '/invoices', {
                offer_id: offer.id,
                offer_revision: offer.revision,
              });
          }}
        >
          <label>
            固定额度方案
            <select
              aria-label="固定额度方案"
              required
              value={offerId}
              onChange={(e) => setOfferId(e.target.value)}
            >
              <option value="">选择当前方案</option>
              {catalog.offers.map((o) => (
                <option key={o.id} value={o.id}>
                  {o.name} · 付款 {usd(o.amount_minor)} · 额度 {credit(o.credit_micros)}
                </option>
              ))}
            </select>
          </label>
          <button disabled={busy || uncertain || !offer || !catalog.credit_account_ready}>
            创建原付款订单
          </button>
        </form>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>原订单</th>
              <th>付款 / 额度</th>
              <th>状态</th>
              <th>退款 / 已入额度</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {history.items.map((v) => (
              <tr key={v.invoice_id}>
                <td>
                  {v.invoice_id}
                  <br />
                  {v.offer_name}
                </td>
                <td>
                  {usd(v.amount_minor)} / {credit(v.credit_micros)}
                </td>
                <td>
                  {v.paid ? '已核验付款' : v.checkout_state}
                  {v.dispute_state && ' · 争议 ' + v.dispute_state}
                </td>
                <td>
                  {usd(v.refunded_minor)} / {credit(v.applied_credit_micros)}
                </td>
                <td>
                  {catalog?.enabled &&
                    !v.paid &&
                    (v.checkout_state === 'open' || v.can_retry_checkout) && (
                      <button
                        disabled={busy || uncertain}
                        onClick={() =>
                          mutate(root + '/invoices/' + v.invoice_id + '/checkout', {
                            version: v.version,
                          })
                        }
                      >
                        请求原订单收银台
                      </button>
                    )}
                  {catalog?.enabled && !v.paid && v.checkout_state === 'ready' && (
                    <button disabled={busy} onClick={() => openCheckout(v)}>
                      读取原收银台链接
                    </button>
                  )}
                  <button disabled={busy} onClick={() => readReceipts(v.invoice_id)}>
                    查看付款回执
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {link && (
        <p>
          <a href={link.url} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">
            前往原订单收银台
          </a>
        </p>
      )}
      {history.next_cursor && (
        <button disabled={busy} onClick={more}>
          更早付款订单
        </button>
      )}
      {invoiceId && (
        <div aria-label="原付款回执">
          <h3>原付款回执 · {invoiceId}</h3>
          {receipts.map((r) => (
            <p key={r.inbox_id}>
              {r.event_type} · {r.state}
              {r.error_code && ' · ' + r.error_code} · {r.source_kind}
            </p>
          ))}
        </div>
      )}
      {catalog?.can_manage && (
        <form
          className="identity-form"
          aria-label="核对原付款记录"
          onSubmit={(e) => {
            e.preventDefault();
            if (selected)
              void mutate(operator + '/' + selected.invoice_id + '/reconcile', {
                version: selected.version,
                object_type: kind,
                object_id: objectId,
                reason,
                evidence_sha256: evidence,
              });
          }}
        >
          <h3>核对原付款记录</h3>
          <p>从支付方读取原始记录。此操作可恢复原收银台或同步已付款、退款和争议状态。</p>
          <label>
            待核对原订单
            <select
              aria-label="待核对原订单"
              required
              value={invoiceId}
              onChange={(e) => {
                setInvoiceId(e.target.value);
                setReceipts([]);
              }}
            >
              <option value="">选择原订单</option>
              {history.items
                .filter((v) => v.checkout_dispatched)
                .map((v) => (
                  <option key={v.invoice_id} value={v.invoice_id}>
                    {v.invoice_id}
                  </option>
                ))}
            </select>
          </label>
          <label>
            支付方记录类型
            <select
              aria-label="支付方记录类型"
              value={kind}
              onChange={(e) => setKind(e.target.value)}
            >
              <option value="session">收银台 Session</option>
              <option value="charge">Charge</option>
              <option value="dispute">Dispute</option>
            </select>
          </label>
          <label>
            支付方原记录 ID
            <input
              aria-label="支付方原记录 ID"
              value={objectId}
              onChange={(e) => setObjectId(e.target.value)}
              required
              maxLength={255}
            />
          </label>
          <label>
            付款核对原因
            <input
              aria-label="付款核对原因"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              required
              minLength={8}
              maxLength={500}
            />
          </label>
          <label>
            付款证据 SHA-256
            <input
              aria-label="付款证据 SHA-256"
              value={evidence}
              onChange={(e) => setEvidence(e.target.value)}
              required
              pattern="[a-f0-9]{64}"
            />
          </label>
          <button disabled={busy || uncertain || !selected || !selected.checkout_dispatched}>
            核对原支付方记录
          </button>
        </form>
      )}
    </section>
  );
}
