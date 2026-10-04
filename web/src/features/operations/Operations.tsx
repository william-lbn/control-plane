import { useState } from 'react';
import { api, projectPath } from '../../api';
import type { Operation } from '../../api';
import { fmt, short, status, PageHeading, Empty } from '../../shared/ui';

export function Operations({
  projectId,
  operations,
  canEdit,
  onChanged,
  showError,
}: {
  projectId: string;
  operations: Operation[];
  canEdit: boolean;
  onChanged: () => Promise<void>;
  showError: (error: unknown) => void;
}) {
  const [selected, setSelected] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const detail = operations.find((op) => op.id === selected);
  // Durable operations outlive a creation modal or a browser refresh. Retry the
  // existing identity from history rather than creating another resource.
  async function retry() {
    if (!detail || !canEdit || busy) return;
    setBusy(true);
    try {
      await api(`${projectPath(projectId)}/operations/${encodeURIComponent(detail.id)}/retry`, {
        method: 'POST',
      });
      await onChanged();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeading
        kicker="PROJECT / ACTIVITY"
        title="操作记录"
        description="每次异步调谐都有可追踪的状态与请求 ID。"
      />
      <div className="table-card">
        <table>
          <thead>
            <tr>
              <th>操作</th>
              <th>资源</th>
              <th>状态</th>
              <th>提交时间</th>
              <th>完成时间</th>
            </tr>
          </thead>
          <tbody>
            {operations.map((op) => (
              <tr key={op.id} className="clickable">
                <td>
                  <button
                    className="operation-open"
                    type="button"
                    onClick={() => setSelected(op.id)}
                    aria-label={`查看操作 ${op.id} 的步骤`}
                  >
                    <span className="table-title">◷ {op.action}</span>
                    <small>{op.id}</small>
                  </button>
                </td>
                <td>
                  <code>{short(op.resource_id)}</code>
                </td>
                <td>{status(op.state)}</td>
                <td>{fmt(op.created_at)}</td>
                <td>{fmt(op.finished_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {operations.length === 0 && (
          <Empty title="暂无操作" message="v2 控制面的新调谐记录会显示在这里。" />
        )}
      </div>
      {detail && (
        <div className="modal-backdrop" onClick={() => setSelected(null)}>
          <div
            className="modal"
            role="dialog"
            aria-modal="true"
            aria-label="操作详情"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="panel-heading">
              <h2>{detail.action}</h2>
              <button className="icon-button" onClick={() => setSelected(null)} aria-label="关闭">
                ×
              </button>
            </div>
            <div className="detail-row">
              <span>操作 ID</span>
              <code>{detail.id}</code>
            </div>
            <div className="detail-row">
              <span>资源</span>
              <code>{detail.resource_id}</code>
            </div>
            <div className="detail-row">
              <span>状态</span>
              {status(detail.state)}
            </div>
            {detail.error_message && (
              <div className="form-error">
                {detail.error_code} · {detail.error_message}
              </div>
            )}
            {canEdit && detail.state === 'failed' && detail.retryable && (
              <button className="button primary" onClick={retry} disabled={busy}>
                {busy ? '正在提交重试…' : '重试原操作'}
              </button>
            )}
            <h3>执行步骤</h3>
            {detail.steps.length ? (
              detail.steps.map((step) => (
                <div key={step.ordinal} className="list-row">
                  <span>
                    {step.ordinal + 1}. {step.name}
                  </span>
                  {status(step.state)}
                </div>
              ))
            ) : (
              <p className="muted">尚无步骤记录。</p>
            )}
          </div>
        </div>
      )}
    </>
  );
}
