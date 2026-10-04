import { useEffect, useRef, useState } from 'react';
import { api, projectPath, type Branch } from '../../api';
import { newRequestKey } from '../../shared/requestKey';
import { Empty, PageHeading, fmt } from '../../shared/ui';

type Credential = {
  id: string;
  name: string;
  issuer_id: string;
  branch_id: string;
  scopes: string[];
  branch_scope: 'self' | 'self_and_descendants';
  allowed_models: string[];
  generation: number;
  state: string;
  created_at: string;
  expires_at: string;
  rotated_at: string | null;
  can_manage?: boolean;
};
type Listing = {
  items: Credential[];
  enabled: boolean;
  inference_available: boolean;
  next_cursor: string;
};
type Reply = { credential: Credential; secret_available: boolean; api_token?: string };

export function BackendCredentials({
  projectId,
  branches,
  canEdit,
  showError,
}: {
  projectId: string;
  branches: Branch[];
  canEdit: boolean;
  showError: (error: unknown) => void;
}) {
  const [branch, setBranch] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [listing, setListing] = useState<Listing | null>(null);
  const [name, setName] = useState('server-app');
  const [days, setDays] = useState(7);
  const [scope, setScope] = useState<Credential['branch_scope']>('self_and_descendants');
  const [models, setModels] = useState('');
  const [busy, setBusy] = useState(false);
  const [oneTime, setOneTime] = useState<{ id: string; token: string } | null>(null);
  const [visible, setVisible] = useState(false);
  const [checkToken, setCheckToken] = useState('');
  const [checkModel, setCheckModel] = useState('');
  const [checkResult, setCheckResult] = useState('');
  const epoch = useRef(0);
  const base = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/credentials`;

  useEffect(() => {
    const current = ++epoch.current;
    setListing(null);
    setOneTime(null);
    setCheckToken('');
    setCheckResult('');
    setVisible(false);
    setBusy(false);
    if (branch)
      api<Listing>(base)
        .then((value) => {
          if (epoch.current === current) setListing(value);
        })
        .catch((error) => {
          if (epoch.current === current) showError(error);
        });
    return () => {
      epoch.current++;
    };
  }, [base]);

  async function mutate(action: 'create' | 'rotate' | 'revoke', credential?: Credential) {
    const current = epoch.current;
    setBusy(true);
    setOneTime(null);
    setVisible(false);
    try {
      const body = {
        name: credential?.name || name,
        scopes: ['ai_gateway:invoke'],
        branch_scope: credential?.branch_scope || scope,
        allowed_models:
          credential?.allowed_models ||
          models
            .split(',')
            .map((m) => m.trim())
            .filter(Boolean),
        expires_at: new Date(Date.now() + days * 86400000).toISOString(),
      };
      const reply = await api<Reply>(
        base + (credential ? `/${credential.id}${action === 'rotate' ? '/rotate' : ''}` : ''),
        {
          method: action === 'revoke' ? 'DELETE' : 'POST',
          headers: {
            'Idempotency-Key': newRequestKey(),
            ...(credential ? { 'If-Match': `"${credential.generation}"` } : {}),
          },
          ...(action === 'revoke' ? {} : { body: JSON.stringify(body) }),
        },
      );
      if (epoch.current !== current) return;
      if (reply.api_token) setOneTime({ id: reply.credential.id, token: reply.api_token });
      else if (action !== 'revoke' && !reply.secret_available)
        showError('本次请求已处理，Token 无法再次读回。请轮换凭据取得新 Token。');
      const updated = await api<Listing>(base);
      if (epoch.current === current) setListing(updated);
    } catch (error) {
      if (epoch.current === current) showError(error);
    } finally {
      if (epoch.current === current) setBusy(false);
    }
  }

  async function check() {
    const current = epoch.current;
    setBusy(true);
    setCheckResult('');
    try {
      const value = await api<{ credential_id: string; branch_id: string }>(base + '/check', {
        method: 'POST',
        body: JSON.stringify({ api_token: checkToken, model: checkModel }),
      });
      if (epoch.current === current)
        setCheckResult(
          `授权通过 · ${value.credential_id} → ${value.branch_id}。此检查不调用模型，AI Gateway 推理仍待实现。`,
        );
    } catch (error) {
      if (epoch.current === current) {
        setCheckResult('授权未通过');
        showError(error);
      }
    } finally {
      if (epoch.current === current) setBusy(false);
    }
  }

  async function nextPage() {
    if (!listing?.next_cursor) return;
    const current = epoch.current;
    setBusy(true);
    try {
      const updated = await api<Listing>(
        base + '?cursor=' + encodeURIComponent(listing.next_cursor),
      );
      if (epoch.current === current) setListing(updated);
    } catch (error) {
      if (epoch.current === current) showError(error);
    } finally {
      if (epoch.current === current) setBusy(false);
    }
  }

  if (!branches.length)
    return <Empty title="先创建分支" message="应用凭据绑定一个分支及明确的授权范围。" />;
  return (
    <>
      <PageHeading
        kicker="BRANCH / CREDENTIALS"
        title="应用凭据"
        description="为应用创建分支级凭据，独立管理范围、到期时间、轮换和撤销。"
      />
      <div className="toolbar">
        <label>
          分支{' '}
          <select
            aria-label="凭据分支"
            value={branch}
            disabled={busy}
            onChange={(e) => setBranch(e.target.value)}
          >
            {branches.map((b) => (
              <option key={b.id} value={b.id}>
                {b.name}
              </option>
            ))}
          </select>
        </label>
        {listing && (
          <span className="tag">{listing.enabled ? '凭据管理已启用' : '密钥服务未配置'}</span>
        )}
      </div>
      <div className="subtle-note">
        当前支持 ai_gateway:invoke 的凭据管理和真实授权检查。模型供应商、推理网关及 Playground
        尚未启用；创建凭据不会产生可调用的模型服务。Token 到期会强制失效。
      </div>
      {oneTime && (
        <section
          className="table-card backend-panel"
          style={{ padding: 24 }}
          data-testid="backend-one-time"
        >
          <h3>保存应用 Token</h3>
          <p>
            仅此次显示。关闭、切换分支或离开页面后无法再次读取；丢失时需轮换。不要把服务器应用 Token
            编进网页。
          </p>
          <small>{oneTime.id}</small>
          <input
            aria-label="新 Backend Token"
            type={visible ? 'text' : 'password'}
            value={oneTime.token}
            readOnly
            autoComplete="off"
            style={{ width: '100%' }}
          />
          <div className="toolbar">
            <button className="button" onClick={() => setVisible(!visible)}>
              {visible ? '隐藏 Token' : '显示 Token'}
            </button>
            <button
              className="button"
              onClick={() =>
                void (async () => {
                  try {
                    if (!navigator.clipboard)
                      throw new Error('复制需要 HTTPS，可选择输入框手动复制');
                    await navigator.clipboard.writeText(oneTime.token);
                  } catch (error) {
                    showError(error);
                  }
                })()
              }
            >
              复制 Token
            </button>
            <button className="button" onClick={() => setOneTime(null)}>
              已保存，关闭
            </button>
          </div>
        </section>
      )}
      {canEdit && (
        <section className="table-card backend-panel" style={{ padding: 24 }}>
          <h3>创建应用凭据</h3>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void mutate('create');
            }}
          >
            <div className="toolbar">
              <label>
                名称{' '}
                <input
                  aria-label="凭据名称"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  maxLength={100}
                  required
                />
              </label>
              <label>
                到期{' '}
                <select
                  aria-label="凭据有效天数"
                  value={days}
                  onChange={(e) => setDays(Number(e.target.value))}
                >
                  {[1, 7, 14, 30].map((d) => (
                    <option key={d} value={d}>
                      {d} 天
                    </option>
                  ))}
                </select>
              </label>
              <label>
                范围{' '}
                <select
                  aria-label="凭据分支范围"
                  value={scope}
                  onChange={(e) => setScope(e.target.value as Credential['branch_scope'])}
                >
                  <option value="self_and_descendants">当前分支及后代</option>
                  <option value="self">仅当前分支</option>
                </select>
              </label>
            </div>
            <label>
              模型约束{' '}
              <input
                aria-label="凭据模型约束"
                value={models}
                onChange={(e) => setModels(e.target.value)}
                placeholder="逗号分隔；留空仍受实际模型权限控制"
                style={{ width: '100%' }}
              />
            </label>
            <p>
              <span className="tag">ai_gateway:invoke</span>{' '}
              模型约束只收窄访问权，不授予模型或平台运营权限。
            </p>
            <button className="button primary" disabled={busy || !listing?.enabled}>
              创建应用凭据
            </button>
          </form>
        </section>
      )}
      <section className="table-card">
        <table>
          <thead>
            <tr>
              <th>凭据</th>
              <th>范围</th>
              <th>状态</th>
              <th>到期</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {listing?.items.map((c) => (
              <tr key={c.id} data-testid={`backend-row-${c.id}`}>
                <td>
                  <strong>{c.name}</strong>
                  <br />
                  <small>
                    {c.id} · v{c.generation}
                  </small>
                </td>
                <td>
                  {c.branch_scope === 'self' ? '当前分支' : '分支及后代'}
                  <br />
                  <small>
                    {c.allowed_models.length ? c.allowed_models.join(', ') : '实际获授权模型'}
                  </small>
                </td>
                <td>
                  <span className={`status status-${c.state}`}>
                    ●{' '}
                    {c.state === 'active'
                      ? '未到期 / 未撤销'
                      : c.state === 'revoked'
                        ? '已撤销'
                        : c.state === 'expired'
                          ? '已过期'
                          : '状态未知'}
                  </span>
                </td>
                <td>{fmt(c.expires_at)}</td>
                <td>
                  {canEdit && c.can_manage && c.state !== 'revoked' && (
                    <>
                      <button
                        className="button small"
                        disabled={busy || !listing.enabled}
                        onClick={() => void mutate('rotate', c)}
                      >
                        轮换凭据
                      </button>
                      <button
                        className="button small"
                        disabled={busy || !listing.enabled}
                        onClick={() => void mutate('revoke', c)}
                      >
                        撤销凭据
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {listing?.next_cursor && (
          <button className="button" disabled={busy} onClick={() => void nextPage()}>
            下一页
          </button>
        )}
      </section>
      {canEdit && (
        <section className="table-card backend-panel" style={{ padding: 24 }}>
          <h3>检查分支与模型访问范围</h3>
          <p>仅检查凭据和当前授权；不会产生模型推理费用。</p>
          <label>
            应用 Token{' '}
            <input
              aria-label="检查 Backend Token"
              type="password"
              value={checkToken}
              onChange={(e) => setCheckToken(e.target.value)}
              autoComplete="off"
              style={{ width: '100%' }}
            />
          </label>
          <label>
            模型 ID（可选）{' '}
            <input
              aria-label="检查模型 ID"
              value={checkModel}
              onChange={(e) => setCheckModel(e.target.value)}
            />
          </label>
          <button
            className="button"
            disabled={busy || !listing?.enabled || !checkToken}
            onClick={() => void check()}
          >
            检查应用凭据
          </button>
          {checkResult && (
            <p role="status" data-testid="backend-check-result">
              {checkResult}
            </p>
          )}
        </section>
      )}
    </>
  );
}
