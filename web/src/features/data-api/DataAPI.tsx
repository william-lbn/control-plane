import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Operation, Runtime } from '../../api';
import { Empty, PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { followOperation } from '../../shared/followOperation';

type Spec = {
  database: string;
  schema: string;
  issuer: string;
  audience: string;
  jwks: unknown;
  allowed_origins: string[];
};
type Instance = {
  branch_id: string;
  generation: number;
  state: string;
  driver_enabled: boolean;
  request_role: string;
  public_endpoint: string;
  spec: Spec | null;
  runtime?: Runtime;
};

export function DataAPI({
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
  const [instance, setInstance] = useState<Instance | null>(null);
  const [database, setDatabase] = useState('postgres');
  const [schema, setSchema] = useState('app_data');
  const [issuer, setIssuer] = useState('');
  const [audience, setAudience] = useState('');
  const [jwks, setJWKS] = useState('');
  const [origins, setOrigins] = useState('');
  const [operation, setOperation] = useState<Operation | null>(null);
  const [trackingNotice, setTrackingNotice] = useState('');
  const polling = useRef<AbortController | null>(null);
  const [busy, setBusy] = useState(false);
  const [token, setToken] = useState('');
  const [resource, setResource] = useState('notes');
  const [method, setMethod] = useState('GET');
  const [body, setBody] = useState('{}');
  const [response, setResponse] = useState('');
  const [sending, setSending] = useState(false);
  const epoch = useRef(0);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/data-api`;

  useEffect(() => {
    polling.current?.abort();
    const current = ++epoch.current;
    setInstance(null);
    setToken('');
    setResponse('');
    setOperation(null);
    setTrackingNotice('');
    setDatabase('postgres');
    setSchema('app_data');
    setIssuer('');
    setAudience('');
    setJWKS('');
    setOrigins('');
    setBusy(false);
    setSending(false);
    if (branch)
      api<Instance>(path)
        .then((value) => {
          if (epoch.current !== current) return;
          setInstance(value);
          if (value.spec) {
            setDatabase(value.spec.database);
            setSchema(value.spec.schema);
            setIssuer(value.spec.issuer);
            setAudience(value.spec.audience);
            setJWKS(JSON.stringify(value.spec.jwks, null, 2));
            setOrigins(value.spec.allowed_origins.join('\n'));
          }
        })
        .catch((error) => {
          if (epoch.current === current) showError(error);
        });
    return () => {
      epoch.current++;
      polling.current?.abort();
    };
  }, [path]);

  async function observe(id: string, currentEpoch: number) {
    polling.current?.abort();
    const controller = new AbortController();
    polling.current = controller;
    const current = await followOperation({
      id,
      signal: controller.signal,
      read: (signal) => api<Operation>(`${projectPath(projectId)}/operations/${id}`, { signal }),
      onValue: (value) => {
        if (epoch.current === currentEpoch) {
          setOperation(value);
          setTrackingNotice('');
        }
      },
      onUnavailable: () => {
        if (epoch.current === currentEpoch)
          setTrackingNotice('服务暂时不可用，正在继续查询已受理的操作。');
      },
    });
    if (epoch.current !== currentEpoch) return;
    if (!current) throw new Error('状态查询暂未完成，可继续查询原操作。');
    const updated = await api<Instance>(path, {
      signal: AbortSignal.any([controller.signal, AbortSignal.timeout(10_000)]),
    });
    if (epoch.current !== currentEpoch) return;
    setInstance(updated);
    if (current.state !== 'succeeded')
      throw new Error(`${current.error_message || '操作未成功'} · ${id}`);
  }

  async function continueTracking() {
    if (!operation || busy) return;
    const currentEpoch = epoch.current;
    setBusy(true);
    try {
      await observe(operation.id, currentEpoch);
    } catch (error) {
      if (epoch.current === currentEpoch) showError(error);
    } finally {
      if (epoch.current === currentEpoch) setBusy(false);
    }
  }

  async function mutate(disable: boolean) {
    if (!instance || (operation && ['queued', 'running', 'retry_wait'].includes(operation.state)))
      return;
    const currentEpoch = epoch.current;
    setBusy(true);
    try {
      const spec: Spec | undefined = disable
        ? undefined
        : {
            database,
            schema,
            issuer,
            audience,
            jwks: JSON.parse(jwks),
            allowed_origins: origins
              .split(/[\n,]/)
              .map((value) => value.trim())
              .filter(Boolean),
          };
      const accepted = await api<{ operation: Operation }>(path, {
        method: disable ? 'DELETE' : 'POST',
        headers: { 'Idempotency-Key': newRequestKey(), 'If-Match': `"${instance.generation}"` },
        ...(disable ? {} : { body: JSON.stringify(spec) }),
      });
      if (epoch.current !== currentEpoch) return;
      setOperation(accepted.operation);
      await observe(accepted.operation.id, currentEpoch);
    } catch (error) {
      if (epoch.current === currentEpoch) showError(error);
    } finally {
      if (epoch.current === currentEpoch) setBusy(false);
    }
  }

  async function sendRequest() {
    if (!instance || !/^[a-zA-Z][a-zA-Z0-9_]{0,62}$/.test(resource)) {
      showError('请输入表名，不接受 URL 或路径');
      return;
    }
    const currentEpoch = epoch.current;
    setSending(true);
    setResponse('');
    try {
      const result = await api<{ status: number; body: string; truncated: boolean }>(
        path + '/request',
        {
          method: 'POST',
          body: JSON.stringify({
            method,
            table: resource,
            application_token: token,
            ...(method === 'GET' ? {} : { body: JSON.parse(body) }),
          }),
        },
      );
      if (epoch.current !== currentEpoch) return;
      setResponse(
        `HTTP ${result.status}\n${result.body}${result.truncated ? '\n[响应已截断]' : ''}`,
      );
    } catch (error) {
      if (epoch.current === currentEpoch) showError(error);
    } finally {
      if (epoch.current === currentEpoch) setSending(false);
    }
  }
  async function useBranchAuth() {
    const currentEpoch = epoch.current;
    setBusy(true);
    try {
      const auth = await api<{ state: string; issuer: string; audience: string }>(
        `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/auth`,
      );
      if (auth.state !== 'active') throw new Error('先启用当前分支 Auth');
      const response = await fetch(`/auth/v1/${branch}/jwks`, { credentials: 'same-origin' });
      if (!response.ok) throw new Error('当前分支 Auth JWKS 不可用');
      const keys = await response.json();
      if (epoch.current !== currentEpoch) return;
      setIssuer(auth.issuer);
      setAudience(auth.audience);
      setJWKS(JSON.stringify(keys, null, 2));
    } catch (error) {
      if (epoch.current === currentEpoch) showError(error);
    } finally {
      if (epoch.current === currentEpoch) setBusy(false);
    }
  }
  if (!branches.length)
    return <Empty title="先创建数据库分支" message="Data API 绑定分支的读写 Endpoint。" />;
  return (
    <>
      <PageHeading
        kicker="PROJECT / DATA API"
        title="Data API"
        description="通过分支级 JWT 身份访问 Postgres REST API；数据库 RLS 决定每一行的访问权限。"
      />
      <div className="toolbar">
        <label>
          分支{' '}
          <select
            aria-label="Data API 分支"
            value={branch}
            onChange={(e) => setBranch(e.target.value)}
            disabled={busy || sending}
          >
            {branches.map((b) => (
              <option value={b.id} key={b.id}>
                {b.name}
              </option>
            ))}
          </select>
        </label>
        {instance && status(instance.state)}
      </div>
      <div className="subtle-note">
        当前 Native Driver 需要显式启用实验室 HTTP 配置。全链路可信 TLS、动态
        JWKS、RPC、视图及分区表仍有独立验收门槛。启用不会自动猜测或创建 RLS 策略。
      </div>
      <section className="table-card backend-panel" style={{ padding: 24 }}>
        <h3>身份与数据库</h3>
        <button
          className="button"
          type="button"
          disabled={!canEdit || busy || instance?.state === 'active'}
          onClick={() => void useBranchAuth()}
        >
          使用当前分支 Auth
        </button>
        <p>
          先通过 SQL 工作台准备专用应用 schema，并为每张表启用 ENABLE / FORCE ROW LEVEL
          SECURITY。避免授予 PUBLIC 访问权限。请求角色：
          <code>{instance?.request_role || '加载中'}</code>
        </p>
        <form
          className="backend-form"
          onSubmit={(e) => {
            e.preventDefault();
            void mutate(false);
          }}
        >
          <label>
            数据库{' '}
            <input
              aria-label="Data API 数据库"
              value={database}
              onChange={(e) => setDatabase(e.target.value)}
              required
              disabled={busy || instance?.state === 'active'}
            />
          </label>
          <label>
            应用 Schema{' '}
            <input
              aria-label="Data API Schema"
              value={schema}
              onChange={(e) => setSchema(e.target.value)}
              required
              disabled={busy || instance?.state === 'active'}
            />
          </label>
          <label>
            JWT Issuer{' '}
            <input
              aria-label="JWT Issuer"
              value={issuer}
              onChange={(e) => setIssuer(e.target.value)}
              required
              disabled={busy || instance?.state === 'active'}
            />
          </label>
          <label>
            分支 Audience{' '}
            <input
              aria-label="JWT Audience"
              value={audience}
              onChange={(e) => setAudience(e.target.value)}
              required
              disabled={busy || instance?.state === 'active'}
            />
          </label>
          <label className="backend-wide">
            Provider 公钥 JWKS{' '}
            <textarea
              aria-label="Provider JWKS"
              value={jwks}
              onChange={(e) => setJWKS(e.target.value)}
              rows={6}
              required
              disabled={busy || instance?.state === 'active'}
            />
          </label>
          <p className="subtle-note">
            只填写公钥；不接受私钥。角色由 Driver 管理，Provider Token 请省略 role claim。Audience
            应为此分支专用；克隆分支不会自动启用 Data API。
          </p>
          <label className="backend-wide">
            允许的浏览器来源（可选，每行一个 HTTPS Origin）{' '}
            <textarea
              aria-label="Data API 允许来源"
              value={origins}
              onChange={(e) => setOrigins(e.target.value)}
              rows={3}
              placeholder="https://app.example.com"
              disabled={busy || instance?.state === 'active'}
            />
          </label>
          <p className="subtle-note">
            仅填写应用页面的协议和主机，例如 https://app.example.com；不支持通配符、路径或 HTTP。
            留空仍允许服务器调用和本控制台 Explorer。
          </p>
          <div className="actions">
            <button
              type="submit"
              className="button primary"
              disabled={
                busy ||
                (!!operation && ['queued', 'running', 'retry_wait'].includes(operation.state)) ||
                !canEdit ||
                !instance?.driver_enabled ||
                !['disabled'].includes(instance?.state || '')
              }
            >
              启用 Data API
            </button>
            <button
              type="button"
              className="button"
              disabled={
                busy ||
                (!!operation && ['queued', 'running', 'retry_wait'].includes(operation.state)) ||
                !canEdit ||
                !instance ||
                ['disabled', 'provisioning', 'disabling'].includes(instance.state)
              }
              onClick={() => void mutate(true)}
            >
              停用 Data API
            </button>
          </div>
        </form>
        {operation && (
          <div role="status" data-testid="data-api-operation">
            {operation.id} · {operation.state}
            {trackingNotice && <p>{trackingNotice}</p>}
            {!busy && ['queued', 'running', 'retry_wait'].includes(operation.state) && (
              <button className="button" onClick={continueTracking}>
                继续查询操作
              </button>
            )}
          </div>
        )}
        {instance && (
          <p data-testid="data-api-state">
            {instance.state} · generation {instance.generation}
            {instance.runtime && ` · runtime ${instance.runtime.observed_state}`}
          </p>
        )}
      </section>
      <section className="table-card backend-panel" style={{ padding: 24, marginTop: 24 }}>
        <h3>API Explorer</h3>
        <p>
          入口 <code>{instance?.public_endpoint}</code>。应用 JWT
          仅保存在此页面内存，切换分支时清除。Explorer 使用受会话与 CSRF
          保护的服务器测试入口；分支服务仅接收应用 JWT。
        </p>
        <label>
          应用 JWT{' '}
          <input
            aria-label="应用 JWT"
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            autoComplete="off"
          />
        </label>
        <label>
          表名{' '}
          <input
            aria-label="Data API 表名"
            value={resource}
            onChange={(e) => setResource(e.target.value)}
          />
        </label>
        <label>
          方法{' '}
          <select
            aria-label="Data API 方法"
            value={method}
            onChange={(e) => setMethod(e.target.value)}
          >
            <option>GET</option>
            <option>POST</option>
          </select>
        </label>
        {method === 'POST' && (
          <label>
            JSON Body{' '}
            <textarea
              aria-label="Data API Body"
              value={body}
              onChange={(e) => setBody(e.target.value)}
              rows={4}
            />
          </label>
        )}
        <button
          className="button primary"
          disabled={sending || !canEdit || instance?.state !== 'active' || !token}
          onClick={() => void sendRequest()}
        >
          发送 Data API 请求
        </button>
        <pre data-testid="data-api-response">{response}</pre>
      </section>
    </>
  );
}
