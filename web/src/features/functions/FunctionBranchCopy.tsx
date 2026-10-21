import { useEffect, useRef, useState } from 'react';
import { api, ApiError, projectPath, type Branch, type Operation, type Page } from '../../api';
import { newRequestKey } from '../../shared/requestKey';

type Source = {
  id: string;
  slug: string;
  state: string;
  version_token: string;
  active_deployment_id: string | null;
};
type Deployment = { id: string; state: string; bundle_digest: string; environment_names: string[] };
type Pending = { key: string; version: string; body: string };
export function FunctionBranchCopy({
  projectId,
  targetBranch,
  branches,
  enabled,
  disabled,
  onAccepted,
}: {
  projectId: string;
  targetBranch: string;
  branches: Branch[];
  enabled: boolean;
  disabled: boolean;
  onAccepted: (reply: { operation: Operation }) => void;
}) {
  const [sourceBranch, setSourceBranch] = useState(''),
    [sources, setSources] = useState<Source[]>([]),
    [sourceSlug, setSourceSlug] = useState(''),
    [deployments, setDeployments] = useState<Deployment[]>([]),
    [after, setAfter] = useState<string | null>(null),
    [deployment, setDeployment] = useState(''),
    [slug, setSlug] = useState(''),
    [ack, setAck] = useState(false),
    [refresh, setRefresh] = useState(0);
  const [busy, setBusy] = useState(false),
    [error, setError] = useState(''),
    [pending, setPending] = useState<Pending | null>(null);
  const sourcePath = `${projectPath(projectId)}/branches/${sourceBranch}/functions`;
  const selected = sources.find((s) => s.slug === sourceSlug);
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    setSources([]);
    setSourceSlug('');
    setDeployments([]);
    setDeployment('');
    setAfter(null);
    setError('');
    if (!sourceBranch) return;
    setBusy(true);
    void api<Page<Source>>(sourcePath + '?limit=100', { signal: controller.signal })
      .then((data) => {
        if (!controller.signal.aborted)
          setSources(data.items.filter((f) => ['active', 'suspended'].includes(f.state)));
      })
      .catch((e) => {
        if (!controller.signal.aborted) setError(String(e));
      })
      .finally(() => {
        if (!controller.signal.aborted) setBusy(false);
      });
    return () => controller.abort();
  }, [sourcePath, refresh]);
  useEffect(() => {
    const controller = new AbortController();
    setDeployments([]);
    setDeployment('');
    setAfter(null);
    setError('');
    if (!sourceSlug) return;
    setBusy(true);
    void api<Page<Deployment>>(sourcePath + '/' + sourceSlug + '/deployments?limit=20', {
      signal: controller.signal,
    })
      .then((data) => {
        if (!controller.signal.aborted) {
          setDeployments(data.items);
          setAfter(data.next_cursor);
        }
      })
      .catch((e) => {
        if (!controller.signal.aborted) setError(String(e));
      })
      .finally(() => {
        if (!controller.signal.aborted) setBusy(false);
      });
    return () => controller.abort();
  }, [sourcePath, sourceSlug]);
  async function more() {
    if (!after || busy || pending) return;
    const path =
      sourcePath + '/' + sourceSlug + '/deployments?limit=20&after=' + encodeURIComponent(after);
    setBusy(true);
    try {
      const data = await api<Page<Deployment>>(path);
      if (live.current) {
        setDeployments((old) => [...old, ...data.items]);
        setAfter(data.next_cursor);
      }
    } catch (e) {
      if (live.current) setError(String(e));
    } finally {
      if (live.current) setBusy(false);
    }
  }
  async function submit(intent: Pending) {
    setPending(intent);
    setBusy(true);
    setError('');
    try {
      const reply = await api<{ operation: Operation }>(
        `${projectPath(projectId)}/branches/${targetBranch}/functions-copy`,
        {
          method: 'POST',
          headers: { 'Idempotency-Key': intent.key, 'If-Match': `"${intent.version}"` },
          body: intent.body,
        },
      );
      if (!live.current) return;
      setPending(null);
      setAck(false);
      onAccepted(reply);
    } catch (e) {
      if (!live.current) return;
      setError(String(e));
      if (
        e instanceof ApiError &&
        e.status >= 400 &&
        e.status < 500 &&
        ![408, 429].includes(e.status)
      )
        setPending(null);
    } finally {
      if (live.current) setBusy(false);
    }
  }
  const blocked = disabled || busy || !!pending;
  return (
    <section className="panel function-authoring" aria-label="跨分支复制函数">
      <h2>从其他分支复制函数</h2>
      <p className="muted">
        固定所选代码版本，环境在目标分支重新封装，数据库使用目标分支的新受限身份。准备成功后保持暂停；不会启动客户代码，也不继承公开入口、日志采集授权或已启用触发器。
      </p>
      {!enabled && <p>当前尚未启用分支函数复制。</p>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!selected || blocked || !enabled || !ack || !slug || !deployment) return;
          void submit({
            key: newRequestKey(),
            version: selected.version_token,
            body: JSON.stringify({
              source_branch_id: sourceBranch,
              source_function_slug: sourceSlug,
              source_deployment_id: deployment,
              slug,
              acknowledge_environment_copy: ack,
            }),
          });
        }}
      >
        <div className="function-form-grid">
          <label>
            来源分支
            <select
              aria-label="复制来源分支"
              value={sourceBranch}
              disabled={blocked}
              onChange={(e) => setSourceBranch(e.target.value)}
            >
              <option value="">选择来源分支</option>
              {branches
                .filter((b) => b.id !== targetBranch && b.state === 'ready')
                .map((b) => (
                  <option key={b.id} value={b.id}>
                    {b.name}
                  </option>
                ))}
            </select>
          </label>
          <label>
            来源函数
            <select
              aria-label="复制来源函数"
              value={sourceSlug}
              disabled={blocked || !sources.length}
              onChange={(e) => {
                setSourceSlug(e.target.value);
                setSlug(e.target.value);
              }}
            >
              <option value="">选择来源函数</option>
              {sources.map((f) => (
                <option key={f.id} value={f.slug}>
                  {f.slug} · v{f.version_token}
                </option>
              ))}
            </select>
          </label>
          <label>
            完成版本
            <select
              aria-label="复制来源部署"
              value={deployment}
              disabled={blocked || !sourceSlug}
              onChange={(e) => setDeployment(e.target.value)}
            >
              <option value="">选择明确版本</option>
              {deployments
                .filter((d) => d.state === 'completed')
                .map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.id}
                    {d.id === selected?.active_deployment_id ? ' · 当前接受' : ''}
                  </option>
                ))}
            </select>
          </label>
          <label>
            目标函数名称
            <input
              aria-label="复制目标函数名称"
              value={slug}
              maxLength={20}
              disabled={blocked}
              onChange={(e) => setSlug(e.target.value)}
            />
          </label>
        </div>
        {after && (
          <button type="button" disabled={blocked} onClick={() => void more()}>
            更多复制来源版本
          </button>
        )}
        <button
          type="button"
          disabled={blocked || !sourceBranch}
          onClick={() => setRefresh((v) => v + 1)}
        >
          刷新复制来源
        </button>
        <label>
          <input
            type="checkbox"
            aria-label="确认复制环境到目标分支"
            checked={ack}
            disabled={blocked}
            onChange={(e) => setAck(e.target.checked)}
          />
          确认把所选版本的环境复制到目标分支；旧版本不会随父分支后续更新而改变
        </label>
        <button
          disabled={
            blocked ||
            !enabled ||
            !selected ||
            !deployment ||
            !/^[a-z0-9]{1,20}$/.test(slug) ||
            !ack
          }
        >
          复制并准备暂停函数
        </button>
      </form>
      {error && <p role="alert">{error}</p>}
      {pending && !busy && (
        <p role="status">
          复制受理结果待确认。
          <button disabled={disabled} onClick={() => void submit(pending)}>
            重试原复制请求
          </button>
        </p>
      )}
    </section>
  );
}
