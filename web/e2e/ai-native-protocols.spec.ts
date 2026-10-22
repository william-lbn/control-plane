import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';

// This slice checks the installed product against a deterministic protocol
// supplier, and does not claim real Anthropic/Gemini/OpenAI account acceptance.
test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Native AI protocols, scoped SDK credentials and durable spending controls', async ({
  page,
  context,
}, info) => {
  test.setTimeout(300000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !fixtureFile || !adminFile || !privateDir)
    throw Error('Protected Linux AI protocol fixture required');
  const owner = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    !/^prj_[a-f0-9]{16}$/.test(owner.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(owner.branch_id) ||
    !/^ci-native-[a-f0-9]+$/.test(owner.model_id) ||
    owner.test_upstream !== true ||
    !/^http:\/\/10\.43\.[0-9]+\.[0-9]+:9081$/.test(owner.upstream_url)
  )
    throw Error('Explicit owned synthetic upstream fixture required');
  const ai = `/api/v1/projects/${owner.project_id}/branches/${owner.branch_id}/ai-gateway`;
  const prefix = `/api/v1/projects/${owner.project_id}`;
  const state = owner.resume_file
    ? JSON.parse(await readFile(owner.resume_file, 'utf8'))
    : {
        result: 'running',
        checks: [] as string[],
        request_ids: [] as string[],
        credential_id: '',
        token: '',
      };
  state.result = 'running';
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'ai-native-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result: state.result,
        checks: state.checks,
        request_ids: state.request_ids,
        credential_id: state.credential_id,
        project_id: owner.project_id,
        branch_id: owner.branch_id,
        synthetic_protocol_upstream: true,
        real_vendor_inference: false,
        prior_real_ollama_receipts_reused: true,
        performance_reliability_campaign: false,
        credentials_in_evidence: false,
        compute_created: false,
      }) + '\n',
    );
  }
  async function budget() {
    const r = await context.request.get(ai + '/budget');
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function csrf() {
    return decodeURIComponent(
      (await context.cookies()).find((v) => v.name === 'neon_v2_csrf')?.value || '',
    );
  }
  async function stats() {
    const r = await context.request.get(owner.upstream_url + '/stats');
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function open() {
    await page.goto(`/#/projects/${owner.project_id}/ai-gateway`);
    await page.getByLabel('AI 分支', { exact: true }).selectOption(owner.branch_id);
    await expect(page.getByLabel('AI 模型', { exact: true })).toContainText(owner.model_id);
    await page.getByLabel('AI 模型', { exact: true }).selectOption(owner.model_id);
    await expect(page.getByRole('region', { name: 'AI 预算', exact: true })).toBeVisible();
  }
  async function policy(enabled: boolean, micros: number) {
    await page.getByLabel('启用 AI 预算', { exact: true }).setChecked(enabled);
    await page.getByLabel('AI 预算额度', { exact: true }).fill((micros / 1000000).toFixed(6));
    const response = page.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().endsWith(ai + '/budget'),
    );
    await page.getByRole('button', { name: '保存 AI 预算', exact: true }).click();
    expect((await response).status()).toBe(200);
    await expect(page.getByRole('button', { name: '保存 AI 预算', exact: true })).toBeEnabled();
    expect((await budget()).policy.limit_micros).toBe(micros);
  }
  async function invoke(
    protocol: string,
    stream: boolean,
    prompt = 'synthetic protocol canary',
    want = 200,
  ) {
    await page.getByLabel('AI 协议', { exact: true }).selectOption(protocol);
    if (protocol !== 'embeddings' && protocol !== 'gemini')
      await page.getByLabel('AI 流式输出', { exact: true }).setChecked(stream);
    await page.getByLabel('AI 输入', { exact: true }).fill(prompt);
    const response = page.waitForResponse(
      (r) =>
        r.request().method() === 'POST' &&
        r.url().endsWith(ai + '/request' + (protocol === 'chat_completions' ? '' : '/' + protocol)),
    );
    await page.getByRole('button', { name: '发送到模型', exact: true }).click();
    const r = await response;
    expect(r.status()).toBe(want);
    const id = r.headers()['x-request-id'];
    if (id) {
      state.request_ids.push(id);
      await save();
    }
    await expect(page.getByRole('button', { name: '发送到模型', exact: true })).toBeEnabled();
    if (want === 200) await expect(page.getByTestId('ai-error')).toHaveCount(0);
    return id;
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await open();
    if (!state.checks.includes('ui_budget_policy_and_native_json_settlement')) {
      expect((await budget()).policy.version).toBe(0);
      await policy(true, 144);
      const id = await invoke('responses', false);
      await expect(page.getByTestId('ai-answer')).toContainText('synthetic-native-reply');
      await expect.poll(async () => (await budget()).policy.spent_micros).toBe(7);
      const b = await budget();
      expect(b.policy.reserved_micros).toBe(0);
      expect(b.reservations.find((v: { request_id: string }) => v.request_id === id).state).toBe(
        'settled',
      );
      await save('ui_budget_policy_and_native_json_settlement');
    }
    if (!state.checks.includes('budget_denied_before_supplier')) {
      const before = (await stats()).calls;
      await invoke('responses', false, 'denied canary', 429);
      await expect(page.getByTestId('ai-error')).toContainText('budget_exceeded');
      expect((await stats()).calls).toBe(before);
      await save('budget_denied_before_supplier');
    }
    if (!state.checks.includes('responses_and_messages_native_sse')) {
      await policy(true, 10000);
      await invoke('responses', true);
      await expect(page.getByTestId('ai-answer')).toContainText('synthetic-native-reply');
      await invoke('messages', true);
      await expect(page.getByTestId('ai-answer')).toContainText('synthetic-native-reply');
      await save('responses_and_messages_native_sse');
    }
    if (!state.checks.includes('gemini_and_embeddings_ui')) {
      await invoke('gemini', false);
      await expect(page.getByTestId('ai-answer')).toContainText('synthetic-native-reply');
      await invoke('embeddings', false);
      await expect(page.getByTestId('ai-answer')).toContainText('0.25');
      await save('gemini_and_embeddings_ui');
    }
    if (!state.checks.includes('chat_custom_tools_and_inline_image')) {
      await page.getByLabel('AI 协议', { exact: true }).selectOption('chat_completions');
      await page.getByLabel('AI 流式输出', { exact: true }).uncheck();
      await page.getByLabel('AI 高级 JSON', { exact: true }).check();
      await page.getByLabel('AI 请求 JSON', { exact: true }).fill(
        JSON.stringify({
          model: owner.model_id,
          messages: [
            {
              role: 'user',
              content: [
                { type: 'text', text: 'synthetic tool' },
                { type: 'image_url', image_url: { url: 'data:image/png;base64,AA==' } },
              ],
            },
          ],
          max_completion_tokens: 8,
          stream: false,
          tools: [
            { type: 'function', function: { name: 'weather', parameters: { type: 'object' } } },
          ],
        }),
      );
      const pending = page.waitForResponse(
        (r) => r.request().method() === 'POST' && r.url().endsWith(ai + '/request'),
      );
      await page.getByRole('button', { name: '发送到模型', exact: true }).click();
      const r = await pending;
      expect(r.status()).toBe(200);
      state.request_ids.push(r.headers()['x-request-id']);
      await expect(page.getByTestId('ai-answer')).toContainText('weather');
      await page.getByLabel('AI 高级 JSON', { exact: true }).uncheck();
      await save('chat_custom_tools_and_inline_image');
    }
    if (!state.checks.includes('sdk_headers_batch_embeddings_and_branch_denial')) {
      await page.goto(`/#/projects/${owner.project_id}/credentials`);
      await page.getByLabel('凭据分支', { exact: true }).selectOption(owner.branch_id);
      await page.getByLabel('凭据名称').fill('ci-native-' + Date.now());
      await page.getByLabel('凭据分支范围').selectOption('self');
      await page.getByLabel('凭据模型约束').fill(owner.model_id);
      await page.getByLabel('凭据有效天数').selectOption('1');
      const pending = page.waitForResponse(
        (r) => r.request().method() === 'POST' && r.url().endsWith('/credentials'),
      );
      await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
      const created = await pending;
      expect(created.status()).toBe(201);
      state.credential_id = (await created.json()).credential.id;
      state.token = await page.getByLabel('新 Backend Token').inputValue();
      await save();
      await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
      const root = `/ai/v1/${owner.branch_id}`;
      const auth = { Authorization: 'Bearer ' + state.token };
      const messages = await context.request.post(root + '/anthropic/v1/messages', {
        headers: { 'x-api-key': state.token, 'anthropic-version': '2023-06-01' },
        data: {
          model: owner.model_id,
          messages: [{ role: 'user', content: 'canary' }],
          max_tokens: 8,
        },
      });
      expect(messages.status()).toBe(200);
      expect((await messages.json()).type).toBe('message');
      const gemini = await context.request.post(
        root + '/gemini/v1beta/models/' + owner.model_id + ':streamGenerateContent?alt=sse',
        {
          headers: { 'x-goog-api-key': state.token },
          data: {
            contents: [{ parts: [{ text: 'canary' }] }],
            generationConfig: { maxOutputTokens: 8 },
          },
        },
      );
      expect(gemini.status()).toBe(200);
      expect(await gemini.text()).toContain('synthetic-native-reply');
      const embedding = await context.request.post(root + '/openai/v1/embeddings', {
        headers: auth,
        data: { model: owner.model_id, input: ['first', 'second'], dimensions: 2 },
      });
      expect(embedding.status()).toBe(200);
      expect((await embedding.json()).data.length).toBe(2);
      expect(
        (
          await context.request.post(`/ai/v1/${owner.sibling_id}/v1/responses`, {
            headers: auth,
            data: { model: owner.model_id, input: 'denied', max_output_tokens: 8 },
          })
        ).status(),
      ).toBe(403);
      await save('sdk_headers_batch_embeddings_and_branch_denial');
    }
    await open();
    if (!state.checks.includes('unknown_usage_holds_budget')) {
      state.unknown_id = await invoke('responses', false, '__uncertain__');
      await expect
        .poll(
          async () =>
            (await budget()).reservations.find(
              (v: { request_id: string }) => v.request_id === state.unknown_id,
            )?.state,
        )
        .toBe('uncertain');
      const b = await budget();
      expect(b.policy.reserved_micros).toBe(144);
      await policy(true, b.policy.spent_micros + b.policy.reserved_micros);
      const before = (await stats()).calls;
      await invoke('responses', false, 'held budget denied', 429);
      expect((await stats()).calls).toBe(before);
      await save('unknown_usage_holds_budget');
    }
    if (!state.checks.includes('operator_evidence_reconciliation')) {
      const statement = await context.request.get(
        owner.upstream_url + '/statement?request_id=' + state.unknown_id,
      );
      expect(statement.status()).toBe(200);
      const proof = await statement.json();
      expect(proof.request_id).toBe(state.unknown_id);
      expect(proof.synthetic).toBe(true);
      const evidence = createHash('sha256').update(JSON.stringify(proof)).digest('hex');
      await writeFile(
        path.join(privateDir!, 'ai-native-supplier-statement.json'),
        JSON.stringify(proof),
        { mode: 0o600 },
      );
      await page.getByRole('button', { name: '刷新预算', exact: true }).click();
      await page.getByRole('button', { name: '对账 ' + state.unknown_id, exact: true }).click();
      await page.getByLabel('AI 对账证据', { exact: true }).fill(evidence);
      await page.getByLabel('AI 对账用量', { exact: true }).fill(JSON.stringify(proof.usage));
      const pending = page.waitForResponse(
        (r) =>
          r.request().method() === 'POST' &&
          r.url().endsWith('/requests/' + state.unknown_id + '/reconcile'),
      );
      await page.getByRole('button', { name: '提交已确认对账', exact: true }).click();
      expect((await pending).status()).toBe(200);
      await expect.poll(async () => (await budget()).policy.reserved_micros).toBe(0);
      expect(
        (await budget()).reservations.find(
          (v: { request_id: string }) => v.request_id === state.unknown_id,
        ).actual_micros,
      ).toBe(7);
      await save('operator_evidence_reconciliation');
    }
    if (!state.checks.includes('credential_revoked_and_budget_disabled_counters_retained')) {
      const credentials = await context.request.get(
        prefix + '/branches/' + owner.branch_id + '/credentials',
      );
      expect(credentials.status()).toBe(200);
      const current = (await credentials.json()).items.find(
        (item: { id: string }) => item.id === state.credential_id,
      );
      const revoked = await context.request.delete(
        prefix + '/branches/' + owner.branch_id + '/credentials/' + state.credential_id,
        {
          headers: {
            'X-CSRF-Token': await csrf(),
            'If-Match': `"${current.generation}"`,
            'Idempotency-Key': 'native-revoke-' + state.credential_id,
          },
        },
      );
      expect(revoked.status()).toBe(200);
      expect(
        (
          await context.request.get(`/ai/v1/${owner.branch_id}/v1/models`, {
            headers: { Authorization: 'Bearer ' + state.token },
          })
        ).status(),
      ).toBe(401);
      const b = await budget();
      await policy(false, b.policy.limit_micros);
      const final = await budget();
      expect(final.policy.enabled).toBe(false);
      expect(final.policy.spent_micros).toBe(b.policy.spent_micros);
      expect(final.policy.reserved_micros).toBe(0);
      state.token = '';
      await save('credential_revoked_and_budget_disabled_counters_retained');
    }
    state.result = 'pass';
    await save();
  } catch (e) {
    state.result = 'fail';
    await save();
    throw e;
  }
});
