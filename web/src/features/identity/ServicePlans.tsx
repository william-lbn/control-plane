import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Revision = { id: string; revision: number; max_projects: number; max_endpoints: number };
type Plan = { id: string; name: string; state: string; version: number; latest_revision: Revision };
type Entitlement = {
  org_id: string;
  revision_id: string | null;
  version: number;
  plan_name: string | null;
  max_projects: number;
  max_endpoints: number;
  current_projects: number;
  current_endpoints: number;
  base_max_projects: number;
  base_max_endpoints: number;
};
type State = {
  entitlement: Entitlement;
  history: {
    id: string;
    version: number;
    max_projects: number;
    max_endpoints: number;
    revision_id: string | null;
  }[];
  can_manage: boolean;
};
type Pending = { path: string; method: string; body: string; key: string };

export function ServicePlans({ organizationId }: { organizationId: string }) {
  const [data, setData] = useState<State | null>(null),
    [plans, setPlans] = useState<Plan[]>([]),
    [canPublish, setCanPublish] = useState(false);
  const [selected, setSelected] = useState(''),
    [revisions, setRevisions] = useState<Revision[]>([]),
    [assign, setAssign] = useState('');
  const [name, setName] = useState(''),
    [projects, setProjects] = useState('50'),
    [endpoints, setEndpoints] = useState('200');
  const [busy, setBusy] = useState(false),
    [choosing, setChoosing] = useState(false),
    [uncertain, setUncertain] = useState(false),
    [error, setError] = useState(''),
    [notice, setNotice] = useState('');
  const pending = useRef<Pending | null>(null),
    active = useRef(organizationId),
    selection = useRef('');
  active.current = organizationId;
  const orgPath = '/api/v1/organizations/' + encodeURIComponent(organizationId) + '/service-plan',
    managePath =
      '/api/v1/platform/organizations/' + encodeURIComponent(organizationId) + '/service-plan';
  async function load() {
    const catalog = await api<{ items: Plan[]; can_manage: boolean }>('/api/v1/service-plans');
    let current: State;
    try {
      current = await api<State>(catalog.can_manage ? managePath : orgPath);
    } catch (e) {
      if (catalog.can_manage && e instanceof ApiError && e.status === 403)
        current = await api<State>(orgPath);
      else throw e;
    }
    const all = catalog.can_manage
      ? await api<{ items: Plan[] }>('/api/v1/platform/service-plans')
      : catalog;
    if (active.current !== organizationId) return;
    setPlans(all.items);
    setCanPublish(catalog.can_manage);
    setData(current);
    setAssign(current.entitlement.revision_id || '');
  }
  useEffect(() => {
    active.current = organizationId;
    selection.current = '';
    setChoosing(false);
    setData(null);
    setSelected('');
    setRevisions([]);
    setError('');
    setNotice('');
    setUncertain(false);
    pending.current = null;
    load().catch((e) => {
      if (active.current === organizationId) setError(String(e));
    });
    return () => {
      active.current = '';
    };
  }, [organizationId]);
  async function choose(id: string) {
    selection.current = id;
    setSelected(id);
    setRevisions([]);
    setError('');
    if (!id) {
      setName('');
      setProjects('50');
      setEndpoints('200');
      setChoosing(false);
      return;
    }
    setChoosing(true);
    try {
      const result = await api<{ plan: { name: string }; revisions: Revision[] }>(
        '/api/v1/platform/service-plans/' + encodeURIComponent(id),
      );
      if (active.current === organizationId && selection.current === id) {
        setRevisions(result.revisions);
        setName(result.plan.name);
        setProjects(String(result.revisions[0].max_projects));
        setEndpoints(String(result.revisions[0].max_endpoints));
      }
    } catch (e) {
      if (active.current === organizationId && selection.current === id) setError(String(e));
    } finally {
      if (active.current === organizationId && selection.current === id) setChoosing(false);
    }
  }
  async function send(value: Pending) {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const result = await api<{ plan_id?: string }>(value.path, {
        method: value.method,
        headers: { 'Idempotency-Key': value.key },
        body: value.body,
      });
      if (active.current !== organizationId) return;
      pending.current = null;
      setUncertain(false);
      setNotice('原操作已确认，配额与不可变历史已保存。');
      await load();
      if (result.plan_id) await choose(result.plan_id);
    } catch (e) {
      if (active.current === organizationId) {
        setError(String(e));
        const rejected = e instanceof ApiError && e.status >= 400 && e.status < 500;
        if (rejected) pending.current = null;
        setUncertain(!rejected);
        await load().catch(() => {});
      }
    } finally {
      if (active.current === organizationId) setBusy(false);
    }
  }
  function mutate(path: string, method: string, value: unknown) {
    if (pending.current) return;
    const request = { path, method, body: JSON.stringify(value), key: newRequestKey() };
    pending.current = request;
    void send(request);
  }
  const currentPlan = plans.find((p) => p.id === selected),
    disabled = busy || choosing || uncertain;
  const publishable =
    name.trim().length > 0 &&
    /^[0-9]+$/.test(projects) &&
    /^[0-9]+$/.test(endpoints) &&
    Number(projects) >= 1 &&
    Number(projects) <= 10000 &&
    Number(endpoints) >= 1 &&
    Number(endpoints) <= 10000;
  return (
    <section className="panel identity-panel" aria-label="组织服务计划">
      <h2>服务计划与资源配额</h2>
      <p>计划版本固定资源上限；发布新版本不会自动切换现有组织，禁用计划也不会删除现有资源。</p>
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {uncertain && (
        <p role="status">
          原计划操作结果尚未确认。
          <button disabled={busy} onClick={() => pending.current && void send(pending.current)}>
            读取原计划操作回执
          </button>
        </p>
      )}
      {data && (
        <>
          <p>
            当前计划：{data.entitlement.plan_name || '原有组织配额'} · 版本{' '}
            {data.entitlement.version}
          </p>
          <p>
            项目 {data.entitlement.current_projects} / {data.entitlement.max_projects}；Endpoint{' '}
            {data.entitlement.current_endpoints} / {data.entitlement.max_endpoints}
          </p>
          {data.can_manage && (
            <fieldset>
              <legend>分配组织计划</legend>
              <label>
                计划版本
                <select
                  aria-label="组织计划版本"
                  value={assign}
                  disabled={disabled}
                  onChange={(e) => setAssign(e.target.value)}
                >
                  <option value="">
                    恢复首次分配前的原配额（{data.entitlement.base_max_projects}/
                    {data.entitlement.base_max_endpoints}）
                  </option>
                  {plans
                    .filter((p) => p.state === 'active')
                    .map((p) => (
                      <option key={p.latest_revision.id} value={p.latest_revision.id}>
                        {p.name} · 修订 {p.latest_revision.revision} ·{' '}
                        {p.latest_revision.max_projects}/{p.latest_revision.max_endpoints}
                      </option>
                    ))}
                  {data.entitlement.revision_id &&
                    !plans.some((p) => p.latest_revision.id === data.entitlement.revision_id) && (
                      <option value={data.entitlement.revision_id}>
                        当前保留的原修订 {data.entitlement.revision_id}
                      </option>
                    )}
                  {revisions
                    .filter(
                      (r) =>
                        !plans.some((p) => p.latest_revision.id === r.id) &&
                        r.id !== data.entitlement.revision_id,
                    )
                    .map((r) => (
                      <option key={r.id} value={r.id}>
                        所选计划修订 {r.revision} · {r.max_projects}/{r.max_endpoints}
                      </option>
                    ))}
                </select>
              </label>
              <button
                disabled={disabled}
                onClick={() =>
                  mutate(managePath, 'PUT', {
                    version: data.entitlement.version,
                    revision_id: assign || null,
                  })
                }
              >
                确认分配组织计划
              </button>
            </fieldset>
          )}
          {data.history.length > 0 && (
            <details>
              <summary>组织计划历史</summary>
              <ul>
                {data.history.map((item) => (
                  <li key={item.id}>
                    版本 {item.version} · {item.revision_id || '原有组织配额'} · {item.max_projects}
                    /{item.max_endpoints}
                  </li>
                ))}
              </ul>
            </details>
          )}
        </>
      )}
      {canPublish && (
        <fieldset>
          <legend>平台计划管理</legend>
          <label>
            编辑计划
            <select
              aria-label="编辑服务计划"
              value={selected}
              disabled={disabled}
              onChange={(e) => void choose(e.target.value)}
            >
              <option value="">创建新计划</option>
              {plans.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name} · {p.state} · {p.version}
                </option>
              ))}
            </select>
          </label>
          <label>
            计划名称
            <input
              aria-label="服务计划名称"
              value={name}
              maxLength={80}
              disabled={disabled}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label>
            项目上限
            <input
              aria-label="服务计划项目上限"
              inputMode="numeric"
              value={projects}
              disabled={disabled}
              onChange={(e) => setProjects(e.target.value)}
            />
          </label>
          <label>
            Endpoint 上限
            <input
              aria-label="服务计划 Endpoint 上限"
              inputMode="numeric"
              value={endpoints}
              disabled={disabled}
              onChange={(e) => setEndpoints(e.target.value)}
            />
          </label>
          <button
            disabled={disabled || !publishable || currentPlan?.state === 'disabled'}
            onClick={() =>
              mutate(
                '/api/v1/platform/service-plans' +
                  (selected ? '/' + encodeURIComponent(selected) + '/revisions' : ''),
                'POST',
                {
                  version: currentPlan?.version || 0,
                  name,
                  max_projects: Number(projects),
                  max_endpoints: Number(endpoints),
                },
              )
            }
          >
            {selected ? '发布新的计划修订' : '创建服务计划'}
          </button>
          {currentPlan && (
            <button
              disabled={disabled}
              onClick={() =>
                mutate(
                  '/api/v1/platform/service-plans/' + encodeURIComponent(currentPlan.id) + '/state',
                  'PUT',
                  {
                    version: currentPlan.version,
                    state: currentPlan.state === 'active' ? 'disabled' : 'active',
                  },
                )
              }
            >
              {currentPlan.state === 'active' ? '禁用服务计划' : '启用服务计划'}
            </button>
          )}
        </fieldset>
      )}
      <small>
        分配会拒绝低于当前资源使用量的上限。此处管理自托管资源权益，不会自动扣款或发放 AI 额度。
      </small>
    </section>
  );
}
