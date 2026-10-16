import { useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError, projectPath } from '../../api';
import { newRequestKey } from '../../shared/requestKey';
import { assistantResponseText, type AssistantModel } from './sqlAssistant';
import { consoleCSRF, responseJSON } from './aiResponse';
import { buildHistoryNameRequest, parseHistoryName, validHistoryName } from './historyName';
import type { Entry } from './QueryHistory';

export type HistoryName = {
  history_id: string;
  name: string | null;
  version_token: string;
  updated_at: string | null;
};
type Attempt = { key: string; version: string; body: { name: string } };

export function HistoryNamePanel({
  projectId,
  entry,
  onClose,
  onChanged,
}: {
  projectId: string;
  entry: Entry;
  onClose: () => void;
  onChanged: (value: HistoryName) => void;
}) {
  const base = projectPath(projectId) + '/query-history/' + encodeURIComponent(entry.id),
    ai =
      projectPath(projectId) + '/branches/' + encodeURIComponent(entry.branch_id) + '/ai-gateway';
  const [value, setValue] = useState<HistoryName | null>(null),
    [name, setName] = useState(''),
    [busy, setBusy] = useState(true),
    [uncertain, setUncertain] = useState(false),
    [needsReload, setNeedsReload] = useState(false),
    [notice, setNotice] = useState('');
  const [models, setModels] = useState<AssistantModel[]>([]),
    [model, setModel] = useState(''),
    [source, setSource] = useState<{ sql: string; truncated: boolean } | null>(null),
    [consent, setConsent] = useState(false),
    [aiBusy, setAIBusy] = useState(false),
    [draft, setDraft] = useState(''),
    [aiNotice, setAINotice] = useState(''),
    [requestId, setRequestId] = useState('');
  const attempt = useRef<Attempt | null>(null),
    epoch = useRef(0),
    aiEpoch = useRef(0),
    active = useRef<AbortController | null>(null);
  useEffect(() => {
    let live = true;
    epoch.current++;
    setBusy(true);
    void api<HistoryName>(base + '/name')
      .then((v) => {
        if (live) {
          setValue(v);
          setName(v.name || '');
        }
      })
      .catch((e) => {
        if (live) setNotice(e instanceof Error ? e.message : '名称读取失败');
      })
      .finally(() => {
        if (live) setBusy(false);
      });
    void api<{ enabled: boolean; models: AssistantModel[] }>(ai)
      .then((v) => {
        if (live && v.enabled) {
          const choices = v.models.filter((m) => m.protocols.includes('chat_completions'));
          setModels(choices);
          setModel(choices[0]?.id || '');
        }
      })
      .catch(() => {
        if (live) setAINotice('模型目录暂不可用；可继续手工命名。');
      });
    return () => {
      live = false;
      epoch.current++;
      aiEpoch.current++;
      active.current?.abort();
    };
  }, [base, ai]);
  const selected = models.find((m) => m.id === model);
  const preview = useMemo(() => {
    try {
      if (!source || !selected) return { body: null, error: '先明确读取本人历史文本并选择模型。' };
      return { body: buildHistoryNameRequest(source.sql, source.truncated, selected), error: '' };
    } catch (e) {
      return { body: null, error: e instanceof Error ? e.message : '上下文无效' };
    }
  }, [source, selected]);
  const fingerprint = JSON.stringify(preview);
  useEffect(() => {
    aiEpoch.current++;
    active.current?.abort();
    active.current = null;
    setAIBusy(false);
    setConsent(false);
    setDraft('');
    setRequestId('');
  }, [fingerprint]);
  const locked = busy || aiBusy || uncertain || needsReload;
  async function readCurrent() {
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    try {
      const next = await api<HistoryName>(base + '/name');
      if (current !== epoch.current) return;
      setValue(next);
      setName(next.name || '');
      onChanged(next);
      attempt.current = null;
      setUncertain(false);
      setNeedsReload(false);
      setNotice('已读取当前版本；不会执行历史 SQL。');
    } catch (e) {
      if (current === epoch.current) setNotice(e instanceof Error ? e.message : '读取失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function save() {
    if (!value || (!uncertain && !validHistoryName(name))) return;
    const current = epoch.current;
    setBusy(true);
    setNotice('');
    if (!attempt.current)
      attempt.current = {
        key: 'history-name-' + newRequestKey(),
        version: value.version_token,
        body: { name },
      };
    const original = attempt.current;
    try {
      await api(base + '/name', {
        method: 'PUT',
        headers: { 'Idempotency-Key': original.key, 'If-Match': '"' + original.version + '"' },
        body: JSON.stringify(original.body),
      });
      const next = await api<HistoryName>(base + '/name');
      if (current !== epoch.current) return;
      setValue(next);
      setName(next.name || '');
      onChanged(next);
      attempt.current = null;
      setUncertain(false);
      setNeedsReload(false);
      setNotice(
        next.name
          ? '个人名称已保存，数据库与共享历史没有改变。'
          : '个人名称已清除；原执行记录保持。',
      );
    } catch (e) {
      if (current !== epoch.current) return;
      const unknown = !(e instanceof ApiError) || e.status >= 500;
      setUncertain(unknown);
      setNeedsReload(e instanceof ApiError && (e.status === 412 || e.status === 409));
      if (!unknown) attempt.current = null;
      setNotice(
        (e instanceof Error ? e.message : '名称结果未确认') +
          (unknown ? '；重试原提交使用同一 key/版本/内容，或先读取当前名称。' : ''),
      );
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function loadSource() {
    const current = epoch.current;
    setBusy(true);
    setAINotice('');
    try {
      const v = await api<{ sql: string; truncated: boolean }>(base);
      if (current === epoch.current) setSource({ sql: v.sql, truncated: v.truncated });
    } catch (e) {
      if (current === epoch.current)
        setAINotice(e instanceof Error ? e.message : '历史文本读取失败');
    } finally {
      if (current === epoch.current) setBusy(false);
    }
  }
  async function generate() {
    if (!preview.body || !consent || locked) return;
    const current = ++aiEpoch.current,
      scope = epoch.current;
    const controller = new AbortController();
    active.current = controller;
    setAIBusy(true);
    setDraft('');
    setAINotice('');
    setRequestId('');
    try {
      const response = await fetch(ai + '/request', {
        method: 'POST',
        credentials: 'same-origin',
        signal: controller.signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': consoleCSRF() },
        body: JSON.stringify(preview.body),
      });
      if (current !== aiEpoch.current || scope !== epoch.current) return;
      setRequestId(response.headers.get('X-Request-ID') || '');
      const body = await responseJSON(response);
      if (!response.ok) throw new Error(`模型请求失败 (${response.status})，不会自动重试。`);
      const text = assistantResponseText(body);
      if (current !== aiEpoch.current || scope !== epoch.current) return;
      setDraft(parseHistoryName(text));
      setAINotice('未验证名称建议已返回。采用只会填入输入框；保存需要另行点击。');
    } catch (e) {
      if (current === aiEpoch.current && !controller.signal.aborted)
        setAINotice(e instanceof Error ? e.message : '模型结果未确认；不会自动重试。');
    } finally {
      if (current === aiEpoch.current) {
        active.current = null;
        setAIBusy(false);
        setConsent(false);
      }
    }
  }
  function cancel() {
    aiEpoch.current++;
    active.current?.abort();
    active.current = null;
    setAIBusy(false);
    setConsent(false);
    setAINotice('已取消等待；模型可能已处理请求，请核对 AI 用量。不会自动重试。');
  }
  return (
    <section className="panel" aria-label="本人历史名称设置" data-testid="history-name-panel">
      <h3>本人历史名称</h3>
      <code>{entry.id}</code>
      <p>
        个人名称加密保存，不随 SQL 历史共享。不会重放 SQL、撤销执行结果或唤醒 Compute。最多 80
        字符/240 UTF-8 字节，清空输入可删除名称。
      </p>
      {value && (
        <>
          <p data-testid="history-name-current">
            当前名称：{value.name || '未命名'} · 版本 {value.version_token}
          </p>
          <label>
            名称
            <input
              aria-label="本人历史名称"
              value={name}
              disabled={locked}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          {!validHistoryName(name) && (
            <p role="alert">名称需为有界单行文本，不能有前后空白或控制符。</p>
          )}
          <button
            className="button"
            disabled={busy || aiBusy || needsReload || (!uncertain && !validHistoryName(name))}
            onClick={() => void save()}
          >
            {uncertain ? '重试原名称提交' : name ? '保存个人名称' : '清除个人名称'}
          </button>
        </>
      )}
      <button className="button" disabled={busy || aiBusy} onClick={() => void readCurrent()}>
        读取当前名称
      </button>
      <button className="button" disabled={busy || aiBusy || uncertain} onClick={onClose}>
        关闭名称设置
      </button>
      {notice && (
        <p role="status" data-testid="history-name-notice">
          {notice}
        </p>
      )}
      <details>
        <summary>AI 建议名称 · 需单独确认发送历史 SQL</summary>
        <p>
          历史 SQL 可能包含敏感字面量。只发送下方明确选择的完整
          SQL，不读取连接密码、数据库结果或当前编辑器；截断历史请手工命名。模型标题可能不准确，必须审阅。
        </p>
        <button
          className="button"
          disabled={locked || entry.truncated}
          onClick={() => void loadSource()}
        >
          读取这条本人历史 SQL
        </button>
        <label>
          模型
          <select
            aria-label="历史命名模型"
            value={model}
            disabled={locked}
            onChange={(e) => setModel(e.target.value)}
          >
            <option value="">选择模型</option>
            {models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          实际发送内容
          <textarea
            aria-label="历史命名发送内容"
            readOnly
            rows={8}
            value={preview.body ? JSON.stringify(preview.body, null, 2) : preview.error}
          />
        </label>
        <label>
          <input
            type="checkbox"
            aria-label="确认发送这条历史 SQL"
            checked={consent}
            disabled={locked || !preview.body}
            onChange={(e) => setConsent(e.target.checked)}
          />
          我确认将上方这条本人历史 SQL 发给所选模型。
        </label>
        <button
          className="button"
          disabled={locked || !consent || !preview.body}
          onClick={() => void generate()}
        >
          生成历史名称建议
        </button>
        <button className="button" disabled={!aiBusy} onClick={cancel}>
          取消命名建议
        </button>
        {draft && (
          <>
            <pre data-testid="history-name-ai-draft">{draft}</pre>
            <button
              className="button"
              disabled={locked}
              onClick={() => {
                setName(draft);
                setAINotice('已填入名称输入框，尚未保存。');
              }}
            >
              采用名称建议
            </button>
          </>
        )}
        {aiNotice && (
          <p role="status" data-testid="history-name-ai-notice">
            {aiNotice}
          </p>
        )}
        {requestId && <small data-testid="history-name-ai-request">请求 {requestId}</small>}
      </details>
    </section>
  );
}
