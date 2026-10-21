import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
import { newRequestKey } from '../../shared/requestKey';
import { CatalogIdentityRestore } from './CatalogIdentityRestore';

type NativeObject = {
  kind: 'role' | 'database';
  name: string;
  oid: number;
  owner?: string;
  managed: boolean;
  protected: boolean;
  can_initialize: boolean;
  management_excluded?: boolean;
  identity_status?: string;
  intent_version?: string;
};
type Difference = {
  binding_id: string;
  kind: 'role' | 'database';
  expected_name: string;
  expected_oid: number;
  observed_name?: string;
  observed_oid?: number;
  observed_owner?: string;
  status: string;
  catalog_status: string;
  changes: string[];
  credentials: 'not_observed';
  intent_version: string;
  captured_at: string;
};
type Inventory = {
  branch_id: string;
  endpoint_id: string;
  items: Difference[];
  current_bindings: (Difference & {
    identity_id: string;
    current_intent_version: string;
    source_type: string;
    options: 'not_recorded';
    management_excluded?: boolean;
  })[];
  native_objects: NativeObject[];
  credentials: 'not_observed';
  complete: true;
  limit: number;
};
type Pending = { key: string; version: string; body: string };
const statusNames: Record<string, string> = {
  catalog_matches: '目录身份一致',
  renamed: '原对象已改名',
  replaced: '同名对象已被替换',
  missing: '原对象不存在',
  name_collision: '原名称被其他对象占用',
  privilege_conflict: '超级用户权限冲突',
  provenance_changed: '管理标记变化',
  owner_changed: '数据库 Owner 变化',
  owner_role_renamed: '原 Owner 角色已改名',
  role_options_changed: '角色选项变化',
  database_options_changed: '数据库选项变化',
  scope_conflict: '分支身份冲突',
  binding_invalid: '基准记录无效',
  observation_invalid: '观察不完整',
  intent_changed: '平台管理意图已有变化',
  management_excluded: '已排除平台管理',
};
const sourceNames: Record<string, string> = {
  initial_observation: '首次身份观察',
  create_result: '原创建回执',
  mutation_result: '目录变更回执',
  rename_result: '重命名回执',
  rename_follower: 'Owner 名称同步',
  inherited_identity: '分支原身份继承',
};

export function CatalogIdentities({
  path,
  canAdmin,
  disabled,
  onBusy,
}: {
  path: string;
  canAdmin: boolean;
  disabled: boolean;
  onBusy: (busy: boolean) => void;
}) {
  const [inventory, setInventory] = useState<Inventory | null>(null);
  const [selected, setSelected] = useState('');
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [pending, setPending] = useState<Pending | null>(null);
  const active = useRef(true);
  useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
    };
  }, []);
  const url = path + '/catalog-identities';
  const candidates = inventory?.native_objects.filter((v) => v.can_initialize) || [];
  const candidate = candidates.find((v) => v.kind + ':' + v.oid === selected);
  async function read(reset = false) {
    setBusy(true);
    onBusy(true);
    setError('');
    try {
      const value = await api<Inventory>(url);
      if (!active.current) return;
      setInventory(value);
      if (reset) {
        setPending(null);
        setSelected('');
        setAck(false);
      }
    } catch (e) {
      if (active.current) setError(e instanceof Error ? e.message : '身份观察失败');
    } finally {
      if (active.current) {
        setBusy(false);
        onBusy(false);
      }
    }
  }
  async function initialize() {
    if (!canAdmin || (!pending && (!candidate?.intent_version || !ack))) return;
    const request = pending || {
      key: newRequestKey(),
      version: candidate!.intent_version!,
      body: JSON.stringify({
        kind: candidate!.kind,
        name: candidate!.name,
        oid: candidate!.oid,
        acknowledge_initial_observation: true,
      }),
    };
    setPending(request);
    setBusy(true);
    onBusy(true);
    setError('');
    setMessage('');
    try {
      const receipt = await api<{ binding_id: string }>(url, {
        method: 'POST',
        headers: { 'Idempotency-Key': request.key, 'If-Match': `"${request.version}"` },
        body: request.body,
      });
      if (!active.current) return;
      setPending(null);
      setSelected('');
      setAck(false);
      setMessage(`原身份已记录 · ${receipt.binding_id}`);
      const value = await api<Inventory>(url);
      if (active.current) setInventory(value);
    } catch (e) {
      if (active.current) setError(e instanceof Error ? e.message : '记录身份失败');
    } finally {
      if (active.current) {
        setBusy(false);
        onBusy(false);
      }
    }
  }
  return (
    <section className="panel" aria-label="外部 DDL 身份观察">
      <div className="panel-heading">
        <h2>外部 DDL 身份观察</h2>
        <button className="button" disabled={disabled || busy} onClick={() => void read()}>
          读取身份变化
        </button>
      </div>
      <p>按原 OID 区分改名和同名重建。读取会使用读写 Compute；只查询系统目录，不读取用户表。</p>
      <p>目录一致不表示密码一致：客户角色密码及 SCRAM verifier 均未观察。</p>
      {error && <p role="alert">{error}</p>}
      {message && <p role="status">{message}</p>}
      {inventory && (
        <>
          <h3>当前受管理身份</h3>
          <p>
            按当前管理代际核对原 OID、名称、Owner 和分支标记；这些记录不包含历史选项或密码快照。
          </p>
          <div className="table-wrap">
            <table aria-label="当前目录身份">
              <thead>
                <tr>
                  <th>对象 / 原 OID</th>
                  <th>当前对象 / OID</th>
                  <th>身份代际 / 管理代际</th>
                  <th>来源</th>
                  <th>观察结果</th>
                </tr>
              </thead>
              <tbody>
                {(inventory.current_bindings || []).map((v) => (
                  <tr key={v.binding_id}>
                    <td>
                      {v.kind === 'role' ? '角色' : '数据库'} · {v.expected_name} · {v.expected_oid}
                    </td>
                    <td>
                      {v.observed_name || '—'} / {v.observed_oid || '—'}
                      {v.observed_owner && <small>Owner {v.observed_owner}</small>}
                    </td>
                    <td>
                      {v.intent_version} / {v.current_intent_version}
                    </td>
                    <td>{sourceNames[v.source_type] || v.source_type}</td>
                    <td>
                      {statusNames[v.status] || v.status}
                      {v.status !== v.catalog_status && (
                        <small>目录：{statusNames[v.catalog_status] || v.catalog_status}</small>
                      )}
                      <small>历史选项：未记录 · 密码：未观察</small>
                    </td>
                  </tr>
                ))}
                {!inventory.current_bindings?.length && (
                  <tr>
                    <td colSpan={5}>尚无持续身份记录；首次观察历史与未证明的旧意图单独保留。</td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
          <h3>首次观察历史</h3>
          <div className="table-wrap">
            <table aria-label="身份变化记录">
              <thead>
                <tr>
                  <th>原对象</th>
                  <th>原 OID</th>
                  <th>当前对象 / OID</th>
                  <th>观察结果</th>
                </tr>
              </thead>
              <tbody>
                {inventory.items.map((v) => (
                  <tr key={v.binding_id}>
                    <td>
                      {v.kind === 'role' ? '角色' : '数据库'} · {v.expected_name}
                    </td>
                    <td>{v.expected_oid}</td>
                    <td>
                      {v.observed_name || '—'} / {v.observed_oid || '—'}
                    </td>
                    <td>
                      {statusNames[v.status] || v.status}
                      {v.status === 'intent_changed' && (
                        <small>目录：{statusNames[v.catalog_status] || v.catalog_status}</small>
                      )}
                      <small>密码：未观察</small>
                    </td>
                  </tr>
                ))}
                {!inventory.items.length && (
                  <tr>
                    <td colSpan={4}>尚未记录原身份；不能根据当前名称推断历史。</td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
          <p>平台操作改变管理意图后会单独显示，原基准不会自动覆盖。读取身份变化只执行观察。</p>
          {canAdmin && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                void initialize();
              }}
            >
              <label>
                首次记录原身份
                <select
                  aria-label="首次记录原身份"
                  value={selected}
                  disabled={disabled || busy || !!pending}
                  onChange={(e) => {
                    setSelected(e.target.value);
                    setAck(false);
                  }}
                >
                  <option value="">选择尚未记录的受管理对象</option>
                  {candidates.map((v) => (
                    <option key={v.kind + ':' + v.oid} value={v.kind + ':' + v.oid}>
                      {v.kind === 'role' ? '角色' : '数据库'} · {v.name} · OID {v.oid}
                    </option>
                  ))}
                </select>
              </label>
              <label className="checkbox-label">
                <input
                  type="checkbox"
                  checked={ack}
                  disabled={disabled || busy || !!pending}
                  onChange={(e) => setAck(e.target.checked)}
                />
                我确认仅从当前时点记录原身份；此前是否发生过重建无法由名称或标记证明。
              </label>
              <button
                className="button"
                disabled={disabled || busy || (!pending && (!candidate || !ack))}
              >
                {pending ? '按原请求重试记录' : '确认并记录原身份'}
              </button>
              {pending && (
                <button
                  type="button"
                  className="button"
                  disabled={disabled || busy}
                  onClick={() => void read(true)}
                >
                  读取状态并重新选择
                </button>
              )}
            </form>
          )}
          {canAdmin && (
            <CatalogIdentityRestore
              path={path}
              items={inventory.items}
              disabled={disabled || busy}
              onBusy={onBusy}
              onComplete={async () => {
                const value = await api<Inventory>(url);
                if (active.current) setInventory(value);
              }}
            />
          )}
          <details>
            <summary>完整系统目录（含 NOLOGIN 角色及禁止连接的数据库）</summary>
            <ul>
              {inventory.native_objects.map((v) => (
                <li key={v.kind + ':' + v.oid}>
                  {v.kind === 'role' ? '角色' : '数据库'} · {v.name} · OID {v.oid}
                  {v.owner ? ' · Owner ' + v.owner : ''}
                  {v.protected ? ' · 平台保护' : ''}
                  {v.management_excluded ? ' · 原身份不匹配，平台管理已隔离' : ''}
                  {v.identity_status
                    ? ' · ' + (statusNames[v.identity_status] || v.identity_status)
                    : ''}
                </li>
              ))}
            </ul>
          </details>
        </>
      )}
    </section>
  );
}
