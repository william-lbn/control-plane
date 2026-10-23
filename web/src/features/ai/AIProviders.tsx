import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../../api';
import { Empty, fmt, PageHeading, status } from '../../shared/ui';
import { newRequestKey } from '../../shared/requestKey';
import { protocolNames } from './protocols';
import type { AIPrice } from './protocols';

export type ProviderCapabilities = {
  enabled: boolean;
  can_manage: boolean;
  raw_secret_input: boolean;
  lab_secret_input: boolean;
  credential_sources: { id: string; name: string; allowed_base_urls: string[] }[];
};
type Provider = {
  id: string;
  name: string;
  version: number;
  state: string;
  active_publication_id: string | null;
  target_revision_id: string | null;
};
type Revision = {
  id: string;
  base_url: string;
  provider_type: string;
  credential_mode: string;
  credential_source_id: string | null;
  has_credential: boolean;
  state: string;
  models: string[] | null;
  operation_id: string;
  operation_state: string;
  attempts: number;
  retryable: boolean;
  error_code: string | null;
};
type Model = {
  id: string;
  name: string;
  upstream_model: string;
  max_output_tokens: number;
  protocols?: string[];
  price?: AIPrice;
  input_modalities?: string[];
};
type Detail = {
  provider: Provider;
  revisions: Revision[];
  publications: { id: string; revision_id: string; models: Model[]; created_at: string }[];
};
type Pending = { path: string; method: string; body: string; key: string };
const inputLabels: Record<string, string> = {
  text: '文本',
  image: '图片',
  pdf: 'PDF',
  audio: '音频',
  video: '视频',
};
function allowedInputTypes(protocols: string[] = ['chat_completions']) {
  const result = ['text'];
  if (protocols.some((v) => v !== 'embeddings')) result.push('image', 'pdf');
  if (protocols.includes('chat_completions') || protocols.includes('gemini')) result.push('audio');
  if (protocols.includes('gemini')) result.push('video');
  return result;
}
function modelInputTypes(model: Model) {
  return (
    model.input_modalities ||
    allowedInputTypes(model.protocols).filter((v) => v === 'text' || v === 'image')
  );
}
const blankModel = (kind = 'openai_compatible'): Model => ({
  id: '',
  name: '',
  upstream_model: '',
  max_output_tokens: 256,
  ...(kind === 'anthropic'
    ? { protocols: ['messages'] }
    : kind === 'gemini'
      ? { protocols: ['gemini'] }
      : {}),
});

export function AIProviders({
  capabilities,
  organizationId,
}: {
  capabilities: ProviderCapabilities | null;
  organizationId?: string;
}) {
  const prefix = organizationId
    ? `/api/v1/organizations/${encodeURIComponent(organizationId)}/ai-providers`
    : '/api/v1/platform/ai-providers';
  const [items, setItems] = useState<Provider[]>([]);
  const [selected, setSelected] = useState('');
  const [detail, setDetail] = useState<Detail | null>(null);
  const [name, setName] = useState('');
  const [base, setBase] = useState('');
  const [providerType, setProviderType] = useState('openai_compatible');
  const [mode, setMode] = useState(organizationId ? 'sealed' : 'operator_reference');
  const [source, setSource] = useState('');
  const [reuse, setReuse] = useState('');
  const [secret, setSecret] = useState('');
  const [labAck, setLabAck] = useState(false);
  const [models, setModels] = useState<Model[]>([blankModel()]);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  // A lost response can only replay this exact in-memory submission. Provider
  // keys never enter browser storage, URLs, logs, traces or read API responses.
  const pending = useRef<Pending | null>(null);
  const activeSelection = useRef('');
  useEffect(
    () => () => {
      pending.current = null;
    },
    [],
  );
  const canManage = !!capabilities?.can_manage;
  const target = detail?.revisions.find((r) => r.id === detail.provider.target_revision_id);
  const publication = detail?.publications.find(
    (p) => p.id === detail.provider.active_publication_id,
  );
  const testing = target?.operation_state === 'queued' || target?.operation_state === 'running';
  async function refresh(id = activeSelection.current) {
    const result = await api<{ items: Provider[] }>(prefix);
    setItems(result.items);
    if (id) {
      const value = await api<Detail>(`${prefix}/${encodeURIComponent(id)}`);
      if (activeSelection.current === id) setDetail(value);
    }
  }
  useEffect(() => {
    if (canManage) refresh().catch((e) => setError(String(e)));
  }, [canManage]);
  useEffect(() => {
    if (!testing || !selected) return;
    const timer = setInterval(() => refresh(selected).catch((e) => setError(String(e))), 2000);
    return () => clearInterval(timer);
  }, [selected, testing]);
  async function choose(id: string) {
    if (busy || uncertain) return;
    activeSelection.current = id;
    setSelected(id);
    setDetail(null);
    setError('');
    setNotice('');
    setSecret('');
    setLabAck(false);
    setModels([blankModel()]);
    if (!id) {
      setName('');
      setBase('');
      setProviderType('openai_compatible');
      setMode(organizationId ? 'sealed' : 'operator_reference');
      setReuse('');
      setSource('');
      return;
    }
    try {
      const value = await api<Detail>(`${prefix}/${encodeURIComponent(id)}`);
      if (activeSelection.current !== id) return;
      setDetail(value);
      setName(value.provider.name);
      const current = value.revisions.find((r) => r.id === value.provider.target_revision_id);
      setBase(current?.base_url || '');
      setProviderType(current?.provider_type || 'openai_compatible');
      const tested = value.revisions.find((r) => r.state === 'tested' && r.has_credential);
      setReuse(tested?.id || '');
      setSource(current?.credential_source_id || '');
      setMode(
        tested
          ? 'reuse'
          : organizationId
            ? 'sealed'
            : current?.credential_source_id
              ? 'operator_reference'
              : 'none',
      );
      const published = value.publications.find(
        (p) => p.id === value.provider.active_publication_id,
      );
      setModels(
        published?.models.map((m) => ({ ...m })) || [
          {
            ...blankModel(),
            ...(current?.provider_type === 'anthropic'
              ? { protocols: ['messages'] }
              : current?.provider_type === 'gemini'
                ? { protocols: ['gemini'] }
                : {}),
          },
        ],
      );
    } catch (e) {
      setError(String(e));
    }
  }
  async function send(request: Pending) {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const receipt = await api<{
        provider_id?: string;
        operation_id?: string;
        revision_id?: string;
        state?: string;
      }>(request.path, {
        method: request.method,
        body: request.body,
        headers: { 'Idempotency-Key': request.key },
      });
      pending.current = null;
      setUncertain(false);
      if (receipt.state === 'deleted') {
        activeSelection.current = '';
        setSelected('');
        setDetail(null);
        setName('');
        setBase('');
        setReuse('');
        await refresh('');
      } else {
        const id = receipt.provider_id || selected;
        activeSelection.current = id;
        setSelected(id);
        await refresh(id);
        if (receipt.revision_id && !receipt.state) setReuse(receipt.revision_id);
      }
      setNotice(
        receipt.operation_id ? `已受理模型测试 · ${receipt.operation_id}` : '目录操作已保存',
      );
    } catch (e) {
      // A 5xx, malformed success or network failure may follow a committed
      // mutation. Keep the original key/body until the operator replays it.
      const definitive = e instanceof ApiError && e.status >= 400 && e.status < 500;
      if (definitive) {
        pending.current = null;
        setUncertain(false);
      } else {
        pending.current = request;
        setUncertain(true);
      }
      setError(e instanceof ApiError ? `${e.message} · ${e.code}` : String(e));
    } finally {
      setBusy(false);
    }
  }
  async function mutate(path: string, body: unknown, method = 'POST') {
    if (busy || uncertain) return;
    const request = { path, method, body: JSON.stringify(body), key: newRequestKey() };
    pending.current = request;
    await send(request);
  }
  async function save(event: React.FormEvent) {
    event.preventDefault();
    const credential = {
      mode,
      ...(mode === 'sealed' ? { secret, lab_secret_acknowledged: labAck } : {}),
      ...(mode === 'operator_reference' ? { source_id: source } : {}),
      ...(mode === 'reuse' ? { reuse_revision_id: reuse } : {}),
    };
    setSecret('');
    await mutate(selected ? `${prefix}/${selected}/revisions` : prefix, {
      version: detail?.provider.version || 0,
      name,
      base_url: base,
      ...(providerType !== 'openai_compatible' ? { provider_type: providerType } : {}),
      credential,
    });
  }
  function updateModel(index: number, patch: Partial<Model>) {
    setModels((values) =>
      values.map((value, at) => (at === index ? { ...value, ...patch } : value)),
    );
  }
  if (!capabilities) return <Empty title="正在读取平台权限" message="请稍候。" />;
  if (!canManage)
    return (
      <Empty
        title={organizationId ? '组织 BYOK 未启用' : '需要平台操作员权限'}
        message={
          organizationId
            ? '需要组织管理员会话及运营方启用组织 BYOK。'
            : '仅明确获准的平台操作员可以管理模型供应商。'
        }
      />
    );
  return (
    <>
      <PageHeading
        kicker={organizationId ? 'ORGANIZATION / AI' : 'PLATFORM / AI'}
        title={organizationId ? '组织自有 AI 供应商' : 'AI 供应商'}
        description={
          organizationId
            ? '使用本组织供应商账户；模型仅供本组织分支使用，供应商费用不扣平台额度，仍受分支预算限制。'
            : '测试连接和模型列表，选择公开别名后发布到各分支的 AI Gateway。'
        }
        action={
          <button disabled={busy || uncertain} onClick={() => choose('')}>
            ＋ 新建供应商
          </button>
        }
      />
      {error && (
        <div role="alert" className="subtle-note">
          {error}
        </div>
      )}
      {notice && (
        <div role="status" className="subtle-note">
          {notice}
        </div>
      )}
      {uncertain && (
        <section className="panel">
          <p>提交结果尚未确认。保留了本次请求，可用原键查询并恢复受理结果。</p>
          <button disabled={busy} onClick={() => pending.current && send(pending.current)}>
            重试相同提交
          </button>
        </section>
      )}
      <section className="panel provider-list">
        <label>
          供应商定义
          <select
            aria-label="供应商定义"
            value={selected}
            disabled={busy || uncertain}
            onChange={(e) => choose(e.target.value)}
          >
            <option value="">新建供应商</option>
            {items.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} · {p.state}
              </option>
            ))}
          </select>
        </label>
        <button disabled={busy} onClick={() => refresh().catch((e) => setError(String(e)))}>
          刷新目录
        </button>
      </section>
      {selected && !detail ? (
        <p>正在读取供应商…</p>
      ) : (
        <>
          <section className="panel provider-editor">
            <div className="panel-heading">
              <h2>{selected ? '修改供应商并测试' : '新建供应商并测试'}</h2>
              {detail && status(detail.provider.state)}
            </div>
            <p>测试只读取模型元数据，不产生推理请求。新版本未发布前，已发布版本继续服务。</p>
            <form onSubmit={save}>
              <fieldset disabled={busy || uncertain || !!testing}>
                <label>
                  供应商名称
                  <input
                    aria-label="供应商名称"
                    required
                    maxLength={100}
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                  />
                </label>
                <label>
                  供应商类型
                  <select
                    aria-label="供应商类型"
                    value={providerType}
                    onChange={(e) => {
                      const kind = e.target.value;
                      setProviderType(kind);
                      setMode(kind === 'openai_compatible' ? 'none' : 'operator_reference');
                      setSecret('');
                      setLabAck(false);
                      setReuse('');
                      setModels([blankModel(kind)]);
                    }}
                  >
                    <option value="openai_compatible">OpenAI compatible</option>
                    <option value="anthropic">Anthropic Messages</option>
                    <option value="gemini">Google Gemini</option>
                  </select>
                </label>
                <p>API 根地址统一使用 /v1；系统按供应商类型读取原生模型目录，不发起推理。</p>
                <label>
                  兼容接口地址
                  <input
                    aria-label="兼容接口地址"
                    type="url"
                    required
                    maxLength={2048}
                    placeholder="https://api.example.com/v1"
                    value={base}
                    onChange={(e) => setBase(e.target.value)}
                  />
                </label>
                <label>
                  认证方式
                  <select
                    aria-label="供应商认证方式"
                    value={mode}
                    onChange={(e) => {
                      setMode(e.target.value);
                      setSecret('');
                      setLabAck(false);
                    }}
                  >
                    {!organizationId && (
                      <option value="operator_reference">使用平台已批准的凭据</option>
                    )}
                    <option
                      value="none"
                      disabled={!!organizationId || providerType !== 'openai_compatible'}
                    >
                      无需认证
                    </option>
                    <option
                      value="sealed"
                      disabled={!capabilities.raw_secret_input && !capabilities.lab_secret_input}
                    >
                      输入新密钥
                    </option>
                    {selected && <option value="reuse">复用同地址已测试凭据</option>}
                  </select>
                </label>
                {mode === 'operator_reference' && (
                  <label>
                    已批准凭据
                    <select
                      required
                      aria-label="已批准凭据"
                      value={source}
                      onChange={(e) => setSource(e.target.value)}
                    >
                      <option value="">请选择</option>
                      {capabilities.credential_sources
                        .filter((s) => s.allowed_base_urls.includes(base.replace(/\/$/, '')))
                        .map((s) => (
                          <option key={s.id} value={s.id}>
                            {s.name}
                          </option>
                        ))}
                    </select>
                  </label>
                )}
                {mode === 'reuse' && (
                  <label>
                    凭据来源版本
                    <select
                      required
                      aria-label="凭据来源版本"
                      value={reuse}
                      onChange={(e) => setReuse(e.target.value)}
                    >
                      <option value="">请选择已测试版本</option>
                      {detail?.revisions
                        .filter(
                          (r) =>
                            r.state === 'tested' &&
                            r.has_credential &&
                            r.base_url === base.replace(/\/$/, ''),
                        )
                        .map((r) => (
                          <option key={r.id} value={r.id}>
                            {r.id}
                          </option>
                        ))}
                    </select>
                  </label>
                )}
                {mode === 'sealed' && (
                  <>
                    <label>
                      新供应商密钥
                      <input
                        aria-label="新供应商密钥"
                        type="password"
                        autoComplete="off"
                        spellCheck={false}
                        required
                        minLength={16}
                        maxLength={8192}
                        value={secret}
                        onChange={(e) => setSecret(e.target.value)}
                      />
                    </label>
                    <p>密钥只在本次提交中使用，保存后不回显。</p>
                    {!capabilities.raw_secret_input && capabilities.lab_secret_input && (
                      <label className="provider-checkbox">
                        <input
                          type="checkbox"
                          required
                          checked={labAck}
                          onChange={(e) => setLabAck(e.target.checked)}
                        />
                        我确认这是已批准内网实验接口的专用测试凭据
                      </label>
                    )}
                  </>
                )}
                <button className="button primary" type="submit">
                  保存并测试模型
                </button>
              </fieldset>
            </form>
          </section>
          {target && (
            <section className="panel provider-editor" data-testid="provider-test-result">
              <div className="panel-heading">
                <h2>当前草稿测试</h2>
                {status(target.operation_state)}
              </div>
              <p>
                <code>{target.id}</code> · 原操作 <code>{target.operation_id}</code> · 第{' '}
                {target.attempts} 次
              </p>
              <p>
                {target.state === 'tested'
                  ? `已发现 ${target.models?.length || 0} 个模型；需选择模型、明确协议后发布。模型列表不代表推理健康检查。`
                  : testing
                    ? '正在读取模型列表…'
                    : `测试失败：${target.error_code || '请刷新原操作'}`}
              </p>
              {target.error_code && (
                <code data-testid="provider-test-error">{target.error_code}</code>
              )}
              {target.operation_state === 'failed' && target.retryable && (
                <button
                  disabled={busy || uncertain}
                  onClick={() =>
                    mutate(
                      `${organizationId ? `/api/v1/organizations/${encodeURIComponent(organizationId)}` : '/api/v1/platform'}/ai-provider-operations/${target.operation_id}/retry`,
                      {},
                    )
                  }
                >
                  重试原模型测试
                </button>
              )}
            </section>
          )}
          {target?.state === 'tested' && (
            <section className="panel provider-editor">
              <h2>发布模型别名</h2>
              <p>选择已发现的模型；公开别名在整个目录中必须唯一。模型能力由供应商提供。</p>
              <fieldset disabled={busy || uncertain}>
                {models.map((model, index) => (
                  <div className="provider-model-row" key={index}>
                    <label>
                      上游模型
                      <select
                        aria-label={`上游模型 ${index + 1}`}
                        required
                        value={model.upstream_model}
                        onChange={(e) => updateModel(index, { upstream_model: e.target.value })}
                      >
                        <option value="">请选择</option>
                        {target.models?.map((id) => (
                          <option key={id} value={id}>
                            {id}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      公开别名
                      <input
                        aria-label={`公开别名 ${index + 1}`}
                        required
                        maxLength={128}
                        value={model.id}
                        onChange={(e) => updateModel(index, { id: e.target.value })}
                      />
                    </label>
                    <label>
                      模型名称
                      <input
                        aria-label={`模型名称 ${index + 1}`}
                        required
                        maxLength={100}
                        value={model.name}
                        onChange={(e) => updateModel(index, { name: e.target.value })}
                      />
                    </label>
                    <label>
                      最大输出 Token
                      <input
                        aria-label={`最大输出 Token ${index + 1}`}
                        type="number"
                        min={1}
                        max={4096}
                        required
                        value={model.max_output_tokens}
                        onChange={(e) =>
                          updateModel(index, { max_output_tokens: Number(e.target.value) })
                        }
                      />
                    </label>
                    <fieldset>
                      <legend>已批准的协议 {index + 1}</legend>
                      {Object.entries(protocolNames)
                        .filter(
                          ([p]) =>
                            (target?.provider_type || providerType) === 'openai_compatible' ||
                            p ===
                              ((target?.provider_type || providerType) === 'anthropic'
                                ? 'messages'
                                : 'gemini'),
                        )
                        .map(([p, label]) => (
                          <label key={p}>
                            <input
                              type="checkbox"
                              aria-label={`协议 ${label} ${index + 1}`}
                              disabled={
                                (target?.provider_type || providerType) !== 'openai_compatible'
                              }
                              checked={(model.protocols || ['chat_completions']).includes(p)}
                              onChange={(e) => {
                                const current = model.protocols || ['chat_completions'];
                                const next = e.target.checked
                                  ? [...current, p]
                                  : current.filter((v) => v !== p);
                                updateModel(index, {
                                  protocols: next.length ? next : ['chat_completions'],
                                  input_modalities: modelInputTypes(model).filter((v) =>
                                    allowedInputTypes(
                                      next.length ? next : ['chat_completions'],
                                    ).includes(v),
                                  ),
                                });
                              }}
                            />
                            {label}
                          </label>
                        ))}
                    </fieldset>
                    <fieldset>
                      <legend>已批准的输入类型 {index + 1}</legend>
                      {allowedInputTypes(model.protocols).map((kind) => (
                        <label key={kind}>
                          <input
                            type="checkbox"
                            aria-label={`输入 ${inputLabels[kind]} ${index + 1}`}
                            disabled={kind === 'text'}
                            checked={modelInputTypes(model).includes(kind)}
                            onChange={(e) => {
                              const current = modelInputTypes(model);
                              updateModel(index, {
                                input_modalities: e.target.checked
                                  ? [...current, kind]
                                  : current.filter((v) => v !== kind),
                              });
                            }}
                          />
                          {inputLabels[kind]}
                        </label>
                      ))}
                    </fieldset>
                    <label>
                      <input
                        aria-label={`显式计价 ${index + 1}`}
                        type="checkbox"
                        checked={Boolean(model.price)}
                        onChange={(e) =>
                          updateModel(index, {
                            price: e.target.checked
                              ? {
                                  version: 'manual-v1',
                                  currency: 'USD',
                                  max_input_tokens: 32000,
                                  input_microusd_per_million_tokens: 0,
                                  output_microusd_per_million_tokens: 0,
                                  cache_read_microusd_per_million_tokens: 0,
                                  cache_write_microusd_per_million_tokens: 0,
                                }
                              : undefined,
                          })
                        }
                      />
                      显式计价（含免费模型；未配置时不能使用金额预算）
                    </label>
                    {model.price && (
                      <div>
                        <label>
                          价格版本
                          <input
                            aria-label={`价格版本 ${index + 1}`}
                            value={model.price.version}
                            maxLength={128}
                            onChange={(e) =>
                              updateModel(index, {
                                price: { ...model.price!, version: e.target.value },
                              })
                            }
                          />
                        </label>
                        <label>
                          输入 Token 上界
                          <input
                            aria-label={`输入 Token 上界 ${index + 1}`}
                            type="number"
                            min={1}
                            max={1000000}
                            value={model.price.max_input_tokens}
                            onChange={(e) =>
                              updateModel(index, {
                                price: {
                                  ...model.price!,
                                  max_input_tokens: Number(e.target.value),
                                },
                              })
                            }
                          />
                        </label>
                        {(
                          [
                            ['input_microusd_per_million_tokens', '输入'],
                            ['output_microusd_per_million_tokens', '输出'],
                            ['cache_read_microusd_per_million_tokens', '缓存读取'],
                            ['cache_write_microusd_per_million_tokens', '缓存写入'],
                          ] as const
                        ).map(([field, label]) => (
                          <label key={field}>
                            {label}费率（微美元 / 百万 Token）
                            <input
                              aria-label={`${label}费率 ${index + 1}`}
                              type="number"
                              min={0}
                              max={1000000000}
                              value={model.price![field]}
                              onChange={(e) =>
                                updateModel(index, {
                                  price: { ...model.price!, [field]: Number(e.target.value) },
                                })
                              }
                            />
                          </label>
                        ))}
                      </div>
                    )}
                    {models.length > 1 && (
                      <button
                        onClick={() => setModels((rows) => rows.filter((_, at) => at !== index))}
                      >
                        移除别名 {index + 1}
                      </button>
                    )}
                  </div>
                ))}
                <button
                  disabled={models.length >= 16}
                  onClick={() =>
                    setModels((values) => [
                      ...values,
                      blankModel(target?.provider_type || providerType),
                    ])
                  }
                >
                  增加模型别名
                </button>
                <button
                  className="button primary"
                  disabled={
                    !models.length ||
                    models.some(
                      (m) =>
                        !m.id ||
                        !m.name ||
                        !m.upstream_model ||
                        !Number.isInteger(m.max_output_tokens) ||
                        m.max_output_tokens < 1 ||
                        m.max_output_tokens > 4096,
                    )
                  }
                  onClick={() =>
                    mutate(`${prefix}/${selected}/publish`, {
                      version: detail!.provider.version,
                      revision_id: target.id,
                      models,
                    })
                  }
                >
                  发布当前测试版本
                </button>
              </fieldset>
            </section>
          )}
          {detail && (
            <section className="panel provider-editor">
              <h2>已发布版本</h2>
              {publication ? (
                <>
                  <p>
                    <code>{publication.revision_id}</code> · {fmt(publication.created_at)}
                  </p>
                  <p>
                    {detail.provider.state === 'active'
                      ? '正在供新请求使用'
                      : '已停止接收新请求；历史记录保留'}
                  </p>
                  <ul>
                    {publication.models.map((m) => (
                      <li key={m.id}>
                        <code>{m.id}</code> → {m.upstream_model} · 上限 {m.max_output_tokens}
                      </li>
                    ))}
                  </ul>
                </>
              ) : (
                <p>尚未发布模型。</p>
              )}
              {detail.provider.state === 'active' ? (
                <button
                  disabled={busy || uncertain}
                  onClick={() =>
                    mutate(`${prefix}/${selected}/disable`, { version: detail.provider.version })
                  }
                >
                  停用供应商
                </button>
              ) : (
                <button
                  disabled={busy || uncertain || !!testing}
                  onClick={() =>
                    mutate(`${prefix}/${selected}`, { version: detail.provider.version }, 'DELETE')
                  }
                >
                  删除定义并保留历史
                </button>
              )}
              <details>
                <summary>版本与发布历史</summary>
                <ul>
                  {detail.revisions.map((r) => (
                    <li key={r.id}>
                      <code>{r.id}</code> · {r.state} · {r.base_url} ·{' '}
                      {r.has_credential ? '已保存凭据' : '无需认证'}
                    </li>
                  ))}
                </ul>
              </details>
            </section>
          )}
        </>
      )}
    </>
  );
}
