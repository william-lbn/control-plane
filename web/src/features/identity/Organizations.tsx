import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { api, projectPath } from '../../api';
import type { Organization, Page, Project } from '../../api';
import { Empty, PageHeading } from '../../shared/ui';
import { Invitations } from './Invitations';

type Member = { user_id: string; username: string; role: string };
type Key = {
  id: string;
  name: string;
  project_id: string | null;
  max_role: string;
  expires_at: string;
  revoked_at: string | null;
  token?: string;
};
const roles = ['admin', 'editor', 'viewer', 'collaborator'];
const labels: Record<string, string> = {
  owner: 'Admin · Bootstrap',
  admin: 'Admin',
  editor: 'Editor',
  viewer: 'Viewer',
  collaborator: 'Collaborator',
};

export function Organizations({
  organization,
  onCreated,
  showError,
}: {
  organization: Organization | null;
  onCreated: (id: string) => Promise<void>;
  showError: (error: unknown) => void;
}) {
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [members, setMembers] = useState<Member[]>([]);
  const [username, setUsername] = useState('');
  const [role, setRole] = useState('collaborator');
  const isAdmin = organization?.role === 'admin' || organization?.role === 'owner';
  const orgPath = `/api/v1/organizations/${encodeURIComponent(organization?.id || '')}`;
  async function reloadMembers() {
    setMembers((await api<Page<Member>>(orgPath + '/members')).items);
  }
  useEffect(() => {
    setMembers([]);
    if (isAdmin) void reloadMembers().catch(showError);
  }, [organization?.id, isAdmin]);
  async function create(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    try {
      const org = await api<Organization>('/api/v1/organizations', {
        method: 'POST',
        body: JSON.stringify({ name }),
      });
      setName('');
      await onCreated(org.id);
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  async function add(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    try {
      await api(orgPath + '/members', {
        method: 'POST',
        body: JSON.stringify({ username, password: '', role }),
      });
      setUsername('');
      await reloadMembers();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  async function change(member: Member, next: string) {
    setBusy(true);
    try {
      await api(orgPath + `/members/${encodeURIComponent(member.user_id)}`, {
        method: next === 'remove' ? 'DELETE' : 'PATCH',
        ...(next === 'remove' ? {} : { body: JSON.stringify({ role: next }) }),
      });
      await reloadMembers();
      await onCreated(organization!.id);
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeading
        kicker="WORKSPACE / ORGANIZATION"
        title="组织与访问控制"
        description="组织权限与项目授权按较高权限生效，Collaborator 默认无项目访问。"
      />
      <section className="panel identity-panel">
        <h2>创建组织</h2>
        <form className="identity-form" onSubmit={create}>
          <label>
            组织名称
            <input
              aria-label="组织名称"
              value={name}
              maxLength={80}
              required
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <button className="button primary" disabled={busy}>
            创建组织
          </button>
        </form>
      </section>
      {organization && (
        <section className="panel identity-panel">
          <h2>{organization.name}</h2>
          <p className="muted">
            {organization.id} · {labels[organization.role] || organization.role}
          </p>
          {isAdmin ? (
            <>
              <div className="section-header">
                <h3>组织成员</h3>
                <span>{members.length} 人</span>
                <button
                  className="button"
                  disabled={busy}
                  onClick={() => void reloadMembers().catch(showError)}
                >
                  刷新成员
                </button>
              </div>
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>账号</th>
                      <th>角色</th>
                      <th>操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {members.map((member) => (
                      <tr key={member.user_id}>
                        <td>{member.username}</td>
                        <td>
                          <select
                            aria-label={`组织角色 ${member.username}`}
                            value={member.role === 'owner' ? 'admin' : member.role}
                            disabled={busy}
                            onChange={(e) => void change(member, e.target.value)}
                          >
                            {roles.map((item) => (
                              <option key={item} value={item}>
                                {labels[item]}
                              </option>
                            ))}
                          </select>
                        </td>
                        <td>
                          <button
                            className="text-button"
                            disabled={busy}
                            onClick={() => {
                              if (confirm(`移除 ${member.username} 对本组织的访问？`))
                                void change(member, 'remove');
                            }}
                          >
                            移除成员 {member.username}
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <form className="identity-form" onSubmit={add}>
                <label>
                  成员用户名
                  <input
                    aria-label="成员用户名"
                    value={username}
                    required
                    autoComplete="off"
                    onChange={(e) => setUsername(e.target.value)}
                  />
                </label>
                <label>
                  组织角色
                  <select
                    aria-label="新成员组织角色"
                    value={role}
                    onChange={(e) => setRole(e.target.value)}
                  >
                    {roles.map((item) => (
                      <option key={item} value={item}>
                        {labels[item]}
                      </option>
                    ))}
                  </select>
                </label>
                <button className="button primary" disabled={busy}>
                  添加已有本地账号
                </button>
              </form>
              <p className="muted">
                新用户请使用成员邀请并自行设置密码。组织管理员不能重置共享账号密码。至少保留一位
                Admin。
              </p>
            </>
          ) : (
            <p className="muted">
              成员与组织密钥由 Admin 管理。Viewer 可创建自己的项目；Collaborator 需要项目授权。
            </p>
          )}
        </section>
      )}
      <Invitations
        key={organization?.id || 'no-org'}
        organization={organization}
        onAccepted={onCreated}
        showError={showError}
      />
      {organization && isAdmin && <APIKeys organization={organization} showError={showError} />}
      <APIKeys showError={showError} />
    </>
  );
}

function APIKeys({
  organization,
  showError,
}: {
  organization?: Organization;
  showError: (error: unknown) => void;
}) {
  const path = organization
    ? `/api/v1/organizations/${encodeURIComponent(organization.id)}/api-keys`
    : '/api/v1/api-keys';
  const [keys, setKeys] = useState<Key[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [name, setName] = useState('');
  const [project, setProject] = useState('');
  const [maxRole, setMaxRole] = useState('viewer');
  const [days, setDays] = useState(30);
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  async function reload() {
    setKeys((await api<Page<Key>>(path)).items);
  }
  useEffect(() => {
    setToken('');
    setProjects([]);
    setProject('');
    void reload().catch(showError);
    if (organization)
      void api<Page<Project>>(
        `/api/v1/organizations/${encodeURIComponent(organization.id)}/projects`,
      )
        .then((data) => setProjects(data.items))
        .catch(showError);
  }, [path]);
  async function create(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setToken('');
    try {
      const key = await api<Key>(path, {
        method: 'POST',
        body: JSON.stringify({ name, project_id: project, max_role: maxRole, expires_days: days }),
      });
      setToken(key.token || '');
      setName('');
      await reload();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  async function revoke(key: Key) {
    setBusy(true);
    try {
      await api(`${path}/${encodeURIComponent(key.id)}`, { method: 'DELETE' });
      setToken('');
      await reload();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  const kind = organization ? '组织' : '个人';
  return (
    <section className="panel identity-panel">
      <h2>{kind} API keys</h2>
      <p className="muted">
        服务端仅保存摘要；密钥取权限上限与持有者当前权限的交集。撤销或成员降权后，后续请求立即重新授权。
      </p>
      <form className="identity-form" onSubmit={create}>
        <label>
          名称
          <input
            aria-label={`${kind} API key 名称`}
            value={name}
            required
            maxLength={80}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        {organization && (
          <label>
            项目范围
            <select
              aria-label="API key 项目范围"
              value={project}
              onChange={(e) => setProject(e.target.value)}
            >
              <option value="">整个组织</option>
              {projects.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.name}
                </option>
              ))}
            </select>
          </label>
        )}
        <label>
          权限上限
          <select
            aria-label={`${kind} API key 权限`}
            value={maxRole}
            onChange={(e) => setMaxRole(e.target.value)}
          >
            {['viewer', 'editor', 'admin'].map((item) => (
              <option key={item} value={item}>
                {labels[item]}
              </option>
            ))}
          </select>
        </label>
        <label>
          有效天数
          <input
            aria-label={`${kind} API key 有效天数`}
            type="number"
            min={1}
            max={365}
            value={days}
            required
            onChange={(e) => setDays(Number(e.target.value))}
          />
        </label>
        <button className="button primary" disabled={busy}>
          创建{kind} API key
        </button>
      </form>
      {token && (
        <div className="notice">
          <div>
            <strong>只显示一次，请保存到私有凭据库</strong>
            <label>
              新 API key
              <input aria-label="新 API key（仅显示一次）" type="password" value={token} readOnly />
            </label>
            <button
              className="button"
              onClick={() => void navigator.clipboard.writeText(token).catch(showError)}
            >
              复制密钥
            </button>{' '}
            <button className="text-button" onClick={() => setToken('')}>
              隐藏密钥
            </button>
          </div>
        </div>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>名称</th>
              <th>范围 / 权限</th>
              <th>到期</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {keys.map((key) => (
              <tr key={key.id}>
                <td>{key.name}</td>
                <td>
                  {key.project_id || (organization ? '组织' : '个人访问范围')} ·{' '}
                  {labels[key.max_role]}
                </td>
                <td>{new Date(key.expires_at).toLocaleString()}</td>
                <td>{key.revoked_at ? '已撤销' : '有效'}</td>
                <td>
                  <button
                    className="text-button"
                    disabled={busy || !!key.revoked_at}
                    onClick={() => void revoke(key)}
                  >
                    撤销密钥 {key.name}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

type Permission = Member & {
  organization_role: string;
  project_role: string;
  effective_permission: string;
};
export function ProjectPermissions({
  projectId,
  canAdmin,
  showError,
}: {
  projectId: string;
  canAdmin: boolean;
  showError: (error: unknown) => void;
}) {
  const [items, setItems] = useState<Permission[]>([]);
  const [busy, setBusy] = useState(false);
  const path = projectPath(projectId) + '/permissions';
  async function reload() {
    setItems((await api<Page<Permission>>(path)).items);
  }
  useEffect(() => {
    setItems([]);
    if (canAdmin) void reload().catch(showError);
  }, [projectId, canAdmin]);
  if (!canAdmin)
    return (
      <Empty
        title="需要项目 Admin 权限"
        message="组织角色与项目授权取较高权限，项目 Admin 可以管理授权。"
      />
    );
  async function change(member: Permission, role: string) {
    setBusy(true);
    try {
      await api(`${path}/${encodeURIComponent(member.user_id)}`, {
        method: role === 'inherit' ? 'DELETE' : 'PUT',
        ...(role === 'inherit' ? {} : { body: JSON.stringify({ role }) }),
      });
      await reload();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeading
        kicker="PROJECT / PERMISSIONS"
        title="项目权限"
        description="项目授权只提升访问级别；删除授权后回到组织默认权限。"
      />
      <section className="panel identity-panel">
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>成员</th>
                <th>组织角色</th>
                <th>项目授权</th>
                <th>有效权限</th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={item.user_id}>
                  <td>{item.username}</td>
                  <td>{labels[item.organization_role]}</td>
                  <td>
                    <select
                      aria-label={`项目授权 ${item.username}`}
                      disabled={busy}
                      value={item.project_role || 'inherit'}
                      onChange={(e) => void change(item, e.target.value)}
                    >
                      <option value="inherit">仅继承组织角色</option>
                      {['viewer', 'editor', 'admin'].map((role) => (
                        <option value={role} key={role}>
                          {labels[role]}
                        </option>
                      ))}
                    </select>
                  </td>
                  <td>{labels[item.effective_permission] || '无权限'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </>
  );
}
