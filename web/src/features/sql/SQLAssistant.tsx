import { useEffect, useMemo, useRef, useState } from 'react';
import { api, projectPath } from '../../api';
import {
  assistantResponseText,
  buildAssistantRequest,
  parseSQLDraft,
  type AssistantModel,
  type AssistantPurpose,
  type SQLDraft,
} from './sqlAssistant';
import './sql-assistant.css';

type Catalog = { enabled: boolean; models: AssistantModel[] };
type Plan = { rows: unknown[][]; analyzed: boolean } | null;

async function responseJSON(response: Response) {
  const reader = response.body?.getReader();
  if (!reader) throw new Error('模型响应体不可用。');
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let bytes = 0,
    text = '';
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      bytes += part.value.length;
      if (bytes > 65536) throw new Error('模型响应超过界限，不能载入。');
      text += decoder.decode(part.value, { stream: true });
    }
    text += decoder.decode();
    return JSON.parse(text) as unknown;
  } finally {
    await reader.cancel().catch(() => {});
  }
}

// Connection passwords and ordinary query results are absent from these props.
// All generation uses the existing Go session/CSRF/branch AI endpoint. SQL is
// only loaded on a separate user click; this component has no query executor.
export function SQLAssistant({
  projectId,
  branchId,
  currentSQL,
  plan,
  disabled,
  onLoad,
  canLoadSchema,
  onLoadSchema,
}: {
  projectId: string;
  branchId: string;
  currentSQL: string;
  plan: Plan;
  disabled: boolean;
  onLoad: (sql: string) => void;
  canLoadSchema: boolean;
  onLoadSchema: () => Promise<string>;
}) {
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [model, setModel] = useState('');
  const [purpose, setPurpose] = useState<AssistantPurpose>('sql_draft');
  const [prompt, setPrompt] = useState('');
  const [schema, setSchema] = useState('');
  const [includeSQL, setIncludeSQL] = useState(false);
  const [consent, setConsent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [raw, setRaw] = useState('');
  const [draft, setDraft] = useState<SQLDraft | null>(null);
  const [failure, setFailure] = useState('');
  const [notice, setNotice] = useState('');
  const [requestId, setRequestId] = useState('');
  const [schemaBusy, setSchemaBusy] = useState(false);
  const [schemaNotice, setSchemaNotice] = useState('');
  const mounted = useRef(true);
  const epoch = useRef(0);
  const active = useRef<AbortController | null>(null);
  const prefix = `${projectPath(projectId)}/branches/${encodeURIComponent(branchId)}/ai-gateway`;
  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    let live = true;
    void api<Catalog>(prefix, { signal: controller.signal })
      .then((value) => {
        if (!live) return;
        setCatalog(value);
        setModel(value.models.find((m) => m.protocols.includes('chat_completions'))?.id || '');
      })
      .catch(() => {
        if (live) setFailure('模型目录暂不可用，可继续使用普通 SQL 工作台。');
      });
    return () => {
      live = false;
      mounted.current = false;
      controller.abort();
      epoch.current++;
      active.current?.abort();
    };
  }, [prefix]);
  async function loadSchema() {
    if (!canLoadSchema || disabled || busy || schemaBusy) return;
    setSchemaBusy(true);
    setFailure('');
    try {
      const value = await onLoadSchema();
      if (mounted.current) {
        setSchema(value);
        setConsent(false);
        setSchemaNotice('已读取当前角色可见的 public 列元数据。请在发送前检查摘要与截断标记。');
      }
    } catch (error) {
      if (mounted.current) setFailure(error instanceof Error ? error.message : 'Schema 读取失败。');
    } finally {
      if (mounted.current) setSchemaBusy(false);
    }
  }
  const selected = catalog?.models.find((m) => m.id === model);
  const preview = useMemo(() => {
    try {
      if (!selected) return { request: null, error: '请选择已配置的文本模型。' };
      return {
        request: buildAssistantRequest(
          { purpose, prompt, schema, includeSQL, currentSQL, plan },
          selected,
        ),
        error: '',
      };
    } catch (error) {
      return { request: null, error: error instanceof Error ? error.message : '上下文无效。' };
    }
  }, [selected, purpose, prompt, schema, includeSQL, currentSQL, plan]);
  const fingerprint = preview.request
    ? JSON.stringify(preview.request)
    : JSON.stringify([purpose, model, prompt, schema, includeSQL, preview.error]);
  useEffect(() => {
    epoch.current++;
    active.current?.abort();
    active.current = null;
    setBusy(false);
    setConsent(false);
    setRaw('');
    setDraft(null);
    setRequestId('');
    setNotice('');
    setFailure('');
  }, [fingerprint]);
  async function generate() {
    if (!preview.request || !consent || busy || disabled || !catalog?.enabled) return;
    const current = ++epoch.current;
    const controller = new AbortController();
    active.current = controller;
    const requestedPurpose = purpose;
    setBusy(true);
    setFailure('');
    setNotice('');
    setRaw('');
    setDraft(null);
    setRequestId('');
    try {
      const token =
        document.cookie
          .split(';')
          .map((v) => v.trim())
          .find((v) => v.startsWith('neon_v2_csrf='))
          ?.slice('neon_v2_csrf='.length) || '';
      const response = await fetch(prefix + '/request', {
        method: 'POST',
        credentials: 'same-origin',
        signal: controller.signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': decodeURIComponent(token) },
        body: JSON.stringify(preview.request),
      });
      if (current !== epoch.current) return;
      setRequestId(response.headers.get('X-Request-ID') || '');
      const value = await responseJSON(response);
      if (!response.ok) {
        const body =
          value && typeof value === 'object'
            ? (value as { code?: string; error?: { code?: string } })
            : null;
        throw new Error(body?.code || body?.error?.code || `模型请求失败 (${response.status})`);
      }
      const text = assistantResponseText(value);
      if (current !== epoch.current) return;
      setRaw(text);
      if (requestedPurpose === 'sql_draft') setDraft(parseSQLDraft(text));
      setNotice(
        requestedPurpose === 'sql_draft'
          ? '已生成未验证草稿。审阅后可显式载入编辑器。'
          : '已返回模型建议。请结合实际数据和多次测量人工验证。',
      );
    } catch (error) {
      if (current === epoch.current && !controller.signal.aborted)
        setFailure(error instanceof Error ? error.message : '建议结果未确认，不会自动重试。');
    } finally {
      if (current === epoch.current) {
        active.current = null;
        setBusy(false);
        setConsent(false);
      }
    }
  }
  function cancel() {
    epoch.current++;
    active.current?.abort();
    active.current = null;
    setBusy(false);
    setConsent(false);
    setFailure('已取消等待。供应商可能已经处理请求，请查看 AI Gateway 用量；不会自动重试。');
  }
  return (
    <section className="panel sql-assistant" data-testid="sql-assistant">
      <div className="panel-heading">
        <div>
          <small>AI / HUMAN REVIEW</small>
          <h2>SQL AI 助手</h2>
        </div>
        <span className="status">预览</span>
      </div>
      <p className="muted">
        生成 PostgreSQL 草稿或解读最近一次计划。不会读取连接密码或普通查询结果；生成与载入都不会提交
        SQL。选择附带 SQL/Schema 时，请先检查敏感字面量。
      </p>
      {catalog && !catalog.enabled && (
        <p role="status">AI Gateway 尚未配置；SQL 工作台可继续使用。</p>
      )}
      <div className="sql-assistant-grid">
        <div>
          <div className="selector-row">
            <label>
              建议类型
              <select
                aria-label="AI 建议类型"
                value={purpose}
                disabled={busy || disabled}
                onChange={(e) => setPurpose(e.target.value as AssistantPurpose)}
              >
                <option value="sql_draft">SQL 草稿</option>
                <option value="plan_advice" disabled={!plan}>
                  计划建议 · 需当前计划
                </option>
              </select>
            </label>
            <label>
              模型
              <select
                aria-label="SQL 助手模型"
                value={model}
                disabled={busy || disabled || !catalog?.enabled}
                onChange={(e) => setModel(e.target.value)}
              >
                <option value="">选择模型</option>
                {catalog?.models
                  .filter((m) => m.protocols.includes('chat_completions'))
                  .map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.name}
                    </option>
                  ))}
              </select>
            </label>
          </div>
          <label>
            请求说明
            <textarea
              aria-label="AI 请求说明"
              rows={3}
              value={prompt}
              disabled={busy || disabled}
              onChange={(e) => setPrompt(e.target.value)}
              placeholder="说明要生成的 SQL 或希望检查的计划问题。"
            />
          </label>
          <details>
            <summary>可选 Schema 上下文 · 受控读取或手工提供</summary>
            <p className="muted">
              点击读取时才通过当前角色访问 public 信息目录；最多 60 列和 4096
              字节，不读取数据行/默认值/注释。此操作可能唤醒
              Compute，并清除本次连接密码。发送到模型仍需另行确认。
            </p>
            <button
              className="button ghost"
              disabled={!canLoadSchema || disabled || busy || schemaBusy}
              onClick={() => void loadSchema()}
            >
              加载 public Schema
            </button>
            {schemaNotice && (
              <p role="status" data-testid="sql-assistant-schema-notice">
                {schemaNotice}
              </p>
            )}
            <label>
              Schema 元数据
              <textarea
                aria-label="AI Schema 上下文"
                rows={3}
                value={schema}
                disabled={busy || disabled}
                onChange={(e) => {
                  setSchema(e.target.value);
                  setSchemaNotice('');
                }}
                placeholder="只填写允许发给模型的表/列/类型或 DDL；不要放数据行和密钥。"
              />
            </label>
          </details>
          <label className="sql-assistant-check">
            <input
              type="checkbox"
              aria-label="附带当前 SQL"
              checked={includeSQL}
              disabled={busy || disabled}
              onChange={(e) => setIncludeSQL(e.target.checked)}
            />
            附带当前编辑器 SQL（默认不发送）
          </label>
          {purpose === 'plan_advice' && (
            <p className="muted">
              仅发送有界运算符和数值指标摘要，默认省略关系/索引名、条件、输出表达式和原始 SQL。
            </p>
          )}
          <details open>
            <summary>实际发送内容预览</summary>
            <textarea
              className="sql-assistant-preview"
              aria-label="发给模型的内容"
              readOnly
              value={preview.request ? JSON.stringify(preview.request, null, 2) : preview.error}
              rows={8}
            />
          </details>
          <label className="sql-assistant-check">
            <input
              type="checkbox"
              aria-label="确认发送模型上下文"
              checked={consent}
              disabled={busy || disabled || !preview.request || !catalog?.enabled}
              onChange={(e) => setConsent(e.target.checked)}
            />
            我确认将上方预览内容发给所选模型服务。
          </label>
          <div className="query-actions">
            <button
              className="button primary"
              disabled={busy || disabled || !consent || !preview.request || !catalog?.enabled}
              onClick={() => void generate()}
            >
              {purpose === 'sql_draft' ? '生成 SQL 草稿' : '获取计划建议'}
            </button>
            <button className="button ghost" disabled={!busy} onClick={cancel}>
              取消建议请求
            </button>
          </div>
        </div>
        <div className="sql-assistant-output">
          <h3>{purpose === 'sql_draft' ? '待审阅草稿' : '模型计划建议'}</h3>
          {!raw && !busy && (
            <p className="muted">
              明确确认发送内容后，在这里查看结果。模型建议尚未经过数据库验证。
            </p>
          )}
          {busy && <p role="status">正在请求模型；不会自动重试或执行 SQL。</p>}
          {failure && (
            <p role="alert" data-testid="sql-assistant-error">
              {failure}
            </p>
          )}
          {notice && (
            <p role="status" data-testid="sql-assistant-notice">
              {notice}
            </p>
          )}
          {requestId && <small data-testid="sql-assistant-request-id">请求 {requestId}</small>}
          {draft && (
            <>
              <textarea aria-label="AI SQL 草稿" rows={8} readOnly value={draft.sql} />
              <p>{draft.explanation}</p>
              {draft.warnings.length > 0 && (
                <ul>
                  {draft.warnings.map((warning, i) => (
                    <li key={i}>{warning}</li>
                  ))}
                </ul>
              )}
              <button
                className="button"
                disabled={busy || disabled}
                onClick={() => {
                  onLoad(draft.sql);
                  setNotice('已载入编辑器。请检查语句、角色和目标数据库，输入密码后人工提交。');
                }}
              >
                载入 SQL 编辑器
              </button>
            </>
          )}
          {raw && purpose === 'plan_advice' && (
            <pre data-testid="sql-assistant-plan-advice">{raw}</pre>
          )}
          {raw && purpose === 'sql_draft' && (
            <details>
              <summary>原始模型输出</summary>
              <pre data-testid="sql-assistant-raw">{raw}</pre>
            </details>
          )}
        </div>
      </div>
    </section>
  );
}
