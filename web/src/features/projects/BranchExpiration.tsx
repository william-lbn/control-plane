import { useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

export type ExpiringBranch = {
  id: string;
  name: string;
  state: string;
  version: number;
  is_default: boolean;
  protected: boolean;
  child_count: number;
  deleted_at: string | null;
  expires_at: string | null;
  ttl_interval_seconds: number | null;
  expiration_status: string;
  expiration_error_code: string | null;
  expiration_operation_id: string | null;
};

export function BranchExpiration({
  projectId,
  branch,
  canEdit,
  onChanged,
}: {
  projectId: string;
  branch: ExpiringBranch;
  canEdit: boolean;
  onChanged: () => Promise<void>;
}) {
  const [target, setTarget] = useState<ExpiringBranch | null>(null),
    [value, setValue] = useState(''),
    [remove, setRemove] = useState(false),
    [confirmation, setConfirmation] = useState(''),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const key = useRef('');
  const elapsed = !!branch.expires_at && Date.parse(branch.expires_at) <= Date.now();
  const canSet =
    canEdit &&
    branch.state === 'ready' &&
    !branch.deleted_at &&
    !branch.protected &&
    !branch.is_default &&
    branch.child_count === 0 &&
    !elapsed;
  const open = () => {
    setTarget({ ...branch });
    setValue(
      branch.expires_at?.slice(0, 19) ?? new Date(Date.now() + 86400000).toISOString().slice(0, 19),
    );
    setRemove(false);
    setConfirmation('');
    setError('');
    key.current = '';
  };
  async function submit() {
    if (!target || busy || confirmation !== target.name) return;
    let expires: string | null = null;
    if (!remove) {
      const date = new Date(value + 'Z');
      if (!value || !Number.isFinite(date.getTime())) {
        setError('请选择有效的 UTC 到期时间。');
        return;
      }
      expires = date.toISOString();
    }
    setBusy(true);
    setError('');
    try {
      if (!key.current) key.current = newRequestKey();
      await api(projectPath(projectId) + '/branches/' + target.id + '/expiration', {
        method: 'PATCH',
        headers: { 'If-Match': `"${target.version}"`, 'Idempotency-Key': key.current },
        body: JSON.stringify({ expires_at: expires, confirm_name: confirmation }),
      });
      await onChanged();
      setTarget(null);
    } catch (error) {
      setError(String(error));
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      {branch.expires_at && (
        <div>
          <small>
            UTC 到期：
            <time dateTime={branch.expires_at}>
              {branch.expires_at.replace('T', ' ').replace('Z', ' UTC')}
            </time>
          </small>
          <small data-testid={'expiration-status-' + branch.id}>
            {branch.expiration_status}
            {branch.expiration_error_code ? ' · ' + branch.expiration_error_code : ''}
          </small>
        </div>
      )}
      {branch.state === 'ready' && !branch.deleted_at && (
        <button className="button" disabled={!canSet} onClick={open}>
          {branch.expires_at ? '修改到期时间' : '设置到期时间'}
        </button>
      )}
      {target && (
        <div className="modal-backdrop">
          <section className="modal" role="dialog" aria-modal="true" aria-label="分支到期设置">
            <h2>分支到期：{target.name}</h2>
            <p>
              到期后后台关闭此分支的入口并回收原 Compute，原
              timeline、WAL、对象及凭据保留。仅允许未来 30 天内的时间；到期后不能撤回。
            </p>
            <p className="muted">
              仅支持非默认、未保护、无子分支的就绪分支。请先停用 Data API、退休
              Functions；设置后不能创建子分支、新 Compute 或这些服务。
            </p>
            <label>
              <input
                type="checkbox"
                aria-label="取消自动到期"
                checked={remove}
                disabled={busy}
                onChange={(e) => {
                  setRemove(e.target.checked);
                  key.current = '';
                }}
              />
              取消未来的自动到期
            </label>
            {!remove && (
              <>
                <label>
                  UTC 到期时间
                  <input
                    type="datetime-local"
                    step="1"
                    aria-label="UTC 到期时间"
                    value={value}
                    disabled={busy}
                    onChange={(e) => {
                      setValue(e.target.value);
                      key.current = '';
                    }}
                  />
                </label>
                <div className="form-actions">
                  {[
                    [3600, '1 小时'],
                    [86400, '1 天'],
                    [604800, '7 天'],
                  ].map(([seconds, label]) => (
                    <button
                      className="button"
                      key={label}
                      disabled={busy}
                      onClick={() => {
                        setValue(
                          new Date(Date.now() + Number(seconds) * 1000).toISOString().slice(0, 19),
                        );
                        key.current = '';
                      }}
                    >
                      {label}
                    </button>
                  ))}
                </div>
              </>
            )}
            <label>
              输入完整分支名称确认
              <input
                aria-label="到期分支名称确认"
                value={confirmation}
                disabled={busy}
                onChange={(e) => setConfirmation(e.target.value)}
                autoComplete="off"
              />
            </label>
            {error && (
              <p role="alert" className="inline-error">
                {error}
              </p>
            )}
            <div className="form-actions">
              <button className="button" disabled={busy} onClick={() => setTarget(null)}>
                关闭
              </button>
              <button
                className="button danger"
                disabled={busy || confirmation !== target.name}
                onClick={() => void submit()}
              >
                {busy ? '正在保存…' : '确认到期设置'}
              </button>
            </div>
          </section>
        </div>
      )}
    </>
  );
}
