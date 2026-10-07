import { useEffect, useState } from 'react';
import { api, route, projectPath } from '../../api';
import type {
  Branch,
  BranchService,
  Capabilities,
  Endpoint,
  Operation,
  Page,
  Project,
} from '../../api';
import { fmt, short, stateLabel, status, PageHeading, Empty } from '../../shared/ui';
import { Compute } from '../compute/Compute';
import { Monitoring } from '../monitoring/Monitoring';
import { Connections } from '../connect/Connections';
import { DataAPI } from '../data-api/DataAPI';
import { ManagedAuth } from '../auth/ManagedAuth';
import { BackendCredentials } from '../credentials/BackendCredentials';
import { Workbench } from '../sql/Workbench';
import { Operations } from '../operations/Operations';
import { CreateResource } from './CreateResources';
import { ProjectPermissions } from '../identity/Organizations';
import { Catalog } from '../catalog/Catalog';
import { Restore } from '../restore/Restore';
import { Lifecycle } from './Lifecycle';

export function Projects({
  organizationId,
  canCreate,
  capabilities,
  showError,
}: {
  organizationId: string;
  canCreate: boolean;
  capabilities: Capabilities | null;
  showError: (error: unknown) => void;
}) {
  const [items, setItems] = useState<Project[] | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [trash, setTrash] = useState(false);
  useEffect(() => {
    api<Page<Project>>(
      `/api/v1/organizations/${encodeURIComponent(organizationId)}/projects?deleted=${trash}`,
    )
      .then((data) => setItems(data.items))
      .catch(showError);
  }, [organizationId, showError, trash]);
  return (
    <>
      <PageHeading
        kicker="WORKSPACE / PROJECTS"
        title="项目"
        description="管理隔离的 Postgres 项目，进入分支、Compute 和监控。"
        action={
          <button
            className="button primary"
            disabled={!canCreate || !capabilities?.features.project_create?.enabled}
            title={capabilities?.features.project_create?.reason || '能力检查中'}
            onClick={() => setShowCreate(true)}
          >
            ＋ 创建项目
          </button>
        }
      />
      {showCreate && (
        <CreateResource
          kind="project"
          organizationId={organizationId}
          onClose={() => setShowCreate(false)}
          onDone={(id) => {
            setShowCreate(false);
            location.hash = route(id);
          }}
        />
      )}
      <div className="notice">
        <span>◉</span>
        <div>
          <strong>
            {capabilities?.features.project_create?.enabled
              ? 'Go 原生创建已开放'
              : '创建能力未开放'}
          </strong>
          <p>
            项目创建由 Go Worker 完成，操作步骤持久记录。当前单副本实验环境仍需按生产 Gate
            验证高可用、权限与备份。
          </p>
        </div>
      </div>
      <div className="section-header">
        <h2>{trash ? '已删除项目' : '全部项目'}</h2>
        <button className="button" onClick={() => setTrash(!trash)}>
          {trash ? '查看活动项目' : '查看已删除项目'}
        </button>
        <span>{items?.length ?? '—'} 个项目</span>
      </div>
      {items === null ? (
        <div className="skeleton" />
      ) : items.length === 0 ? (
        <Empty title="尚无项目" message="项目创建功能完成验收后，可在此开始。" />
      ) : (
        <div className="project-grid">
          {items.map((project) => (
            <a
              className="project-card"
              href={route(project.id, project.state === 'ready' ? '' : 'lifecycle')}
              key={project.id}
            >
              <div className="card-top">
                <div className="project-icon">◈</div>
                {status(project.state)}
              </div>
              <h3>{project.name}</h3>
              <p>{project.id}</p>
              <div className="card-footer">
                <span>
                  {project.region_id} · PostgreSQL {project.postgres_version}
                </span>
                <span>打开项目 ↗</span>
              </div>
            </a>
          ))}
        </div>
      )}
    </>
  );
}

export function ProjectWorkspace({
  projectId,
  page,
  branchId,
  capabilities,
  showError,
}: {
  projectId: string;
  page: string;
  branchId?: string;
  capabilities: Capabilities | null;
  showError: (error: unknown) => void;
}) {
  const [project, setProject] = useState<Project | null>(null);
  const [branches, setBranches] = useState<Branch[]>([]);
  const [endpoints, setEndpoints] = useState<Endpoint[]>([]);
  const [operations, setOperations] = useState<Operation[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreateBranch, setShowCreateBranch] = useState(false);
  const refresh = async () => {
    if (page === 'lifecycle') return;
    try {
      const [p, b, e, o] = await Promise.all([
        api<Project>(projectPath(projectId)),
        api<Page<Branch>>(projectPath(projectId) + '/branches'),
        api<Page<Endpoint>>(projectPath(projectId) + '/endpoints'),
        api<Page<Operation>>(projectPath(projectId) + '/operations'),
      ]);
      setProject(p);
      setBranches(b.items);
      setEndpoints(e.items);
      setOperations(o.items);
    } catch (e) {
      showError(e);
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), 30000);
    return () => clearInterval(timer);
  }, [projectId, page]);
  if (page === 'lifecycle') return <Lifecycle projectId={projectId} />;
  if (loading) return <div className="skeleton" />;
  if (!project) return <Empty title="项目不可用" message="请检查 API、权限或元数据库状态。" />;
  const branchName = (id: string) => branches.find((branch) => branch.id === id)?.name || short(id);
  const canEdit =
    project.effective_permission === 'admin' || project.effective_permission === 'editor';
  const canAdmin = project.effective_permission === 'admin';
  if (page === 'auth')
    return (
      <ManagedAuth
        projectId={projectId}
        branches={branches}
        canAdmin={canAdmin}
        showError={showError}
      />
    );
  if (page === 'data-api')
    return (
      <DataAPI projectId={projectId} branches={branches} canEdit={canEdit} showError={showError} />
    );
  if (page === 'credentials')
    return (
      <BackendCredentials
        projectId={projectId}
        branches={branches}
        canEdit={canEdit}
        showError={showError}
      />
    );
  if (page === 'databases')
    return (
      <Catalog projectId={projectId} branches={branches} canEdit={canEdit} showError={showError} />
    );
  if (page === 'permissions')
    return <ProjectPermissions projectId={projectId} canAdmin={canAdmin} showError={showError} />;
  if ((page === 'query' || page === 'connect') && !canEdit)
    return (
      <Empty
        title="需要 Editor 或 Admin 权限"
        message="Viewer 可以查看项目元数据，但不能获取连接串或执行 SQL。"
      />
    );
  if (page === 'restore')
    return (
      <Restore
        projectId={projectId}
        branches={branches}
        canEdit={canEdit}
        enabled={
          project.source === 'managed' &&
          project.state === 'ready' &&
          !!capabilities?.features.pitr_new_branch?.enabled
        }
        onChanged={refresh}
      />
    );
  if (page === 'branches' && branchId) {
    const branch = branches.find((item) => item.id === branchId);
    return branch ? (
      <BranchDetail
        projectId={projectId}
        branch={branch}
        endpoints={endpoints.filter((item) => item.branch_id === branchId)}
        canCreateEndpoint={
          canEdit &&
          !!capabilities?.features.endpoint_create?.enabled &&
          project.source === 'managed'
        }
        onChanged={refresh}
        showError={showError}
      />
    ) : (
      <Empty title="分支不存在" message="请返回分支列表。" />
    );
  }
  if (page === 'branches')
    return (
      <>
        <PageHeading
          kicker="PROJECT / BRANCHES"
          title="数据库分支"
          description="每个分支拥有独立的 Postgres timeline 与 Compute 连接。"
          action={
            <button
              className="button primary"
              disabled={
                !canEdit ||
                !capabilities?.features.branch_create?.enabled ||
                project.source !== 'managed' ||
                project.state !== 'ready'
              }
              title={capabilities?.features.branch_create?.reason || '能力检查中'}
              onClick={() => setShowCreateBranch(true)}
            >
              ＋ 创建分支
            </button>
          }
        />
        {showCreateBranch && (
          <CreateResource
            kind="branch"
            projectId={projectId}
            branches={branches}
            allowHistorical={!!capabilities?.features.pitr_new_branch?.enabled}
            onClose={() => setShowCreateBranch(false)}
            onDone={(id) => {
              setShowCreateBranch(false);
              void refresh();
              location.hash = route(projectId, 'branches/' + encodeURIComponent(id));
            }}
          />
        )}
        <div className="table-card">
          <table>
            <thead>
              <tr>
                <th>分支</th>
                <th>状态</th>
                <th>来源</th>
                <th>Compute</th>
                <th>创建时间</th>
              </tr>
            </thead>
            <tbody>
              {branches.map((branch) => (
                <tr key={branch.id}>
                  <td>
                    <div className="table-title">
                      <a href={route(projectId, 'branches/' + encodeURIComponent(branch.id))}>
                        ⑂ {branch.name}
                      </a>
                      {branch.is_default && <span className="tag">默认</span>}
                    </div>
                    <small>{branch.id}</small>
                  </td>
                  <td>{status(branch.state)}</td>
                  <td>
                    {branch.parent_branch_id ? branchName(branch.parent_branch_id) : '根分支'}
                  </td>
                  <td>{endpoints.filter((endpoint) => endpoint.branch_id === branch.id).length}</td>
                  <td>{fmt(branch.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="subtle-note">
          当前只有 PostgreSQL 数据分支。完整 Backend 分支需要每项服务独立实现与验收。
        </div>
      </>
    );
  if (page === 'compute')
    return (
      <Compute
        readOnly={!canEdit}
        projectId={projectId}
        endpoints={endpoints}
        branchName={branchName}
        onChanged={refresh}
        showError={showError}
      />
    );
  if (page === 'monitoring')
    return (
      <Monitoring
        projectId={projectId}
        endpoints={endpoints}
        branchName={branchName}
        showError={showError}
      />
    );
  if (page === 'connect')
    return (
      <Connections
        projectId={projectId}
        endpoints={endpoints}
        branchName={branchName}
        showError={showError}
      />
    );
  if (page === 'query')
    return (
      <Workbench
        projectId={projectId}
        endpoints={endpoints}
        branchName={branchName}
        showError={showError}
      />
    );
  if (page === 'operations')
    return (
      <Operations
        projectId={projectId}
        operations={operations}
        canEdit={canEdit}
        onChanged={refresh}
        showError={showError}
      />
    );
  return (
    <>
      <PageHeading
        kicker="PROJECT / OVERVIEW"
        title={project.name}
        description={`${project.region_id} · PostgreSQL ${project.postgres_version} · ${project.id}`}
        action={status(project.state)}
      />
      <div className="hero-panel">
        <div>
          <span className="eyebrow">PROJECT HEALTH</span>
          <h2>您的 Postgres 工作空间正在运行</h2>
          <p>通过 Neon Proxy 访问分支数据库，Compute 根据负载在已配置范围内伸缩。</p>
          <a className="button primary" href={route(projectId, 'connect')}>
            查看连接方式 ↗
          </a>
        </div>
        <div className="hero-orbit">
          <span>◈</span>
        </div>
      </div>
      <div className="stats-grid">
        <div className="stat-card">
          <small>数据库分支</small>
          <strong>{branches.length}</strong>
          <span>独立 Timeline</span>
        </div>
        <div className="stat-card">
          <small>Compute Endpoint</small>
          <strong>{endpoints.length}</strong>
          <span>{endpoints.filter((e) => e.observed_state === 'active').length} 个运行中</span>
        </div>
        <div className="stat-card">
          <small>操作记录</small>
          <strong>{operations.length}</strong>
          <span>{operations.filter((o) => o.state === 'failed').length} 个失败</span>
        </div>
        <div className="stat-card">
          <small>项目状态</small>
          <strong>{stateLabel(project.state)}</strong>
          <span>最后更新见操作记录</span>
        </div>
      </div>
      <div className="two-col">
        <section className="panel">
          <div className="panel-heading">
            <h2>分支与 Compute</h2>
            <a href={route(projectId, 'branches')}>查看全部 ↗</a>
          </div>
          {branches.map((branch) => (
            <div className="list-row" key={branch.id}>
              <div className="round-icon">⑂</div>
              <div>
                <strong>{branch.name}</strong>
                <small>
                  {branch.is_default ? '默认分支' : '子分支'} · {short(branch.id)}
                </small>
              </div>
              <span>
                {endpoints.some((e) => e.branch_id === branch.id && e.observed_state === 'active')
                  ? status('active')
                  : status('unknown')}
              </span>
            </div>
          ))}
        </section>
        <section className="panel">
          <div className="panel-heading">
            <h2>最近操作</h2>
            <a href={route(projectId, 'operations')}>查看全部 ↗</a>
          </div>
          {operations.length ? (
            operations.slice(0, 4).map((op) => (
              <div className="list-row" key={op.id}>
                <div className="round-icon">◷</div>
                <div>
                  <strong>{op.action}</strong>
                  <small>{fmt(op.created_at)}</small>
                </div>
                <span>{status(op.state)}</span>
              </div>
            ))
          ) : (
            <Empty title="暂无操作" message="新的资源变更将在这里出现。" />
          )}
        </section>
      </div>
    </>
  );
}

const serviceLabels: Record<string, string> = {
  postgres: 'Lakebase Postgres',
  auth: 'Managed Better Auth',
  object_storage: 'Object Storage',
  functions: 'Functions',
  ai_gateway: 'AI Gateway',
  data_api: 'Data API',
};
const serviceDescriptions: Record<string, string> = {
  postgres: '分支数据、角色、数据库与 Compute',
  auth: '分支内用户与会话，状态随数据库分支',
  object_storage: '写时复制的对象和桶',
  functions: '此分支独立部署的后端函数',
  ai_gateway: '分支凭据、路由和用量计量',
  data_api: '面向 HTTP 客户端的 Postgres 接口',
};
function BranchDetail({
  projectId,
  branch,
  endpoints,
  canCreateEndpoint,
  onChanged,
  showError,
}: {
  projectId: string;
  branch: Branch;
  endpoints: Endpoint[];
  canCreateEndpoint: boolean;
  onChanged: () => Promise<void>;
  showError: (error: unknown) => void;
}) {
  const [services, setServices] = useState<BranchService[] | null>(null);
  const [showCreateEndpoint, setShowCreateEndpoint] = useState(false);
  useEffect(() => {
    api<{ items: BranchService[] }>(
      `${projectPath(projectId)}/branches/${encodeURIComponent(branch.id)}/services`,
    )
      .then((data) => setServices(data.items))
      .catch(showError);
  }, [projectId, branch.id]);
  return (
    <>
      {showCreateEndpoint && (
        <CreateResource
          kind="endpoint"
          projectId={projectId}
          branchId={branch.id}
          writerExists={endpoints.some((endpoint) => endpoint.endpoint_type === 'read_write')}
          onClose={() => setShowCreateEndpoint(false)}
          onDone={() => {
            setShowCreateEndpoint(false);
            void onChanged();
          }}
        />
      )}
      <PageHeading
        kicker="PROJECT / BRANCH / BACKEND"
        title={branch.name}
        description={`${branch.id} · ${branch.is_default ? '根分支' : '子分支'} · ${fmt(branch.created_at)}`}
        action={
          <a className="button" href={route(projectId, 'branches')}>
            ← 全部分支
          </a>
        }
      />
      <div className="hero-panel">
        <div>
          <span className="eyebrow">BRANCH BACKEND</span>
          <h2>独立的分支工作空间</h2>
          <p>当前已验证 PostgreSQL 数据分支。其它 Backend 服务按独立 Driver 和验收逐项开放。</p>
          {endpoints.some((endpoint) =>
            ['active', 'suspended'].includes(endpoint.observed_state),
          ) && (
            <a className="button primary" href={route(projectId, 'connect')}>
              连接 Postgres ↗
            </a>
          )}
        </div>
        <div className="hero-orbit">
          <span>⑂</span>
        </div>
      </div>
      <div className="section-header">
        <h2>Backend 服务</h2>
        {status(branch.state)}
      </div>
      {canCreateEndpoint && branch.state === 'ready' && endpoints.length > 0 && (
        <div className="section-header">
          <span className="muted">1 个读写节点 · 可添加多个独立只读节点</span>
          <button className="button primary" onClick={() => setShowCreateEndpoint(true)}>
            ＋ 添加 Compute
          </button>
        </div>
      )}
      {endpoints.length === 0 && (
        <div className="notice compact">
          <span>◈</span>
          <div>
            <strong>此分支尚无 Compute Endpoint</strong>
            <p>Timeline 已创建。添加 Endpoint 后可通过 Proxy 连接并执行 SQL。</p>
            <button
              className="button primary"
              disabled={!canCreateEndpoint || branch.state !== 'ready'}
              onClick={() => setShowCreateEndpoint(true)}
            >
              ＋ 创建 Endpoint
            </button>
          </div>
        </div>
      )}
      <div className="service-grid">
        {services === null ? (
          <div className="skeleton">正在读取分支服务…</div>
        ) : (
          services.map((service) => (
            <section
              className={`service-card ${service.enabled ? 'service-active' : ''}`}
              key={service.service_kind}
            >
              <div className="service-top">
                <div className="project-icon">
                  {service.service_kind === 'postgres' ? '◈' : '◇'}
                </div>
                {service.enabled ? (
                  status(
                    service.service_kind === 'postgres'
                      ? endpoints.some((endpoint) => endpoint.observed_state === 'active')
                        ? 'active'
                        : endpoints.length === 0
                          ? 'ready'
                          : endpoints.every((endpoint) => endpoint.observed_state === 'suspended')
                            ? 'suspended'
                            : endpoints.some((endpoint) => endpoint.observed_state === 'failed')
                              ? 'failed'
                              : endpoints.some((endpoint) =>
                                    ['provisioning', 'starting'].includes(endpoint.observed_state),
                                  )
                                ? 'provisioning'
                                : 'unknown'
                      : service.observed_state,
                  )
                ) : (
                  <span className="tag">规划中</span>
                )}
              </div>
              <h3>{serviceLabels[service.service_kind] || service.service_kind}</h3>
              <p>{serviceDescriptions[service.service_kind] || ''}</p>
              <small>
                {service.enabled
                  ? `${service.driver_version || '驱动未标记'} · ${endpoints.length} Compute`
                  : '当前开源部署尚未提供可验收 Driver'}
              </small>
            </section>
          ))
        )}
      </div>
      <div className="subtle-note">
        Data API 属于 Postgres 子系统；Auth、Object Storage、Functions、AI Gateway
        是分支服务。当前页面没有把 PostgreSQL timeline 克隆误称为完整 Backend 克隆。
      </div>
    </>
  );
}
