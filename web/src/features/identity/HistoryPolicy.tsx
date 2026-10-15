import { useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Policy = { capture_enabled: boolean; retain_days: number; version_token: string };
type Attempt = {
  key: string;
  version: string;
  body: {
    capture_enabled: boolean;
    retain_days: number;
    acknowledge_expired_history_deletion: boolean;
  };
};
export function HistoryPolicy() {
  const [policy, setPolicy] = useState<Policy | null>(null),
    [capture, setCapture] = useState(true),
    [days, setDays] = useState(0),
    [ack, setAck] = useState(false),
    [busy, setBusy] = useState(false),
    [uncertain, setUncertain] = useState(false),
    [notice, setNotice] = useState<{ message: string; error: boolean } | null>(null);
  const attempt = useRef<Attempt | null>(null);
  async function load() {
    setBusy(true);
    setNotice(null);
    try {
      const p = await api<Policy>('/api/v1/account/query-history-policy');
      setPolicy(p);
      setCapture(p.capture_enabled);
      setDays(p.retain_days);
      setAck(false);
      attempt.current = null;
      setUncertain(false);
    } catch (e) {
      setNotice({ message: e instanceof Error ? e.message : '策略读取失败', error: true });
    } finally {
      setBusy(false);
    }
  }
  async function save() {
    if (!policy) return;
    setBusy(true);
    setNotice(null);
    try {
      if (!attempt.current)
        attempt.current = {
          key: 'history-policy-' + newRequestKey(),
          version: policy.version_token,
          body: {
            capture_enabled: capture,
            retain_days: days,
            acknowledge_expired_history_deletion: ack,
          },
        };
      const original = attempt.current;
      await api<Policy>('/api/v1/account/query-history-policy', {
        method: 'PATCH',
        headers: { 'Idempotency-Key': original.key, 'If-Match': '"' + original.version + '"' },
        body: JSON.stringify(original.body),
      });
      const p = await api<Policy>('/api/v1/account/query-history-policy');
      const current =
        p.capture_enabled === original.body.capture_enabled &&
        p.retain_days === original.body.retain_days;
      setPolicy(p);
      setCapture(p.capture_enabled);
      setDays(p.retain_days);
      setAck(false);
      attempt.current = null;
      setUncertain(false);
      setNotice({
        message: current
          ? '已保存本账号的历史策略；不会修改其他账号。'
          : '原提交已确认，但策略已被其他会话更新；已显示当前状态。',
        error: false,
      });
    } catch (e) {
      const unknown = !!attempt.current && (!(e instanceof ApiError) || e.status >= 500);
      setUncertain(unknown);
      if (!unknown) attempt.current = null;
      setNotice({
        message:
          (e instanceof Error ? e.message : '策略结果未确认') +
          (unknown ? '；可重试原提交，或加载当前策略核对。' : ''),
        error: true,
      });
    } finally {
      setBusy(false);
    }
  }
  async function cleanup() {
    setBusy(true);
    setNotice(null);
    try {
      const p = await api<{ deleted_count: number; more_possible: boolean }>(
        '/api/v1/account/query-history-cleanup',
        { method: 'POST', body: JSON.stringify({ acknowledge_expired_history_deletion: ack }) },
      );
      setNotice({
        message: `已清理本账号 ${p.deleted_count} 条到期历史。${p.more_possible ? '本批达到 1000 条；可确认后继续。' : '进行中和结果未知记录保持。'}`,
        error: false,
      });
    } catch (e) {
      setNotice({
        message: e instanceof Error ? e.message : '清理结果未确认，请查询历史。',
        error: true,
      });
    } finally {
      setAck(false);
      setBusy(false);
    }
  }
  return (
    <section className="panel identity-panel" aria-label="我的 SQL 历史策略">
      <h2>我的 SQL 历史策略</h2>
      <p>
        默认保留全部历史。关闭采集只影响后续请求，不会删除已有记录。到期清理会物理删除本账号已完成的历史文本，不影响业务数据库、保存查询或其他账号。
      </p>
      <button className="button" disabled={busy} onClick={() => void load()}>
        加载我的历史策略
      </button>
      {policy && (
        <>
          <p data-testid="history-policy-current">
            当前：{policy.capture_enabled ? '采集开启' : '采集关闭'} ·{' '}
            {policy.retain_days === 0 ? '保留全部' : `保留 ${policy.retain_days} 天`} · 版本{' '}
            {policy.version_token}
          </p>
          <div className="selector-row">
            <label>
              <input
                type="checkbox"
                checked={capture}
                disabled={busy || uncertain}
                onChange={(e) => setCapture(e.target.checked)}
              />
              采集我后续的 SQL 执行历史
            </label>
            <label>
              我的历史保留期限
              <select
                disabled={busy || uncertain}
                value={days}
                onChange={(e) => {
                  setDays(Number(e.target.value));
                  setAck(false);
                }}
              >
                <option value={0}>保留全部</option>
                <option value={7}>7 天</option>
                <option value={30}>30 天</option>
                <option value={90}>90 天</option>
              </select>
            </label>
          </div>
          <label>
            <input
              type="checkbox"
              checked={ack}
              disabled={busy || uncertain}
              onChange={(e) => setAck(e.target.checked)}
            />
            我确认允许物理清理本人已完成的到期历史
          </label>
          <div className="selector-row">
            <button
              className="button primary"
              disabled={busy || (!uncertain && days > 0 && !ack)}
              onClick={() => void save()}
            >
              {uncertain ? '重试原历史策略提交' : '保存我的历史策略'}
            </button>
            <button
              className="button"
              disabled={busy || uncertain || !ack || policy.retain_days === 0}
              onClick={() => void cleanup()}
            >
              执行我的到期历史清理
            </button>
          </div>
          <p className="muted">
            自动清理每小时最多一批 1000
            条。进行中、结果未知或没有完成回执的记录不会自动删除。保留期限适用于本账号所有项目已有及后续的已完成历史。
          </p>
        </>
      )}
      {notice && <p role={notice.error ? 'alert' : 'status'}>{notice.message}</p>}
    </section>
  );
}
