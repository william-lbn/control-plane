import { useEffect, useRef, useState } from 'react';
import { api, projectPath, route } from '../../api';
import type { Branch } from '../../api';
import { PageHeading, Empty, fmt } from '../../shared/ui';

type Model = { id: string; name: string; max_output_tokens: number; protocols: string[] };
type Catalog = { enabled: boolean; models: Model[]; public_endpoint: string };
type Usage = {
  items: {
    id: string;
    model_id: string;
    state: string;
    total_tokens: number | null;
    duration_ms: number | null;
    started_at: string;
  }[];
  summary: {
    requests: number;
    completed: number;
    unknown_usage: number;
    known_total_tokens: number;
  };
};

export function AIGateway({
  projectId,
  branches,
  canEdit,
}: {
  projectId: string;
  branches: Branch[];
  canEdit: boolean;
}) {
  const [branch, setBranch] = useState(
    branches.find((b) => b.is_default)?.id || branches[0]?.id || '',
  );
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [usage, setUsage] = useState<Usage | null>(null);
  const [model, setModel] = useState('');
  const [prompt, setPrompt] = useState('');
  const [answer, setAnswer] = useState('');
  const [stream, setStream] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [requestId, setRequestId] = useState('');
  const abort = useRef<AbortController | null>(null);
  const prefix = projectPath(projectId) + `/branches/${encodeURIComponent(branch)}/ai-gateway`;
  useEffect(() => {
    let cancelled = false;
    setCatalog(null);
    setUsage(null);
    setPrompt('');
    setAnswer('');
    setError('');
    setRequestId('');
    Promise.all([api<Catalog>(prefix), api<Usage>(prefix + '/usage')])
      .then(([c, u]) => {
        if (cancelled) return;
        setCatalog(c);
        setModel(c.models[0]?.id || '');
        setUsage(u);
      })
      .catch((e) => {
        if (!cancelled) setError(String(e));
      });
    return () => {
      cancelled = true;
      abort.current?.abort();
    };
  }, [prefix]);
  async function invoke() {
    setBusy(true);
    setError('');
    setAnswer('');
    setRequestId('');
    const controller = new AbortController();
    abort.current = controller;
    try {
      const csrf =
        document.cookie
          .split('; ')
          .find((v) => v.startsWith('neon_v2_csrf='))
          ?.slice('neon_v2_csrf='.length) || '';
      const selected = catalog?.models.find((m) => m.id === model);
      const response = await fetch(prefix + '/request', {
        method: 'POST',
        credentials: 'same-origin',
        signal: controller.signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': decodeURIComponent(csrf) },
        body: JSON.stringify({
          model,
          messages: [{ role: 'user', content: prompt }],
          max_tokens: Math.min(128, selected?.max_output_tokens || 128),
          temperature: 0,
          stream,
        }),
      });
      if (!response.ok) {
        const data = await response.json();
        throw new Error(data.error?.message || `HTTP ${response.status}`);
      }
      setRequestId(response.headers.get('X-Request-ID') || '');
      if (!stream) {
        const data = await response.json();
        setAnswer(data.choices?.[0]?.message?.content || '');
      } else {
        const reader = response.body?.getReader();
        if (!reader) throw new Error('响应流不可用');
        const decoder = new TextDecoder();
        let pending = '';
        let done = false;
        let text = '';
        while (!done) {
          const chunk = await reader.read();
          if (chunk.done) break;
          pending += decoder.decode(chunk.value, { stream: true });
          let end: number;
          while ((end = pending.indexOf('\n\n')) >= 0) {
            const event = pending.slice(0, end);
            pending = pending.slice(end + 2);
            const data = event
              .split('\n')
              .find((v) => v.startsWith('data:'))
              ?.slice(5)
              .trim();
            if (!data) continue;
            if (data === '[DONE]') {
              done = true;
              break;
            }
            const value = JSON.parse(data);
            if (value.error) throw new Error(value.error.code || '推理流中断');
            text += value.choices?.[0]?.delta?.content || '';
            setAnswer(text);
          }
        }
        if (!done) throw new Error('推理流未正常结束；不会自动重试');
      }
      setUsage(await api<Usage>(prefix + '/usage'));
    } catch (e) {
      if (!controller.signal.aborted) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <div className="ai-workspace">
      <PageHeading
        kicker="BACKEND / AI GATEWAY"
        title="AI Gateway"
        description="统一分支入口 · 真实模型推理 · 应用凭据与用量记录"
      />
      <section className="panel">
        <label>
          AI 分支
          <select
            aria-label="AI 分支"
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
      </section>
      {error && (
        <div role="alert" className="subtle-note" data-testid="ai-error">
          {error}
        </div>
      )}
      {catalog && !catalog.enabled && (
        <Empty
          title="尚未配置模型上游"
          message="平台运营方配置真实供应商或自建模型后，模型目录和 Playground 将可用。"
        />
      )}
      {catalog?.enabled && (
        <>
          <div className="hero-panel">
            <div>
              <span className="eyebrow">BRANCH INFERENCE</span>
              <h2>从分支直接调用模型</h2>
              <p>模型调用独立于数据库 Compute。提示词和回复不写入平台日志或用量表。</p>
              <code>
                {location.origin}
                {catalog.public_endpoint}
              </code>
              <p>
                <a href={route(projectId, 'credentials')}>创建应用凭据 ↗</a>
              </p>
            </div>
          </div>
          <div className="stats-grid">
            <div className="stat-card">
              <small>24h 请求</small>
              <strong>{usage?.summary.requests || 0}</strong>
            </div>
            <div className="stat-card">
              <small>完成请求</small>
              <strong>{usage?.summary.completed || 0}</strong>
            </div>
            <div className="stat-card">
              <small>已知 Token</small>
              <strong>{usage?.summary.known_total_tokens || 0}</strong>
            </div>
            <div className="stat-card">
              <small>用量未确定</small>
              <strong>{usage?.summary.unknown_usage || 0}</strong>
            </div>
          </div>
          <section className="panel ai-playground">
            <div className="panel-heading">
              <h2>Playground</h2>
              <span className="tag">Text Chat · Preview</span>
            </div>
            <label>
              模型
              <select
                aria-label="AI 模型"
                value={model}
                disabled={busy}
                onChange={(e) => setModel(e.target.value)}
              >
                {catalog.models.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name} · {m.id}
                  </option>
                ))}
              </select>
            </label>
            <label>
              输入
              <textarea
                aria-label="AI 输入"
                rows={4}
                maxLength={4000}
                value={prompt}
                disabled={busy}
                onChange={(e) => setPrompt(e.target.value)}
              />
            </label>
            <label className="ai-stream-mode">
              <input
                aria-label="AI 流式输出"
                type="checkbox"
                checked={stream}
                disabled={busy}
                onChange={(e) => setStream(e.target.checked)}
              />
              流式输出
            </label>
            <button
              className="button primary"
              disabled={!canEdit || !model || !prompt.trim() || busy}
              onClick={() => void invoke()}
            >
              {busy ? '模型生成中…' : '发送到模型'}
            </button>
            <pre className="ai-response" data-testid="ai-answer">
              {answer || '模型输出将显示在这里'}
            </pre>
            {requestId && <small data-testid="ai-request-id">Request ID · {requestId}</small>}
          </section>
          <section className="table-card">
            <div className="panel-heading">
              <h2>最近请求</h2>
              <button
                className="button"
                onClick={() =>
                  void api<Usage>(prefix + '/usage')
                    .then(setUsage)
                    .catch((e) => setError(String(e)))
                }
              >
                刷新用量
              </button>
            </div>
            <table>
              <thead>
                <tr>
                  <th>请求</th>
                  <th>模型</th>
                  <th>状态</th>
                  <th>Token</th>
                  <th>耗时</th>
                  <th>时间</th>
                </tr>
              </thead>
              <tbody>
                {usage?.items.map((item) => (
                  <tr key={item.id}>
                    <td>{item.id}</td>
                    <td>{item.model_id}</td>
                    <td>{item.state}</td>
                    <td>{item.total_tokens ?? '未确定'}</td>
                    <td>{item.duration_ms === null ? '—' : `${item.duration_ms} ms`}</td>
                    <td>{fmt(item.started_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
          <div className="subtle-note">
            当前已接入目录中的真实模型，仅支持文本 Chat
            Completions。价格计费、其他协议和生产高可用独立验收。
          </div>
        </>
      )}
    </div>
  );
}
