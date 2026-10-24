import { useState } from 'react';

type Organization = { id: string; name: string; slug: string };
type Member = {
  id: string;
  userId: string;
  role: string;
  user?: { name?: string; email?: string };
};
type Invitation = {
  id: string;
  email: string;
  role: string;
  status: string;
  organizationId: string;
};
class OrganizationReplyError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

export function AuthOrganizationPanel({ branch }: { branch: string }) {
  const [organizations, setOrganizations] = useState<Organization[]>([]),
    [organization, setOrganization] = useState(''),
    [name, setName] = useState(''),
    [slug, setSlug] = useState(''),
    [members, setMembers] = useState<Member[]>([]),
    [memberOffset, setMemberOffset] = useState(0),
    [memberTotal, setMemberTotal] = useState(0),
    [member, setMember] = useState(''),
    [email, setEmail] = useState(''),
    [role, setRole] = useState('member'),
    [invitations, setInvitations] = useState<Invitation[]>([]),
    [invitation, setInvitation] = useState(''),
    [remove, setRemove] = useState(false),
    [busy, setBusy] = useState(false),
    [uncertain, setUncertain] = useState(false),
    [notice, setNotice] = useState('');
  const base = '/auth/v1/' + encodeURIComponent(branch) + '/organization/';
  async function call(action: string, data?: object, writes = data !== undefined) {
    setBusy(true);
    setNotice('');
    try {
      const response = await fetch(base + action, {
        method: data === undefined ? 'GET' : 'POST',
        credentials: 'same-origin',
        headers: data === undefined ? {} : { 'Content-Type': 'application/json' },
        body: data === undefined ? undefined : JSON.stringify(data),
      });
      const value = await response.json().catch(() => null);
      if (!response.ok)
        throw new OrganizationReplyError(
          value?.message || `组织请求失败（${response.status}）`,
          response.status,
        );
      if (value === null && !['set-active'].includes(action)) throw new Error('组织响应无法确认');
      setNotice('应用组织操作已完成。');
      return { value };
    } catch (error) {
      if (writes && (!(error instanceof OrganizationReplyError) || error.status >= 500))
        setUncertain(true);
      setNotice(error instanceof Error ? error.message : '请求结果未知');
    } finally {
      setBusy(false);
    }
  }
  async function list() {
    const result = await call('list');
    if (result) setOrganizations(result.value);
  }
  async function readMembers(offset = memberOffset) {
    const result = await call(
      'list-members?organizationId=' +
        encodeURIComponent(organization) +
        '&limit=100&offset=' +
        offset,
    );
    if (result) {
      setMembers(result.value.members);
      setMemberOffset(offset);
      setMemberTotal(result.value.total);
      setMember('');
      setNotice(`已读取 ${result.value.members.length} 位成员；总计 ${result.value.total} 位。`);
    }
  }
  async function act(action: string, data: object) {
    const result = await call(action, data);
    if (!result) return;
    if (action === 'create') {
      setOrganization(result.value.id);
      await list();
    }
    if (['update', 'delete', 'leave'].includes(action)) await list();
    if (['delete', 'leave'].includes(action)) {
      setOrganization('');
      setMembers([]);
      setInvitations([]);
      setRemove(false);
    }
    if (['remove-member', 'update-member-role'].includes(action)) {
      setMember('');
      await readMembers();
    }
    if (action === 'invite-member') {
      setInvitation(result.value.id);
      setNotice(`邀请已创建：${result.value.id}；收件人需验证邮箱后接受。`);
    }
    if (['accept-invitation', 'reject-invitation', 'cancel-invitation'].includes(action)) {
      setInvitations([]);
      await list();
    }
  }
  const disabled = busy || uncertain;
  return (
    <section className="table-card backend-panel" aria-label="应用组织" style={{ padding: 24 }}>
      <h3>应用组织</h3>
      <p>
        使用上方当前分支的应用会话。组织 Owner、Admin 和 Member
        的权限由成员关系决定。接受或拒绝邀请前须验证收件邮箱；Console
        和应用全局管理员身份不会自动授予组织权限。
      </p>
      <button className="button" disabled={busy} onClick={() => void list()}>
        读取我的应用组织
      </button>
      <div className="backend-form">
        <label>
          应用组织
          <select
            aria-label="当前应用组织"
            value={organization}
            disabled={busy}
            onChange={(e) => {
              setOrganization(e.target.value);
              setMembers([]);
              setMemberOffset(0);
              setMemberTotal(0);
              setMember('');
              setInvitations([]);
              setRemove(false);
            }}
          >
            <option value="">请选择组织</option>
            {organizations.map((o) => (
              <option key={o.id} value={o.id}>
                {o.name} · {o.slug}
              </option>
            ))}
          </select>
        </label>
        <label>
          组织名称
          <input
            aria-label="应用组织名称"
            value={name}
            maxLength={128}
            disabled={disabled}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <label>
          组织标识
          <input
            aria-label="应用组织标识"
            value={slug}
            maxLength={128}
            disabled={disabled}
            onChange={(e) => setSlug(e.target.value)}
          />
        </label>
      </div>
      <div className="button-row">
        <button
          className="button"
          disabled={disabled || !name || !slug}
          onClick={() => void act('create', { name, slug })}
        >
          创建应用组织
        </button>
        <button
          className="button"
          disabled={busy || !slug}
          onClick={async () => {
            const result = await call('check-slug', { slug }, false);
            if (result) setNotice(result.value.status ? '组织标识可用' : '组织标识已被使用');
          }}
        >
          检查组织标识
        </button>
        <button
          className="button"
          disabled={disabled || !organization || !name}
          onClick={() => void act('update', { organizationId: organization, data: { name } })}
        >
          更新组织名称
        </button>
        <button
          className="button"
          disabled={disabled || !organization}
          onClick={() => void act('set-active', { organizationId: organization })}
        >
          设为当前组织
        </button>
        <button
          className="button"
          disabled={disabled}
          onClick={() => void act('set-active', { organizationId: null })}
        >
          清除当前组织
        </button>
        <button
          className="button"
          disabled={busy || !organization}
          onClick={() => void readMembers()}
        >
          读取组织成员
        </button>
        <button
          className="button"
          disabled={busy || !organization || memberOffset === 0}
          onClick={() => void readMembers(Math.max(0, memberOffset - 100))}
        >
          上一页组织成员
        </button>
        <button
          className="button"
          disabled={busy || !organization || memberOffset + members.length >= memberTotal}
          onClick={() => void readMembers(memberOffset + 100)}
        >
          下一页组织成员
        </button>
        <button
          className="button"
          disabled={busy || !organization}
          onClick={async () => {
            const result = await call(
              'has-permission',
              { organizationId: organization, permissions: { organization: ['delete'] } },
              false,
            );
            if (result)
              setNotice(result.value.success ? '当前成员可删除此组织' : '当前成员不能删除此组织');
          }}
        >
          检查组织删除权限
        </button>
      </div>
      <div className="backend-form">
        <label>
          组织成员
          <select
            aria-label="目标组织成员"
            value={member}
            disabled={busy}
            onChange={(e) => setMember(e.target.value)}
          >
            <option value="">请选择成员</option>
            {members.map((m) => (
              <option key={m.id} value={m.id}>
                {m.user?.email || m.userId} · {m.role}
              </option>
            ))}
          </select>
        </label>
        <label>
          组织角色
          <select
            aria-label="目标组织角色"
            value={role}
            disabled={disabled}
            onChange={(e) => setRole(e.target.value)}
          >
            <option value="member">Member</option>
            <option value="admin">Admin</option>
            <option value="owner">Owner</option>
          </select>
        </label>
        <label>
          邀请邮箱
          <input
            aria-label="组织邀请邮箱"
            type="email"
            value={email}
            disabled={disabled}
            maxLength={254}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
      </div>
      <div className="button-row">
        <button
          className="button"
          disabled={disabled || !organization || !member}
          onClick={() =>
            void act('update-member-role', { organizationId: organization, memberId: member, role })
          }
        >
          设置组织角色
        </button>
        <button
          className="button"
          disabled={disabled || !organization || !member}
          onClick={() =>
            void act('remove-member', { organizationId: organization, memberIdOrEmail: member })
          }
        >
          移除组织成员
        </button>
        <button
          className="button"
          disabled={disabled || !organization || !email}
          onClick={() => void act('invite-member', { organizationId: organization, email, role })}
        >
          邀请应用组织成员
        </button>
        <button
          className="button"
          disabled={busy || !organization}
          onClick={async () => {
            const result = await call(
              'list-invitations?organizationId=' + encodeURIComponent(organization),
            );
            if (result) setInvitations(result.value);
          }}
        >
          读取组织邀请
        </button>
        <button
          className="button"
          disabled={busy}
          onClick={async () => {
            const result = await call('list-user-invitations');
            if (result) setInvitations(result.value);
          }}
        >
          读取我收到的邀请
        </button>
      </div>
      <table aria-label="应用组织邀请">
        <thead>
          <tr>
            <th>邀请 ID</th>
            <th>邮箱</th>
            <th>角色</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          {invitations.map((i) => (
            <tr key={i.id}>
              <td>
                <button className="button" onClick={() => setInvitation(i.id)}>
                  {i.id}
                </button>
              </td>
              <td>{i.email}</td>
              <td>{i.role}</td>
              <td>{i.status}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <label>
        邀请 ID
        <input
          aria-label="应用组织邀请 ID"
          value={invitation}
          maxLength={256}
          disabled={busy}
          onChange={(e) => setInvitation(e.target.value)}
        />
      </label>
      <div className="button-row">
        <button
          className="button"
          disabled={busy || !invitation}
          onClick={async () => {
            const result = await call('get-invitation?id=' + encodeURIComponent(invitation));
            if (result)
              setNotice(
                `邀请邮箱 ${result.value.email}，角色 ${result.value.role}，状态 ${result.value.status}`,
              );
          }}
        >
          读取所选邀请
        </button>
        <button
          className="button"
          disabled={disabled || !invitation}
          onClick={() => void act('accept-invitation', { invitationId: invitation })}
        >
          接受组织邀请
        </button>
        <button
          className="button"
          disabled={disabled || !invitation}
          onClick={() => void act('reject-invitation', { invitationId: invitation })}
        >
          拒绝组织邀请
        </button>
        <button
          className="button"
          disabled={disabled || !invitation}
          onClick={() => void act('cancel-invitation', { invitationId: invitation })}
        >
          撤销组织邀请
        </button>
      </div>
      <label>
        <input
          type="checkbox"
          aria-label="确认退出或删除应用组织"
          checked={remove}
          disabled={disabled}
          onChange={(e) => setRemove(e.target.checked)}
        />
        确认退出或永久删除当前应用组织
      </label>
      <div className="button-row">
        <button
          className="button danger"
          disabled={disabled || !organization || !remove}
          onClick={() => void act('leave', { organizationId: organization })}
        >
          退出应用组织
        </button>
        <button
          className="button danger"
          disabled={disabled || !organization || !remove}
          onClick={() => void act('delete', { organizationId: organization })}
        >
          删除应用组织
        </button>
      </div>
      {notice && (
        <p role="status" data-testid="auth-organization-result">
          {notice}
        </p>
      )}
      {uncertain && (
        <p role="alert">
          上次写入结果未确认，已暂停后续写入。请读取组织、成员与邀请核对状态；本页不会自动重试写入。
        </p>
      )}
    </section>
  );
}
