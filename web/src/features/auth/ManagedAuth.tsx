import { useEffect, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Operation, Runtime } from '../../api';
import { PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { followOperation } from '../../shared/followOperation';
import { AuthSecurityPanel } from './AuthSecurityPanel';
import { AuthRecoveryPanel } from './AuthRecoveryPanel';
import { AuthOTPPanel } from './AuthOTPPanel';
import { AuthMagicLinkPanel } from './AuthMagicLinkPanel';
import { AuthTOTPPanel } from './AuthTOTPPanel';
import { AuthAdminPanel } from './AuthAdminPanel';

type Instance = {
  branch_id: string;
  generation: number;
  state: string;
  driver_enabled: boolean;
  public_endpoint: string;
  issuer: string;
  audience: string;
  spec: {
    database: string;
    allowed_origins: string[];
    require_email_verification?: boolean;
    email_otp_enabled?: boolean;
    magic_link_enabled?: boolean;
    magic_link_expires_in?: number;
    magic_link_allow_registration?: boolean;
    two_factor_enabled?: boolean;
    inherit_two_factor?: boolean;
    admin_enabled?: boolean;
    admin_bootstrap_user_id?: string;
  } | null;
  factor_migration?: {
    migrated_factors: number;
    pending_enrollments_removed: number;
    recovery_codes_reset: boolean;
  };
  mail_operator_enabled?: boolean;
  mail_runtime_enabled?: boolean;
  runtime?: Runtime;
};
type AppUser = {
  id: string;
  name: string;
  email: string;
  email_verified?: boolean;
  emailVerified?: boolean;
  twoFactorEnabled?: boolean;
  created_at?: string;
  role?: string;
  banned?: boolean;
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
    branches.find((b) => b.id === new URL(window.location.href).searchParams.get('auth_branch'))
      ?.id ||
      branches.find((b) => b.is_default)?.id ||
      branches[0]?.id ||
      '',
  );
  const [instance, setInstance] = useState<Instance | null>(null);
  const [database, setDatabase] = useState('postgres');
  const [origins, setOrigins] = useState('');
  const [requireEmailVerification, setRequireEmailVerification] = useState(false);
  const [emailOTPEnabled, setEmailOTPEnabled] = useState(false);
  const [twoFactorEnabled, setTwoFactorEnabled] = useState(false);
  const [inheritTwoFactor, setInheritTwoFactor] = useState(false);
  const [adminEnabled, setAdminEnabled] = useState(false);
  const [adminBootstrapUserID, setAdminBootstrapUserID] = useState('');
  const [magicLinkEnabled, setMagicLinkEnabled] = useState(false);
  const [magicLinkExpiresIn, setMagicLinkExpiresIn] = useState(300);
  const [magicLinkAllowRegistration, setMagicLinkAllowRegistration] = useState(false);
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
      setRequireEmailVerification(value.spec.require_email_verification ?? false);
      setEmailOTPEnabled(value.spec.email_otp_enabled ?? false);
      setTwoFactorEnabled(value.spec.two_factor_enabled ?? false);
      setInheritTwoFactor(value.spec.inherit_two_factor ?? false);
      setAdminEnabled(value.spec.admin_enabled ?? false);
      // A grant is an explicit command for the next publication, never a
      // standing selection silently copied into a later configuration.
      setAdminBootstrapUserID('');
      setMagicLinkEnabled(value.spec.magic_link_enabled ?? false);
      setMagicLinkExpiresIn(value.spec.magic_link_expires_in || 300);
      setMagicLinkAllowRegistration(value.spec.magic_link_allow_registration ?? false);
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
    setRequireEmailVerification(false);
    setEmailOTPEnabled(false);
    setTwoFactorEnabled(false);
    setInheritTwoFactor(false);
    setAdminEnabled(false);
    setAdminBootstrapUserID('');
    setMagicLinkEnabled(false);
    setMagicLinkExpiresIn(300);
    setMagicLinkAllowRegistration(false);
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
          ...(requireEmailVerification ? { require_email_verification: true } : {}),
          ...(emailOTPEnabled ? { email_otp_enabled: true } : {}),
          ...(twoFactorEnabled ? { two_factor_enabled: true } : {}),
          ...(twoFactorEnabled && inheritTwoFactor ? { inherit_two_factor: true } : {}),
          ...(adminEnabled
            ? {
                admin_enabled: true,
                ...(adminBootstrapUserID.trim()
                  ? { admin_bootstrap_user_id: adminBootstrapUserID.trim() }
                  : {}),
              }
            : {}),
          ...(magicLinkEnabled
            ? {
                magic_link_enabled: true,
                magic_link_expires_in: magicLinkExpiresIn,
                ...(magicLinkAllowRegistration ? { magic_link_allow_registration: true } : {}),
              }
            : {}),
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
      } else if (result?.twoFactorRedirect) {
        setSession(null);
        token.current = '';
        setTokenReady(false);
        setAppNotice('密码已验证；请在下方完成 TOTP 或使用恢复码登录。');
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
        const verificationPending =
          action === 'sign-up/email' && instance?.spec?.require_email_verification && !result.token;
        setSession(verificationPending ? null : result.user || null);
        setAppNotice(
          verificationPending
            ? '应用用户注册成功；请验证邮箱后登录。'
            : action === 'sign-up/email'
              ? '应用用户注册成功'
              : '应用用户登录成功',
        );
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
        <a
          className="button"
          href={`/api/docs?schema=auth&branch_id=${encodeURIComponent(branch)}`}
          target="_blank"
          rel="noreferrer"
        >
          应用 Auth API 文档
        </a>
        <p>
          注册、登录、会话撤销与短期 JWT/JWKS 已提供。本版本需要显式启用实验室
          HTTP；邮箱链接验证与密码找回已有本地验收；OTP/Magic Link 已有本地验收；TOTP
          本阶段独立验证，OAuth 和全链路可信 TLS 仍有门槛。
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
          <label className="auth-mail-policy backend-wide">
            <input
              aria-label="要求应用邮箱验证"
              type="checkbox"
              checked={requireEmailVerification}
              disabled={busy || active || !instance?.mail_operator_enabled}
              onChange={(e) => setRequireEmailVerification(e.target.checked)}
            />
            要求邮箱验证后才能登录（需要已配置的邮件服务）
          </label>
          <label className="auth-mail-policy backend-wide">
            <input
              aria-label="启用应用邮箱验证码"
              type="checkbox"
              checked={emailOTPEnabled}
              disabled={busy || active || !instance?.mail_operator_enabled}
              onChange={(e) => setEmailOTPEnabled(e.target.checked)}
            />
            允许通过邮件验证码登录、注册、验证邮箱和找回密码
          </label>
          <label className="auth-mail-policy backend-wide">
            <input
              aria-label="启用应用 TOTP 双重认证"
              type="checkbox"
              checked={twoFactorEnabled}
              disabled={busy || active}
              onChange={(e) => setTwoFactorEnabled(e.target.checked)}
            />
            允许密码账户绑定 TOTP 验证器与一次性恢复码
          </label>
          {twoFactorEnabled && branches.find((b) => b.id === branch)?.parent_branch_id && (
            <label className="auth-mail-policy backend-wide">
              <input
                aria-label="迁移父分支双重认证"
                type="checkbox"
                checked={inheritTwoFactor}
                disabled={busy || active}
                onChange={(e) => setInheritTwoFactor(e.target.checked)}
              />
              保留从父分支复制的已绑定验证器。用户须重新登录并输入验证码；原恢复码不在此分支生效，登录后需生成新恢复码。尚未确认的绑定须重新开始。
            </label>
          )}
          <label className="auth-mail-policy backend-wide">
            <input
              aria-label="启用应用 Magic Link"
              type="checkbox"
              checked={magicLinkEnabled}
              disabled={busy || active || !instance?.mail_operator_enabled}
              onChange={(e) => setMagicLinkEnabled(e.target.checked)}
            />
            邮箱免密码登录（Magic Link）
          </label>
          <label className="auth-mail-policy backend-wide">
            <input
              aria-label="启用应用管理员接口"
              type="checkbox"
              checked={adminEnabled}
              disabled={busy || active}
              onChange={(e) => setAdminEnabled(e.target.checked)}
            />
            开放应用管理员接口（需要应用管理员会话）
          </label>
          {adminEnabled && (
            <label className="backend-wide">
              本次授予管理员的已有应用用户 ID（可选）
              <input
                aria-label="应用管理员授权用户 ID"
                value={adminBootstrapUserID}
                maxLength={128}
                disabled={busy || active}
                onChange={(e) => setAdminBootstrapUserID(e.target.value)}
              />
              <span className="subtle-note">
                先注册并读取用户
                ID；停用后重新启用时可显式授予。每次发布只执行一次，不会根据邮箱、注册顺序或 Console
                账号自动赋权。
              </span>
            </label>
          )}
          {magicLinkEnabled && (
            <>
              <label>
                登录链接有效分钟数
                <input
                  aria-label="登录链接有效分钟数"
                  type="number"
                  min={5}
                  max={1440}
                  step={1}
                  value={magicLinkExpiresIn / 60}
                  disabled={busy || active}
                  onChange={(e) => setMagicLinkExpiresIn(Number(e.target.value) * 60)}
                />
              </label>
              <label className="auth-mail-policy backend-wide">
                <input
                  aria-label="Magic Link 允许新用户注册"
                  type="checkbox"
                  checked={magicLinkAllowRegistration}
                  disabled={busy || active}
                  onChange={(e) => setMagicLinkAllowRegistration(e.target.checked)}
                />
                允许链接注册新用户
              </label>
            </>
          )}
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
        {instance?.factor_migration && (
          <p role="status" data-testid="auth-factor-migration">
            已迁移 {instance.factor_migration.migrated_factors} 个验证器，已清除{' '}
            {instance.factor_migration.pending_enrollments_removed}{' '}
            个未完成绑定。原恢复码已停用；用户验证登录后须生成此分支的新恢复码。
          </p>
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
            {session.emailVerified && <span> · 邮箱已验证</span>}
          </p>
        )}
        {tokenReady && (
          <p>
            JWT 已就绪（不展示凭据）。可从应用 SDK 使用 <code>/token</code> 获取并调用 Data API。
          </p>
        )}
        {session && active && (
          <AuthSecurityPanel
            key={`${branch}/${session.id}`}
            branch={branch}
            onChanged={() => {
              setSession(null);
              token.current = '';
              setTokenReady(false);
              setAppNotice('应用凭据已更新，请重新登录。');
            }}
          />
        )}
        <div role="status" data-testid="auth-app-result">
          {appNotice}
        </div>
      </section>
      {active && instance?.mail_runtime_enabled && (
        <AuthRecoveryPanel
          key={`${branch}/recovery`}
          branch={branch}
          projectId={projectId}
          emailHint={session?.email}
          onChanged={() => {
            setSession(null);
            token.current = '';
            setTokenReady(false);
          }}
        />
      )}
      {active && instance?.mail_runtime_enabled && instance.spec?.email_otp_enabled && (
        <AuthOTPPanel
          key={`${branch}/otp`}
          branch={branch}
          emailHint={session?.email}
          onUser={(user) => {
            setSession(user);
            token.current = '';
            setTokenReady(false);
            setAppNotice(
              user ? '已通过邮箱验证码登录' : '应用身份状态已更新，请检查会话或重新登录',
            );
          }}
        />
      )}
      {active && instance?.spec?.two_factor_enabled && (
        <AuthTOTPPanel
          key={`${branch}/totp`}
          branch={branch}
          onUser={(user) => {
            setSession(user);
            token.current = '';
            setTokenReady(false);
            setAppNotice(
              user?.twoFactorEnabled ? '应用用户 TOTP 已启用，会话有效。' : '应用身份状态已更新。',
            );
          }}
        />
      )}
      {active && instance?.mail_runtime_enabled && instance.spec?.magic_link_enabled && (
        <AuthMagicLinkPanel
          key={`${branch}/magic-link`}
          branch={branch}
          projectId={projectId}
          emailHint={session?.email}
          onUser={(user) => {
            setSession(user);
            token.current = '';
            setTokenReady(false);
            setAppNotice(user ? '已通过邮箱链接登录' : '应用会话未建立，请检查邮件链接结果');
          }}
        />
      )}
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
              <th>应用角色 / 封禁</th>
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
                <td>
                  {u.role || 'user'}
                  {u.banned ? ' · 已封禁' : ''}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
      {active && instance?.spec?.admin_enabled && (
        <AuthAdminPanel key={branch + '/admin'} branch={branch} />
      )}
    </div>
  );
}
