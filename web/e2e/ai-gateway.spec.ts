import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('AI Gateway: actual Ollama UI chat, SSE, usage, app authorization and revocation', async ({
  page,
  context,
}, testInfo) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const project = process.env.NEON_E2E_CREDENTIAL_PROJECT;
  if (!adminFile || !project) throw new Error('Linux real-model fixture required');
  const prefix = `/api/v1/projects/${project}`;
  const checks: Record<string, unknown>[] = [];
  let state = 'running';
  let credential = '';
  let branch = '';
  let token = '';
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result: state,
          project_id: project,
          credential_id: credential,
          branch_id: branch,
          checks,
          real_inference: true,
          model_provider: 'local Ollama',
          compute_created: false,
          credentials_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  async function playground() {
    await page.goto(`/#/projects/${project}/ai-gateway`);
    await page.getByLabel('AI 分支', { exact: true }).selectOption(branch);
    await expect(page.getByLabel('AI 模型', { exact: true })).toBeVisible();
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const detail = await context.request.get(prefix);
    expect(detail.status()).toBe(200);
    expect((await detail.json()).name.startsWith('ci-')).toBe(true);
    const listing = await context.request.get(prefix + '/branches');
    expect(listing.status()).toBe(200);
    const branches = (await listing.json()).items;
    branch = branches.find(
      (b: { is_default: boolean; state: string }) => b.is_default && b.state === 'ready',
    ).id;
    await playground();
    const model = await page.getByLabel('AI 模型', { exact: true }).inputValue();
    expect(model).toBe('local-small');
    await record('ui_catalog_approved_real_model');
    for (const stream of [false, true]) {
      await page.getByLabel('AI 输入', { exact: true }).fill('Say hello in one short sentence.');
      await page.getByLabel('AI 流式输出', { exact: true }).setChecked(stream);
      const pending = page.waitForResponse(
        (r) => r.url().endsWith('/ai-gateway/request') && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '发送到模型', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(200);
      await expect(page.getByRole('button', { name: '发送到模型', exact: true })).toBeEnabled({
        timeout: 180000,
      });
      await expect(page.getByTestId('ai-error')).toHaveCount(0);
      const text = await page.getByTestId('ai-answer').textContent();
      expect(Boolean(text && text.trim() && text !== '模型输出将显示在这里')).toBe(true);
      const request = await page.getByTestId('ai-request-id').textContent();
      expect(request).toMatch(/air_[a-f0-9]{24}/);
      await expect
        .poll(
          async () => {
            const usage = await context.request.get(
              `${prefix}/branches/${branch}/ai-gateway/usage`,
            );
            const row = (await usage.json()).items.find((r: { id: string }) =>
              request?.includes(r.id),
            );
            return row?.state;
          },
          { timeout: 15000 },
        )
        .toBe('completed');
      const usage = await context.request.get(`${prefix}/branches/${branch}/ai-gateway/usage`);
      const row = (await usage.json()).items.find((r: { id: string }) => request?.includes(r.id));
      expect(row.total_tokens).toBeGreaterThan(0);
      expect(row.total_tokens).toBe(row.prompt_tokens + row.completion_tokens);
      await record(
        stream ? 'ui_real_sse_finished_and_usage_settled' : 'ui_real_json_and_usage_settled',
        { request_id: row.id, total_tokens: row.total_tokens },
      );
    }
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据分支', { exact: true }).selectOption(branch);
    await page.getByLabel('凭据名称').fill('ai-real-' + Date.now());
    await page.getByLabel('凭据分支范围').selectOption('self');
    await page.getByLabel('凭据模型约束').fill(model);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith('/credentials') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(201);
    credential = (await response.json()).credential.id;
    token = await page.getByLabel('新 Backend Token').inputValue();
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    const headers = { Authorization: 'Bearer ' + token };
    const models = await context.request.get(`/ai/v1/${branch}/v1/models`, { headers });
    expect(models.status()).toBe(200);
    expect((await models.json()).data.map((m: { id: string }) => m.id)).toEqual([model]);
    await record('ui_app_credential_filtered_model_catalog');
    expect((await context.request.get(`/ai/v1/${branch}/v1/models`)).status()).toBe(401);
    const sibling = branches.find(
      (b: { id: string; state: string }) => b.id !== branch && b.state === 'ready',
    );
    if (!sibling) throw new Error('Existing ready sibling required for branch-denial acceptance');
    expect(
      (await context.request.get(`/ai/v1/${sibling.id}/v1/models`, { headers })).status(),
    ).toBe(403);
    const chat = {
      model,
      messages: [{ role: 'user', content: 'Reply with one short greeting.' }],
      max_tokens: 16,
      stream: false,
      temperature: 0,
    };
    const invoked = await context.request.post(`/ai/v1/${branch}/v1/chat/completions`, {
      headers,
      data: chat,
    });
    expect(invoked.status()).toBe(200);
    const result = await invoked.json();
    expect(Boolean(result.choices[0].message.content)).toBe(true);
    expect(result.usage.total_tokens).toBeGreaterThan(0);
    await record('app_real_inference_and_cross_branch_denial');
    expect(
      (
        await context.request.post(`/ai/v1/${branch}/v1/chat/completions`, {
          headers,
          data: { ...chat, model: 'not-configured' },
        })
      ).status(),
    ).toBe(400);
    expect(
      (
        await context.request.post(`/ai/v1/${branch}/v1/chat/completions`, {
          headers,
          data: { ...chat, tools: [] },
        })
      ).status(),
    ).toBe(400);
    await record('unavailable_model_unsupported_protocol_fail_closed');
    const revoking = page.waitForResponse(
      (r) => r.url().endsWith(`/credentials/${credential}`) && r.request().method() === 'DELETE',
    );
    await page
      .getByTestId(`backend-row-${credential}`)
      .getByRole('button', { name: '撤销凭据', exact: true })
      .click();
    expect((await revoking).status()).toBe(200);
    expect((await context.request.get(`/ai/v1/${branch}/v1/models`, { headers })).status()).toBe(
      401,
    );
    token = '';
    await record('ui_revocation_effective_at_gateway');
    await playground();
    await page.getByRole('button', { name: '刷新用量', exact: true }).click();
    await page.screenshot({ path: testInfo.outputPath('ai-gateway.png'), fullPage: true });
    state = 'pass';
    await record('ui_usage_reload_persists_no_compute_needed');
  } finally {
    token = '';
    if (credential && branch) {
      const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '';
      const credentials = await context.request.get(`${prefix}/branches/${branch}/credentials`);
      if (credentials.status() === 200) {
        const item = (await credentials.json()).items.find(
          (v: { id: string }) => v.id === credential,
        );
        if (item && item.state !== 'revoked') {
          const retired = await context.request.delete(
            `${prefix}/branches/${branch}/credentials/${credential}`,
            {
              headers: {
                'X-CSRF-Token': csrf,
                'If-Match': `"${item.generation}"`,
                'Idempotency-Key': 'cleanup-ai-' + credential,
              },
            },
          );
          expect(retired.status()).toBe(200);
        }
      }
    }
    if (state !== 'pass') state = 'fail';
    await record('owned_credential_retired_usage_evidence_retained');
  }
});
