import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { api, projectPath } from '../../api';
import type { Branch, Operation, Page } from '../../api';
import { Empty, PageHeading } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';

type CatalogItem = {
  name: string;
  owner?: string;
  size_bytes?: number;
  managed: boolean;
  protected: boolean;
  state: string;
  superuser?: boolean;
};

export function Catalog({
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
  const [roles, setRoles] = useState<CatalogItem[]>([]);
  const [databases, setDatabases] = useState<CatalogItem[]>([]);
  const [roleName, setRoleName] = useState('');
  const [password, setPassword] = useState('');
  const [databaseName, setDatabaseName] = useState('');
  const [owner, setOwner] = useState('');
  const [rotateName, setRotateName] = useState('');
  const [rotatePassword, setRotatePassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [operation, setOperation] = useState<Operation | null>(null);
  const path = `${projectPath(projectId)}/branches/${encodeURIComponent(branch)}`;
  async function load() {
    const [r, d] = await Promise.all([
      api<Page<CatalogItem>>(path + '/roles'),
      api<Page<CatalogItem>>(path + '/databases'),
    ]);
    setRoles(r.items);
    setDatabases(d.items);
    setLoading(false);
  }
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setRoles([]);
    setDatabases([]);
    setOperation(null);
    setPassword('');
    setRotatePassword('');
    setOwner('');
    setRotateName('');
    if (canEdit && branch)
      Promise.all([
        api<Page<CatalogItem>>(path + '/roles'),
        api<Page<CatalogItem>>(path + '/databases'),
      ])
        .then(([r, d]) => {
          if (!cancelled) {
            setRoles(r.items);
            setDatabases(d.items);
            setLoading(false);
          }
        })
        .catch((e) => {
          if (!cancelled) {
            setLoading(false);
            showError(e);
          }
        });
    return () => {
      cancelled = true;
    };
  }, [projectId, branch, canEdit]);
  async function mutate(kind: string, method: string, body?: Record<string, string>) {
    setBusy(true);
    setOperation(null);
    try {
      const result = await api<{ operation: Operation }>(path + '/' + kind, {
        method,
        body: body ? JSON.stringify(body) : undefined,
        headers: { 'Idempotency-Key': newRequestKey() },
      });
      setPassword('');
      setRotatePassword('');
      setOperation(result.operation);
      const deadline = Date.now() + 490000;
      let current = result.operation;
      while (current.state !== 'succeeded' && current.state !== 'failed') {
        if (Date.now() > deadline) throw new Error('操作仍在执行，请在操作记录中查看结果。');
        await new Promise((resolve) => setTimeout(resolve, 2000));
        current = await api<Operation>(projectPath(projectId) + '/operations/' + current.id);
        setOperation(current);
      }
      if (current.state === 'failed')
        throw new Error(`${current.error_message || '调谐失败'} · ${current.id}`);
      await load();
      setRoleName('');
      setDatabaseName('');
    } catch (e) {
      showError(e);
    } finally {
      setBusy(false);
    }
  }
  if (!canEdit)
    return (
      <Empty
        title="需要 Editor 或 Admin 权限"
        message="数据库目录读取及管理会使用分支的读写 Compute。"
      />
    );
  const editableRoles = roles.filter((r) => r.managed && !r.protected && r.state === 'ready');
  return (
    <>
      <PageHeading
        kicker="PROJECT / DATABASES & ROLES"
        title="数据库与角色"
        description="角色和数据库属于分支。管理操作通过内部身份和 Neon Proxy 执行，并同步所有 Endpoint 的冷启动配置。"
        action={
          <button
            className="button"
            disabled={busy || loading}
            onClick={() => void load().catch(showError)}
          >
            刷新目录
          </button>
        }
      />
      <div className="selector-row">
        <label>
          管理分支
          <select
            aria-label="管理分支"
            value={branch}
            disabled={busy}
            onChange={(e) => setBranch(e.target.value)}
          >
            {branches
              .filter((b) => b.state === 'ready')
              .map((b) => (
                <option key={b.id} value={b.id}>
                  {b.name}
                </option>
              ))}
          </select>
        </label>
        <span>{loading ? '正在读取目录，休眠 Compute 会自动唤醒…' : '通过读写 Endpoint 管理'}</span>
      </div>
      {operation && (
        <div className="panel catalog-operation" role="status">
          <strong>操作状态：{operation.state}</strong>
          <small>{operation.id}</small>
          <div>
            {operation.steps.map((s) => (
              <span className="tag" key={s.ordinal}>
                {s.name} · {s.state}
              </span>
            ))}
          </div>
        </div>
      )}
      <div className="identity-grid">
        <form
          className="panel"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            void mutate('roles', 'POST', { name: roleName, password });
          }}
        >
          <div className="panel-heading">
            <h2>创建数据库角色</h2>
            <span>Console / API 角色</span>
          </div>
          <p className="subtle-note">
            角色为 NOSUPERUSER，并加入 neon_superuser；具备创建数据库、角色和 BYPASSRLS
            权限。面向应用的最小权限角色可使用 SQL 创建。
          </p>
          <label>
            新角色名称
            <input
              required
              maxLength={63}
              value={roleName}
              onChange={(e) => setRoleName(e.target.value)}
            />
          </label>
          <label>
            新角色密码
            <input
              required
              type="password"
              minLength={12}
              maxLength={256}
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          <button
            className="button primary"
            disabled={busy || loading || !roleName || password.length < 12}
          >
            创建角色
          </button>
        </form>
        <form
          className="panel"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            void mutate('databases', 'POST', { name: databaseName, owner });
          }}
        >
          <div className="panel-heading">
            <h2>创建数据库</h2>
            <span>独立 Owner</span>
          </div>
          <label>
            新数据库名称
            <input
              required
              maxLength={63}
              value={databaseName}
              onChange={(e) => setDatabaseName(e.target.value)}
            />
          </label>
          <label>
            数据库 Owner
            <select
              aria-label="数据库 Owner"
              required
              value={owner}
              onChange={(e) => setOwner(e.target.value)}
            >
              <option value="">选择角色</option>
              {editableRoles.map((r) => (
                <option key={r.name} value={r.name}>
                  {r.name}
                </option>
              ))}
            </select>
          </label>
          <button className="button primary" disabled={busy || loading || !databaseName || !owner}>
            创建数据库
          </button>
        </form>
      </div>
      <div className="table-card">
        <table>
          <thead>
            <tr>
              <th>数据库</th>
              <th>Owner</th>
              <th>大小</th>
              <th>状态</th>
              <th>管理</th>
            </tr>
          </thead>
          <tbody>
            {databases.map((d) => (
              <tr key={d.name}>
                <td>
                  <strong>{d.name}</strong>
                </td>
                <td>{d.owner || '—'}</td>
                <td>
                  {d.size_bytes === undefined
                    ? '—'
                    : `${(d.size_bytes / 1024 / 1024).toFixed(1)} MiB`}
                </td>
                <td>{d.state}</td>
                <td>
                  <button
                    className="text-button"
                    aria-label={`删除数据库 ${d.name}`}
                    disabled={busy || !d.managed || d.protected || d.state !== 'ready'}
                    onClick={() => {
                      if (
                        confirm(
                          `删除数据库 ${d.name}？其中的数据将删除；存在活动连接时操作会拒绝执行。`,
                        )
                      )
                        void mutate('databases/' + encodeURIComponent(d.name), 'DELETE');
                    }}
                  >
                    删除
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="table-card">
        <table>
          <thead>
            <tr>
              <th>角色</th>
              <th>身份</th>
              <th>状态</th>
              <th>管理</th>
            </tr>
          </thead>
          <tbody>
            {roles.map((r) => (
              <tr key={r.name}>
                <td>
                  <strong>{r.name}</strong>
                </td>
                <td>
                  {r.protected
                    ? '内部 / 保留角色'
                    : r.managed
                      ? 'Console / API 角色'
                      : 'SQL 创建的角色'}
                  {r.superuser && ' · PostgreSQL SUPERUSER'}
                </td>
                <td>{r.state}</td>
                <td>
                  <button
                    className="text-button"
                    aria-label={`删除角色 ${r.name}`}
                    disabled={busy || !r.managed || r.protected || r.state !== 'ready'}
                    onClick={() => {
                      if (confirm(`删除角色 ${r.name}？存在对象所有权或授权依赖时会拒绝执行。`))
                        void mutate('roles/' + encodeURIComponent(r.name), 'DELETE');
                    }}
                  >
                    删除
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          void mutate('roles/' + encodeURIComponent(rotateName), 'PATCH', {
            password: rotatePassword,
          });
        }}
      >
        <div className="panel-heading">
          <h2>轮换角色密码</h2>
          <span>同步 Writer、Reader 与冷启动配置</span>
        </div>
        <label>
          密码轮换角色
          <select
            aria-label="密码轮换角色"
            required
            value={rotateName}
            onChange={(e) => setRotateName(e.target.value)}
          >
            <option value="">选择角色</option>
            {editableRoles.map((r) => (
              <option key={r.name} value={r.name}>
                {r.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          轮换后密码
          <input
            type="password"
            required
            minLength={12}
            maxLength={256}
            autoComplete="new-password"
            value={rotatePassword}
            onChange={(e) => setRotatePassword(e.target.value)}
          />
        </label>
        <button
          className="button primary"
          disabled={busy || loading || !rotateName || rotatePassword.length < 12}
        >
          轮换密码
        </button>
      </form>
    </>
  );
}
