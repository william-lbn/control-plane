import { useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Difference = {
  kind: string;
  expected_name: string;
  expected_oid: number;
  observed_name?: string;
  observed_oid?: number;
  observed_owner?: string;
  status: string;
  changes: string[];
};
type State = {
  recorded: boolean;
  baseline_id?: string;
  captured_at?: string;
  observation_sha256?: string;
  object_count?: number;
  observed_objects?: { kind: string; name: string; oid: number; owner?: string }[];
  items: Difference[];
  source_endpoint_changed?: boolean;
  source_backings_changed?: boolean;
  platform_intent_changed?: boolean;
};
type Pending = { body: string; key: string };
const statusNames: Record<string, string> = {
  catalog_matches: '原身份一致',
  renamed: '原对象已改名',
  replaced: '原名称被新对象替换',
  missing: '原对象缺失',
  name_collision: '原名称冲突',
  privilege_conflict: '超级用户权限变化',
  role_options_changed: '角色选项变化',
  database_options_changed: '数据库选项变化',
  owner_changed: '数据库属主变化',
  owner_role_renamed: '属主角色改名',
  provenance_changed: '管理标记变化',
  scope_conflict: '原分支时间线不符',
};

export function CatalogPlatformIdentities({
  path,
  canAdmin,
  disabled,
  onBusy,
}: {
  path: string;
  canAdmin: boolean;
  disabled: boolean;
  onBusy: (value: boolean) => void;
}) {
  const [state, setState] = useState<State | null>(null);
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const pending = useRef<Pending | null>(null);
  const url = path + '/catalog-platform-identities';
  async function read() {
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      setState(await api<State>(url));
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
      onBusy(false);
    }
  }
  async function send(request: Pending) {
    setBusy(true);
    onBusy(true);
    setError('');
    setNotice('');
    let confirmed = false;
    try {
      await api(url, {
        method: 'POST',
        body: request.body,
        headers: { 'Idempotency-Key': request.key },
      });
      pending.current = null;
      setUncertain(false);
      setAck(false);
      setNotice('当前平台原身份已记录。后续观察保留同一组原生 OID。');
      confirmed = true;
    } catch (e) {
      const definitive = e instanceof ApiError && e.status >= 400 && e.status < 500;
      pending.current = definitive ? null : request;
      setUncertain(!definitive);
      setError(e instanceof ApiError ? `${e.message} · ${e.code}` : String(e));
    } finally {
      setBusy(false);
      onBusy(false);
    }
    if (confirmed) await read();
  }
  async function capture() {
    if (!canAdmin || !ack || !state?.observation_sha256 || busy || pending.current) return;
    const request = {
      body: JSON.stringify({
        acknowledge_initial_observation: true,
        observation_sha256: state.observation_sha256,
      }),
      key: newRequestKey(),
    };
    pending.current = request;
    await send(request);
  }
  return (
    <section className="panel" aria-label="平台与默认目录身份">
      <div className="panel-heading">
        <h2>平台与默认目录身份</h2>
        <button disabled={disabled || busy} onClick={read}>
          核对平台与默认身份
        </button>
      </div>
      <p>
        通过固定维护库读取原生 OID、名称、属主和选项。密码不在观察范围内；记录动作不改动 SQL
        对象或凭据。
      </p>
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {uncertain && (
        <div role="alert">
          <p>原记录请求结果暂未确认，请保留此页面并恢复同一请求。</p>
          <button
            disabled={busy || disabled}
            onClick={() => pending.current && void send(pending.current)}
          >
            恢复原平台身份记录
          </button>
        </div>
      )}
      {state && !state.recorded && (
        <>
          <p>尚无这一原生时间线的平台身份记录。首次记录仅证明当前观察时点。</p>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>类型</th>
                  <th>当前名称</th>
                  <th>当前 OID</th>
                  <th>当前属主</th>
                </tr>
              </thead>
              <tbody>
                {state.observed_objects?.map((v) => (
                  <tr key={v.kind + ':' + v.oid}>
                    <td>{v.kind}</td>
                    <td>{v.name}</td>
                    <td>{v.oid}</td>
                    <td>{v.owner || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {canAdmin && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                void capture();
              }}
            >
              <label>
                <input
                  type="checkbox"
                  aria-label="确认从当前观察记录平台身份"
                  checked={ack}
                  onChange={(e) => setAck(e.target.checked)}
                  required
                />
                我确认从当前观察开始记录，不将其视为更早历史的证明。
              </label>
              <button disabled={disabled || busy || uncertain || !ack}>记录当前平台原身份</button>
            </form>
          )}
        </>
      )}
      {state?.recorded && (
        <>
          <p>
            原记录：{state.baseline_id} · {state.captured_at}
          </p>
          {(state.source_endpoint_changed ||
            state.source_backings_changed ||
            state.platform_intent_changed) && (
            <p role="status">当前 Endpoint 或配置来源已变化，继续按原 OID 比较；凭据内容未观察。</p>
          )}
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>原对象</th>
                  <th>原 OID</th>
                  <th>当前对象 / OID</th>
                  <th>差异</th>
                </tr>
              </thead>
              <tbody>
                {state.items.map((v) => (
                  <tr key={v.kind + ':' + v.expected_oid}>
                    <td>
                      {v.kind} · {v.expected_name}
                    </td>
                    <td>{v.expected_oid}</td>
                    <td>
                      {v.observed_name || '缺失'} / {v.observed_oid ?? '—'}
                    </td>
                    <td>
                      {statusNames[v.status] || v.status}
                      {v.changes.length > 0 && ' · ' + v.changes.join(', ')}
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
