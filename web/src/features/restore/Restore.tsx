import { useState } from 'react';
import { route } from '../../api';
import type { Branch } from '../../api';
import { PageHeading, fmt, status } from '../../shared/ui';
import { CreateResource } from '../projects/CreateResources';

export function Restore({
  projectId,
  branches,
  enabled,
  canEdit,
  onChanged,
}: {
  projectId: string;
  branches: Branch[];
  enabled: boolean;
  canEdit: boolean;
  onChanged: () => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [source, setSource] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const history = branches.filter(
    (b) => b.restore_source === 'timestamp' || b.restore_source === 'lsn',
  );
  return (
    <>
      <PageHeading
        kicker="PROJECT / RECOVERY"
        title="历史恢复"
        description="从保留的 PostgreSQL 历史创建独立分支，验证数据后再决定业务迁移。"
        action={
          <button
            className="button primary"
            disabled={!enabled || !canEdit || !source}
            onClick={() => setOpen(true)}
          >
            恢复到新分支
          </button>
        }
      />
      <div className="two-col">
        <section className="panel pad">
          <div className="panel-heading">
            <h2>选择恢复来源</h2>
          </div>
          <label>
            源分支
            <select
              aria-label="恢复源分支"
              value={source}
              onChange={(e) => setSource(e.target.value)}
            >
              {branches
                .filter((b) => b.state === 'ready')
                .map((b) => (
                  <option key={b.id} value={b.id}>
                    {b.name} · {b.id}
                  </option>
                ))}
            </select>
          </label>
          <p className="muted">
            支持带时区的时间戳或精确 LSN。控制面以存储的实际保留边界为准，固定恢复点并跟踪持久操作。
          </p>
          {!enabled && <div className="subtle-note">当前部署尚未启用历史分支恢复。</div>}
        </section>
        <section className="panel pad">
          <div className="panel-heading">
            <h2>恢复流程</h2>
            <span className="tag">独立新分支</span>
          </div>
          <ol>
            <li>选择历史时间或 LSN，校验可恢复范围。</li>
            <li>固定恢复点，创建 Timeline 和可选 Compute。</li>
            <li>通过 SQL 工作台检查历史数据与分支隔离。</li>
          </ol>
          <p className="muted">
            源分支与已有 Endpoint 保持当前状态。平台凭据和用量账本不会回滚；外部服务恢复需各自支持。
          </p>
        </section>
      </div>
      <section className="table-card">
        <table>
          <thead>
            <tr>
              <th>恢复分支</th>
              <th>来源</th>
              <th>恢复点</th>
              <th>状态</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            {history.map((b) => (
              <tr key={b.id}>
                <td>
                  <a href={route(projectId, 'branches/' + encodeURIComponent(b.id))}>{b.name}</a>
                  <small>{b.id}</small>
                </td>
                <td>
                  {branches.find((p) => p.id === b.parent_branch_id)?.name || b.parent_branch_id}
                </td>
                <td>
                  {b.parent_timestamp && <div>{fmt(b.parent_timestamp)}</div>}
                  <code>{b.parent_lsn}</code>
                </td>
                <td>{status(b.state)}</td>
                <td>{fmt(b.created_at)}</td>
              </tr>
            ))}
            {!history.length && (
              <tr>
                <td colSpan={5}>尚无历史恢复分支。</td>
              </tr>
            )}
          </tbody>
        </table>
      </section>
      {open && (
        <CreateResource
          kind="branch"
          projectId={projectId}
          branchId={source}
          branches={branches}
          allowHistorical
          restoreOnly
          onClose={() => setOpen(false)}
          onDone={(id) => {
            setOpen(false);
            void onChanged();
            location.hash = route(projectId, 'branches/' + encodeURIComponent(id));
          }}
        />
      )}
    </>
  );
}
