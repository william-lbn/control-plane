import React, { useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { api, ApiError, route } from './api';
import type { Capabilities, User, Organization, Page } from './api';
import { short } from './shared/ui';
import { Projects, ProjectWorkspace } from './features/projects/Projects';
import { Organizations } from './features/identity/Organizations';
import './styles.css';

function useHash() {
  const [hash, setHash] = useState(location.hash || '#/projects');
  useEffect(() => {
    const onChange = () => setHash(location.hash || '#/projects');
    addEventListener('hashchange', onChange);
    return () => removeEventListener('hashchange', onChange);
  }, []);
  return hash;
}

function App() {
  const hash = useHash();
  const [user, setUser] = useState<User | null | undefined>(undefined);
  const [capabilities, setCapabilities] = useState<Capabilities | null>(null);
  const [organizations, setOrganizations] = useState<Organization[]>([]);
  const [organizationId, setOrganizationId] = useState('');
  const [orgPicker, setOrgPicker] = useState(false);
  const [organizationsLoaded, setOrganizationsLoaded] = useState(false);
  const [toast, setToast] = useState<string>('');
  const parts = hash.replace(/^#\//, '').split('/');
  const projectId = parts[0] === 'projects' ? parts[1] : undefined;
  useEffect(() => setToast(''), [hash]);
  useEffect(() => {
    api<{ user: User }>('/api/v1/session')
      .then((data) => setUser(data.user))
      .catch(() => setUser(null));
  }, []);
  useEffect(() => {
    if (user)
      api<Capabilities>('/api/v1/capabilities')
        .then(setCapabilities)
        .catch(() => setCapabilities(null));
  }, [user]);
  useEffect(() => {
    let cancelled = false;
    setOrganizations([]);
    setOrganizationId('');
    setOrganizationsLoaded(false);
    if (user)
      api<Page<Organization>>('/api/v1/organizations')
        .then((data) => {
          if (cancelled) return;
          const saved = sessionStorage.getItem(`neon_v2_org_${user.id}`);
          setOrganizations(data.items);
          setOrganizationId(
            data.items.find((item) => item.id === saved)?.id || data.items[0]?.id || '',
          );
          setOrganizationsLoaded(true);
        })
        .catch(() => {
          if (!cancelled) setOrganizationsLoaded(true);
        });
    return () => {
      cancelled = true;
    };
  }, [user?.id]);
  // Deep links must display the project's organization, even when the last
  // selected workspace belongs to another tenant. Authorization stays server-side.
  useEffect(() => {
    let cancelled = false;
    if (user && projectId && organizationsLoaded)
      api<{ organization_id: string }>(`/api/v1/projects/${encodeURIComponent(projectId)}`)
        .then((project) => {
          if (cancelled || !organizations.some((item) => item.id === project.organization_id))
            return;
          setOrganizationId(project.organization_id);
          sessionStorage.setItem(`neon_v2_org_${user.id}`, project.organization_id);
        })
        .catch(() => {}); // ProjectWorkspace renders the authorized resource error.
    return () => {
      cancelled = true;
    };
  }, [user?.id, projectId, organizationsLoaded, organizations]);
  useEffect(() => {
    if (!toast) return;
    const id = setTimeout(() => setToast(''), 6000);
    return () => clearTimeout(id);
  }, [toast]);
  if (user === undefined) return <div className="boot">正在连接控制面…</div>;
  if (!user) return <Login onLogin={setUser} />;
  const page = parts[2] || 'overview';
  const branchId = page === 'branches' ? parts[3] : undefined;
  const organization = organizations.find((item) => item.id === organizationId) || null;
  async function refreshOrganizations(id: string) {
    const data = await api<Page<Organization>>('/api/v1/organizations');
    setOrganizations(data.items);
    const selected = data.items.find((item) => item.id === id)?.id || data.items[0]?.id || '';
    setOrganizationId(selected);
    sessionStorage.setItem(`neon_v2_org_${user!.id}`, selected);
  }
  const showError = (error: unknown) =>
    setToast(
      error instanceof ApiError
        ? `${error.message} · ${error.code}${error.requestId ? ' · ' + error.requestId : ''}`
        : String(error),
    );
  return (
    <div className="app-shell">
      <aside className="sidebar">
        <a className="brand" href="#/projects">
          <span className="brand-mark">✦</span>
          <span>
            neon<span className="brand-dot">.</span>
          </span>
        </a>
        <button
          className="workspace workspace-picker"
          aria-label="切换组织"
          aria-haspopup="listbox"
          aria-expanded={orgPicker}
          onClick={() => setOrgPicker(!orgPicker)}
        >
          <div className="workspace-icon">N</div>
          <div>
            <strong>{organization?.name || '选择组织'}</strong>
            <small>CONTROL PLANE V2</small>
          </div>
          <span className="chevron">⌄</span>
        </button>
        {orgPicker && (
          <div className="organization-picker" role="listbox" aria-label="可访问组织">
            {organizations.map((item) => (
              <button
                key={item.id}
                role="option"
                aria-selected={item.id === organizationId}
                onClick={() => {
                  setOrganizationId(item.id);
                  sessionStorage.setItem(`neon_v2_org_${user.id}`, item.id);
                  setOrgPicker(false);
                  location.hash = '#/projects';
                }}
              >
                {item.name} · {item.role}
              </button>
            ))}
          </div>
        )}
        <div className="nav-label">工作空间</div>
        <nav className="nav">
          <a className={!projectId && parts[0] === 'projects' ? 'active' : ''} href="#/projects">
            <span>▦</span>项目列表
          </a>
          <a className={parts[0] === 'organization' ? 'active' : ''} href="#/organization">
            <span>◉</span>组织与安全
          </a>
          {projectId && (
            <>
              <div className="nav-label inset">当前项目</div>
              <a className={page === 'overview' ? 'active' : ''} href={route(projectId)}>
                <span>◈</span>总览
              </a>
              <a
                className={page === 'branches' ? 'active' : ''}
                href={route(projectId, 'branches')}
              >
                <span>⑂</span>分支
              </a>
              <a className={page === 'compute' ? 'active' : ''} href={route(projectId, 'compute')}>
                <span>▣</span>Compute
              </a>
              <a
                className={page === 'monitoring' ? 'active' : ''}
                href={route(projectId, 'monitoring')}
              >
                <span>◫</span>监控
              </a>
              <a className={page === 'connect' ? 'active' : ''} href={route(projectId, 'connect')}>
                <span>⌁</span>连接
              </a>
              <a className={page === 'query' ? 'active' : ''} href={route(projectId, 'query')}>
                <span>⌘</span>SQL 工作台
              </a>
              <a
                className={page === 'databases' ? 'active' : ''}
                href={route(projectId, 'databases')}
              >
                <span>▤</span>数据库与角色
              </a>
              <a
                className={page === 'operations' ? 'active' : ''}
                href={route(projectId, 'operations')}
              >
                <span>◷</span>操作记录
              </a>
              <a
                className={page === 'data-api' ? 'active' : ''}
                href={route(projectId, 'data-api')}
              >
                <span>⇄</span>Data API
              </a>
              <a
                className={page === 'permissions' ? 'active' : ''}
                href={route(projectId, 'permissions')}
              >
                <span>♙</span>项目权限
              </a>
              <a
                className={page === 'credentials' ? 'active' : ''}
                href={route(projectId, 'credentials')}
              >
                <span>⚿</span>应用凭据
              </a>
            </>
          )}
        </nav>
        <div className="sidebar-bottom">
          <div className="cluster-dot" /> rke2-lab <span>· PG 16</span>
        </div>
      </aside>
      <div className="main-wrap">
        <header className="topbar">
          <div className="crumb">
            <a href="#/projects">Projects</a>
            {projectId && (
              <>
                {' '}
                <span>/</span> <strong>{short(projectId)}</strong>
              </>
            )}
          </div>
          <div className="top-actions">
            <span className="environment">● LAB ENVIRONMENT</span>
            <div className="avatar">{user.username.slice(0, 1).toUpperCase()}</div>
            <button
              className="text-button"
              onClick={async () => {
                try {
                  await api('/auth/logout', { method: 'POST' });
                  setUser(null);
                  location.hash = '#/projects';
                } catch (e) {
                  showError(e);
                }
              }}
            >
              退出
            </button>
          </div>
        </header>
        <main className="content">
          {!organizationsLoaded ? (
            <div className="skeleton" />
          ) : parts[0] === 'organization' || !organizations.length ? (
            <Organizations
              organization={organization}
              onCreated={refreshOrganizations}
              showError={showError}
            />
          ) : !projectId ? (
            <Projects
              key={organizationId}
              organizationId={organizationId}
              canCreate={organization?.role !== 'collaborator'}
              capabilities={capabilities}
              showError={showError}
            />
          ) : (
            <ProjectWorkspace
              key={projectId}
              projectId={projectId}
              page={page}
              branchId={branchId}
              capabilities={capabilities}
              showError={showError}
            />
          )}
        </main>
      </div>
      {toast && (
        <div role="alert" className="toast">
          {toast}
        </div>
      )}
    </div>
  );
}

function Login({ onLogin }: { onLogin: (user: User) => void }) {
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const result = await api<{ user: User }>('/auth/login', {
        method: 'POST',
        body: JSON.stringify({ username, password }),
      });
      setPassword('');
      onLogin(result.user);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="login-screen">
      <div className="login-glow" />
      <div className="login-layout">
        <div className="login-intro">
          <div className="brand large">
            <span className="brand-mark">✦</span>
            <span>
              neon<span className="brand-dot">.</span>
            </span>
          </div>
          <div className="eyebrow">SELF-HOSTED CONTROL PLANE</div>
          <h1>
            数据库基础设施，
            <br />
            <em>尽在掌握。</em>
          </h1>
          <p>分支化 Postgres、弹性 Compute 与可追踪的操作流程，在一个工作空间里运行。</p>
          <div className="intro-meta">
            <span>◇ 三节点 RKE2</span>
            <span>◇ Neon Proxy</span>
            <span>◇ PostgreSQL 元数据</span>
          </div>
        </div>
        <form className="login-card" onSubmit={submit}>
          <span className="eyebrow">欢迎回来</span>
          <h2>登录控制台</h2>
          <p>使用当前实验环境的管理员凭据。</p>
          <label>
            用户名
            <input
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          </label>
          <label>
            密码
            <input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </label>
          {error && (
            <div role="alert" className="form-error">
              {error}
            </div>
          )}
          <button className="button primary full" disabled={busy}>
            {busy ? '正在登录…' : '登录控制台 →'}
          </button>
          <small>当前为受控实验环境。生产版身份与权限仍在实施中。</small>
        </form>
      </div>
    </div>
  );
}

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
