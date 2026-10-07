import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Operation, Runtime } from '../../api';
import { PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { followOperation } from '../../shared/followOperation';

type Instance = {
  branch_id: string;
  generation: number;
  state: string;
  driver_enabled: boolean;
  public_endpoint: string;
  issuer: string;
  audience: string;
  spec: { database: string; allowed_origins: string[] } | null;
  runtime?: Runtime;
};
type AppUser = {
  id: string;
  name: string;
  email: string;
  email_verified?: boolean;
  created_at?: string;
};

export function ManagedAuth({
  projectId,
  branches,
  canAdmin,
  showError,
}: {
  projectId: string;
  branches: Branch[];
  canAdmin: boolean;
  showError: (e: unknown) => void;
}) {
  const [branch, setBranch] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [instance, setInstance] = useState<Instance | null>(null);
  const [database, setDatabase] = useState('postgres');
  const [origins, setOrigins] = useState('');
  const [busy, setBusy] = useState(false);
  const [operation, setOperation] = useState<Operation | null>(null);
  const [notice, setNotice] = useState('');
  const [users, setUsers] = useState<AppUser[]>([]);
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [session, setSession] = useState<AppUser | null>(null);
  const [appBusy, setAppBusy] = useState(false);
  const [appNotice, setAppNotice] = useState('');
  const [tokenReady, setTokenReady] = useState(false);
  const token = useRef('');
  const epoch = useRef(0);
  const tracking = useRef<AbortController | null>(null);
  const uncertainKey = useRef<{ fingerprint: string; key: string } | null>(null);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}/auth`;
  async function load(current: number) {
    const value = await api<Instance>(path);
    if (epoch.current !== current) return;
    setInstance(value);
    if (value.spec) {
      setDatabase(value.spec.database);
      setOrigins(value.spec.allowed_origins.join('\n'));
    }
  }
  useEffect(() => {
    const current = ++epoch.current;
    tracking.current?.abort();
    uncertainKey.current = null;
    setInstance(null);
    setOperation(null);
    setUsers([]);
    setSession(null);
    setPassword('');
    token.current = '';
    setTokenReady(false);
    setBusy(false);
    setDatabase('postgres');
    setOrigins('');
    setNotice('');
    setAppNotice('');
    if (branch && canAdmin) void load(current).catch(showError);
    return () => tracking.current?.abort();
  }, [branch, projectId, canAdmin]);
  async function mutate(disable: boolean) {
    if (!instance) return;
    const current = epoch.current;
    const body = disable
      ? undefined
      : {
          database,
          allowed_origins: origins
            .split('\n')
            .map((v) => v.trim())
            .filter(Boolean),
        };
    const fingerprint = JSON.stringify([path, instance.generation, disable, body]);
    if (uncertainKey.current?.fingerprint !== fingerprint)
      uncertainKey.current = { fingerprint, key: newRequestKey() };
    setBusy(true);
    setNotice('');
    try {
      const result = await api<{ operation: Operation }>(path, {
        method: disable ? 'DELETE' : 'POST',
        headers: {
          'If-Match': `"${instance.generation}"`,
          'Idempotency-Key': uncertainKey.current.key,
        },
        body: body ? JSON.stringify(body) : undefined,
      });
      if (epoch.current !== current) return;
      setOperation(result.operation);
      const controller = new AbortController();
      tracking.current = controller;
      const final = await followOperation<Operation>({
        id: result.operation.id,
        signal: controller.signal,
        read: (signal) =>
          api<Operation>(`${projectPath(projectId)}/operations/${result.operation.id}`, { signal }),
        onValue: (v) => {
          if (epoch.current === current) {
            setOperation(v);
            setNotice('');
          }
        },
        onUnavailable: () => {
          if (epoch.current === current) setNotice('状态服务暂不可用，继续查询既有 Operation。');
        },
      });
      if (epoch.current !== current) return;
      if (!final) {
        setNotice('状态查询尚未完成；请在操作记录中继续查询原 Operation。');
        return;
      }
      setOperation(final);
      await load(current);
      if (final.state === 'succeeded') {
        uncertainKey.current = null;
        setUsers([]);
        token.current = '';
        setTokenReady(false);
        setSession(null);
      }
    } catch (e) {
      if (epoch.current === current) showError(e);
    } finally {
      if (epoch.current === current) setBusy(false);
    }
  }
  async function appCall(action: string, body?: unknown) {
    const current = epoch.current;
    setAppBusy(true);
    setAppNotice('');
    try {
      const response = await fetch(`/auth/v1/${branch}/${action}`, {
        method: body ? 'POST' : 'GET',
        credentials: 'same-origin',
        headers: body ? { 'Content-Type': 'application/json' } : {},
        body: body ? JSON.stringify(body) : undefined,
      });
      const result = await response.json();
      if (epoch.current !== current) return;
      if (!response.ok) throw new Error(`应用 Auth 请求失败（${response.status}）`);
      if (action === 'get-session') {
        setSession(result?.user || null);
        setAppNotice(result?.user ? '应用会话有效' : '没有应用会话');
      } else if (action === 'token') {
        token.current = result.token;
        setTokenReady(true);
        setAppNotice('已签发分支专用 JWT（5 分钟）；仅保存在当前页面内存');
      } else if (action === 'sign-out') {
        setSession(null);
        token.current = '';
        setTokenReady(false);
        setAppNotice('应用会话已撤销');
      } else {
        setSession(result.user || null);
        setAppNotice(action === 'sign-up/email' ? '应用用户注册成功' : '应用用户登录成功');
      }
    } catch (e) {
      if (epoch.current === current) setAppNotice(e instanceof Error ? e.message : '应用请求失败');
    } finally {
      if (epoch.current === current) {
        setAppBusy(false);
        setPassword('');
      }
    }
  }
  async function readUsers() {
    const current = epoch.current;
    setBusy(true);
    try {
      const result = await api<{ items: AppUser[] }>(path + '/users', {
        method: 'POST',
        body: '{}',
      });
      if (epoch.current === current) setUsers(result.items);
    } catch (e) {
      if (epoch.current === current) showError(e);
    } finally {
      if (epoch.current === current) setBusy(false);
    }
  }
  if (!canAdmin)
    return (
      <>
        <PageHeading
          kicker="Branch identity"
          title="Auth"
          description="应用身份配置和用户资料仅向项目管理员开放。"
        />
      </>
    );
  const active = instance?.state === 'active';
  return (
    <div data-testid="managed-auth-page">
      <PageHeading
        kicker="Branch identity · Better Auth"
        title="Auth"
        description="应用用户、账号与会话存储在当前数据库的 neon_auth schema，和 Console 账号独立。"
      />
      <div className="toolbar">
        <label>
          Auth 分支{' '}
          <select
            aria-label="Auth 分支"
            value={branch}
            onChange={(e) => setBranch(e.target.value)}
            disabled={busy}
          >
            {branches.map((b) => (
              <option key={b.id} value={b.id}>
                {b.name}
              </option>
            ))}
          </select>
        </label>
        {status(instance?.state || 'loading')}
        {instance?.runtime && status(instance.runtime.observed_state)}
      </div>
      <section className="table-card backend-panel" style={{ padding: 24 }}>
        <h3>分支身份服务</h3>
        <p>
          注册、登录、会话撤销与短期 JWT/JWKS 已提供。本版本需要显式启用实验室
          HTTP；邮件验证、OAuth、MFA 和全链路可信 TLS 尚未验收。
        </p>
        <p className="subtle-note">
          首次启用前，由数据库所有者通过 SQL 工作台执行权限委托。已有同名业务 schema 不会被接管。
        </p>
        <pre>
          GRANT CREATE, CONNECT ON DATABASE "{database.replaceAll('"', '""')}" TO control_probe WITH
          GRANT OPTION;
        </pre>
        <form
          className="backend-form"
          onSubmit={(e) => {
            e.preventDefault();
            void mutate(false);
          }}
        >
          <label>
            Auth 数据库{' '}
            <input
              aria-label="Auth 数据库"
              value={database}
              onChange={(e) => setDatabase(e.target.value)}
              disabled={busy || active}
              required
            />
          </label>
          <label>
            可信来源（每行一个精确 Origin）
            <textarea
              aria-label="Auth 可信来源"
              value={origins}
              onChange={(e) => setOrigins(e.target.value)}
              disabled={busy || active}
            />
          </label>
          <div className="backend-wide button-row">
            <button
              className="button primary"
              disabled={busy || !instance?.driver_enabled || instance.state !== 'disabled'}
            >
              启用 Auth
            </button>
            <button
              type="button"
              className="button"
              disabled={
                busy || !instance?.driver_enabled || !instance || instance.state === 'disabled'
              }
              onClick={() => void mutate(true)}
            >
              禁用 Auth
            </button>
            <button
              type="button"
              className="button"
              disabled={busy}
              onClick={() => void load(epoch.current).catch(showError)}
            >
              刷新 Auth 状态
            </button>
          </div>
        </form>
        {operation && (
          <div role="status" data-testid="auth-operation">
            {operation.id} · {operation.state}
            {notice && <p>{notice}</p>}
            {operation.state === 'failed' && (
              <p>请在操作记录中修复前置条件后重试原 Operation；无需创建新的服务意图。</p>
            )}
          </div>
        )}
        {instance && (
          <dl>
            <dt>应用地址 / JWT Issuer</dt>
            <dd>
              <code>{instance.public_endpoint}</code>
            </dd>
            <dt>Audience</dt>
            <dd>
              <code>{instance.audience}</code>
            </dd>
            <dt>JWKS</dt>
            <dd>
              <code>{instance.public_endpoint}/jwks</code>
            </dd>
          </dl>
        )}
      </section>
      <section className="table-card backend-panel" style={{ padding: 24 }}>
        <h3>应用登录体验</h3>
        <p>
          这里测试数据库应用的身份流程。请求经过受限分支服务，Console cookie
          不会转发；密码不会写入控制面元数据或测试报告。
        </p>
        <div className="backend-form">
          <label>
            应用姓名
            <input
              aria-label="应用姓名"
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={appBusy || !active}
              autoComplete="off"
            />
          </label>
          <label>
            应用邮箱
            <input
              aria-label="应用邮箱"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              disabled={appBusy || !active}
              autoComplete="off"
            />
          </label>
          <label>
            应用密码（至少 12 字符）
            <input
              aria-label="应用密码"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={appBusy || !active}
              autoComplete="new-password"
            />
          </label>
          <div className="backend-wide button-row">
            <button
              className="button primary"
              disabled={appBusy || !active || !email || !name || password.length < 12}
              onClick={() => void appCall('sign-up/email', { email, name, password })}
            >
              注册应用用户
            </button>
            <button
              className="button"
              disabled={appBusy || !active || !email || !password}
              onClick={() => void appCall('sign-in/email', { email, password })}
            >
              登录应用
            </button>
            <button
              className="button"
              disabled={appBusy || !active}
              onClick={() => void appCall('get-session')}
            >
              检查应用会话
            </button>
            <button
              className="button"
              disabled={appBusy || !active}
              onClick={() => void appCall('token')}
            >
              签发应用 JWT
            </button>
            <button
              className="button"
              disabled={appBusy || !active}
              onClick={() => void appCall('sign-out', {})}
            >
              退出应用
            </button>
          </div>
        </div>
        {session && (
          <p data-testid="auth-app-session">
            {session.name} · {session.email}
          </p>
        )}
        {tokenReady && (
          <p>
            JWT 已就绪（不展示凭据）。可从应用 SDK 使用 <code>/token</code> 获取并调用 Data API。
          </p>
        )}
        <div role="status" data-testid="auth-app-result">
          {appNotice}
        </div>
      </section>
      <section className="table-card backend-panel" style={{ padding: 24 }}>
        <div className="section-header">
          <h3>应用用户</h3>
          <button className="button" disabled={busy || !active} onClick={() => void readUsers()}>
            读取应用用户
          </button>
        </div>
        <p>
          此操作读取当前分支 SQL，可能唤醒
          Compute。状态刷新与监控不会为读取用户而唤醒数据库。最多显示最近 100 个用户。
        </p>
        <table>
          <thead>
            <tr>
              <th>姓名</th>
              <th>邮箱</th>
              <th>身份 ID</th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id}>
                <td>{u.name}</td>
                <td>{u.email}</td>
                <td>
                  <code>{u.id}</code>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
    </div>
  );
}
