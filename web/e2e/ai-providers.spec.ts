import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Provider UI preserves original operations, publishes cached native models and rejects failed credential updates', async ({
  page,
  context,
}, info) => {
  test.setTimeout(600000);
  const fixtureFile = process.env.NEON_E2E_AI_PROVIDER_FIXTURE;
  const keyFile = process.env.NEON_E2E_AI_PROVIDER_KEY_FILE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !fixtureFile || !keyFile || !adminFile || !privateDir)
    throw Error('Protected Linux native provider fixture required');
  const owner = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    !/^prj_[a-f0-9]{16}$/.test(owner.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(owner.branch_id) ||
    owner.base_url !== 'http://192.168.146.100:11437/v1'
  )
    throw Error('Exact retained scope and approved private relay required');
  const state = owner.resume || {
    result: 'running',
    checks: [] as string[],
    provider_id: '',
    original_revision_id: '',
    original_operation_id: '',
    failed_revision_id: '',
    updated_revision_id: '',
    model_ids: [] as string[],
    request_ids: [] as string[],
    alias: 'ci-provider-' + Date.now(),
    operations: [] as string[],
  };
  state.result = 'running';
  const base = '/api/v1/platform/ai-providers';
  const ai = `/api/v1/projects/${owner.project_id}/branches/${owner.branch_id}/ai-gateway`;
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    const receipt = {
      ...state,
      project_id: owner.project_id,
      branch_id: owner.branch_id,
      real_inference: true,
      downloaded_models: false,
      credentials_in_evidence: false,
      compute_created: false,
      performance_reliability_campaign: false,
    };
    const raw = JSON.stringify(receipt) + '\n';
    await writeFile(info.outputPath('result.json'), raw);
    await writeFile(path.join(privateDir!, 'ai-provider-original-resume.json'), raw, {
      mode: 0o600,
    });
  }
  async function detail() {
    const r = await context.request.get(`${base}/${state.provider_id}`);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function openProvider() {
    await page.goto('/#/ai-providers');
    await expect(page.getByLabel('供应商定义', { exact: true })).toBeVisible();
    if (state.provider_id)
      await page.getByLabel('供应商定义', { exact: true }).selectOption(state.provider_id);
  }
  async function posted(suffix: string, click: () => Promise<void>) {
    const response = page.waitForResponse(
      (r: Response) => r.request().method() === 'POST' && r.url().endsWith(suffix),
    );
    await click();
    const r = await response;
    expect(r.status()).toBe(suffix.endsWith('/publish') || suffix.endsWith('/disable') ? 200 : 202);
    return r.json();
  }
  async function waitTest(revision: string) {
    await expect
      .poll(
        async () => {
          const value = await detail();
          return value.revisions.find((r: { id: string }) => r.id === revision)?.operation_state;
        },
        { timeout: 90000, intervals: [1000, 2000] },
      )
      .toMatch(/^(succeeded|failed)$/);
    await page.getByRole('button', { name: '刷新目录', exact: true }).click();
  }
  async function infer() {
    await page.goto(`/#/projects/${owner.project_id}/ai-gateway`);
    await page.getByLabel('AI 分支', { exact: true }).selectOption(owner.branch_id);
    await expect(page.getByLabel('AI 模型', { exact: true })).toContainText(state.alias);
    await page.getByLabel('AI 模型', { exact: true }).selectOption(state.alias);
    await page.getByLabel('AI 流式输出', { exact: true }).uncheck();
    await page.getByLabel('AI 输入', { exact: true }).fill('Reply with one short greeting.');
    const response = page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith(ai + '/request'),
    );
    await page.getByRole('button', { name: '发送到模型', exact: true }).click();
    const r = await response;
    expect(r.status()).toBe(200);
    const value = await r.json();
    expect(typeof value.choices?.[0]?.message?.content).toBe('string');
    expect(value.choices[0].message.content.length > 0).toBe(true);
    const id = r.headers()['x-request-id'];
    expect(id).toMatch(/^air_[a-f0-9]{24}$/);
    state.request_ids.push(id);
    await expect(page.getByTestId('ai-request-id')).toContainText(id);
    const usage = await context.request.get(ai + '/usage');
    expect(usage.status()).toBe(200);
    const ledger = (await usage.json()).items.find((r: { id: string }) => r.id === id);
    expect(ledger?.state).toBe('completed');
    expect(ledger?.model_id).toBe(state.alias);
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.locator('nav a[href="#/ai-providers"]')).toBeVisible();
    const caps = await context.request.get(base + '/capabilities');
    expect(caps.status()).toBe(200);
    expect((await caps.json()).can_manage).toBe(true);
    if (!state.checks.includes('lost_202_replayed_original_discovery')) {
      if (!state.provider_id) {
        await openProvider();
        await page
          .getByLabel('供应商名称', { exact: true })
          .fill('ci-native-provider-' + state.alias.slice(-13));
        await page.getByLabel('兼容接口地址', { exact: true }).fill(owner.base_url);
        await page.getByLabel('供应商认证方式', { exact: true }).selectOption('operator_reference');
        await page.getByLabel('已批准凭据', { exact: true }).selectOption(owner.source_id);
        let intercepted = false;
        let originalKey = '';
        await page.route('**/api/v1/platform/ai-providers', async (route) => {
          if (route.request().method() !== 'POST' || intercepted) {
            await route.continue();
            return;
          }
          intercepted = true;
          originalKey = route.request().headers()['idempotency-key'];
          const accepted = await route.fetch();
          expect(accepted.status()).toBe(202);
          const receipt = await accepted.json();
          state.provider_id = receipt.provider_id;
          state.original_revision_id = receipt.revision_id;
          state.original_operation_id = receipt.operation_id;
          state.operations.push(receipt.operation_id);
          await save();
          await route.abort('failed');
        });
        await page.getByRole('button', { name: '保存并测试模型', exact: true }).click();
        await expect(page.getByRole('button', { name: '重试相同提交', exact: true })).toBeVisible();
        const replay = page.waitForResponse(
          (r) => r.request().method() === 'POST' && r.url().endsWith(base),
        );
        await page.getByRole('button', { name: '重试相同提交', exact: true }).click();
        const accepted = await replay;
        expect(accepted.status()).toBe(202);
        expect(accepted.headers()['idempotency-replayed']).toBe('true');
        expect(accepted.request().headers()['idempotency-key'] === originalKey).toBe(true);
        const receipt = await accepted.json();
        expect(receipt.provider_id).toBe(state.provider_id);
        expect(receipt.operation_id).toBe(state.original_operation_id);
        expect(receipt.revision_id).toBe(state.original_revision_id);
        await page.unroute('**/api/v1/platform/ai-providers');
      } else {
        await openProvider();
        const retained = await detail();
        const original = retained.revisions.find(
          (r: { id: string }) => r.id === state.original_revision_id,
        );
        if (original?.operation_state === 'failed' && original.retryable) {
          const receipt = await posted(
            `/ai-provider-operations/${state.original_operation_id}/retry`,
            () => page.getByRole('button', { name: '重试原模型测试', exact: true }).click(),
          );
          expect(receipt.operation_id).toBe(state.original_operation_id);
          state.original_discovery_explicit_retry = true;
          await save();
        }
      }
      await waitTest(state.original_revision_id);
      const value = await detail();
      const revision = value.revisions.find(
        (r: { id: string }) => r.id === state.original_revision_id,
      );
      expect(revision.state).toBe('tested');
      expect(revision.models).toContain(owner.upstream_model);
      state.model_ids = revision.models;
      await save('lost_202_replayed_original_discovery');
    }
    if (!state.checks.includes('published_cached_model_actual_inference')) {
      await openProvider();
      await page.getByLabel('上游模型 1', { exact: true }).selectOption(owner.upstream_model);
      await page.getByLabel('公开别名 1', { exact: true }).fill(state.alias);
      await page.getByLabel('模型名称 1', { exact: true }).fill('Native cached small model');
      await page.getByLabel('最大输出 Token 1', { exact: true }).fill('32');
      await posted(`/ai-providers/${state.provider_id}/publish`, () =>
        page.getByRole('button', { name: '发布当前测试版本', exact: true }).click(),
      );
      await infer();
      await save('published_cached_model_actual_inference');
    }
    if (!state.checks.includes('failed_credential_update_kept_active_publication')) {
      await openProvider();
      if (!state.failed_revision_id) {
        await page.getByLabel('供应商认证方式', { exact: true }).selectOption('sealed');
        await page
          .getByLabel('新供应商密钥', { exact: true })
          .fill('disposable-deliberately-wrong-key-0123456789');
        await page
          .getByRole('checkbox', {
            name: '我确认这是已批准内网实验接口的专用测试凭据',
            exact: true,
          })
          .check();
        const receipt = await posted(`/ai-providers/${state.provider_id}/revisions`, () =>
          page.getByRole('button', { name: '保存并测试模型', exact: true }).click(),
        );
        state.failed_revision_id = receipt.revision_id;
        state.operations.push(receipt.operation_id);
        await save();
      }
      await waitTest(state.failed_revision_id);
      await expect(page.getByTestId('provider-test-error')).toHaveText(
        'provider_authentication_rejected',
      );
      await expect(page.getByRole('button', { name: '发布当前测试版本', exact: true })).toHaveCount(
        0,
      );
      const value = await detail();
      expect(value.provider.state).toBe('active');
      const active = value.publications.find(
        (p: { id: string }) => p.id === value.provider.active_publication_id,
      );
      expect(active.revision_id).toBe(state.original_revision_id);
      await infer();
      await save('failed_credential_update_kept_active_publication');
    }
    if (!state.checks.includes('replacement_sealed_credential_published_and_invoked')) {
      await openProvider();
      if (!state.updated_revision_id) {
        await page.getByLabel('供应商认证方式', { exact: true }).selectOption('sealed');
        let key = (await readFile(keyFile, 'utf8')).trim();
        await page.getByLabel('新供应商密钥', { exact: true }).fill(key);
        key = '';
        await page
          .getByRole('checkbox', {
            name: '我确认这是已批准内网实验接口的专用测试凭据',
            exact: true,
          })
          .check();
        const receipt = await posted(`/ai-providers/${state.provider_id}/revisions`, () =>
          page.getByRole('button', { name: '保存并测试模型', exact: true }).click(),
        );
        state.updated_revision_id = receipt.revision_id;
        state.operations.push(receipt.operation_id);
        await save();
        expect(
          (await page.getByLabel('新供应商密钥', { exact: true }).inputValue()).length === 0,
        ).toBe(true);
      }
      await waitTest(state.updated_revision_id);
      await page.getByLabel('上游模型 1', { exact: true }).selectOption(owner.upstream_model);
      await page.getByLabel('公开别名 1', { exact: true }).fill(state.alias);
      await page.getByLabel('模型名称 1', { exact: true }).fill('Updated native model route');
      await page.getByLabel('最大输出 Token 1', { exact: true }).fill('32');
      await posted(`/ai-providers/${state.provider_id}/publish`, () =>
        page.getByRole('button', { name: '发布当前测试版本', exact: true }).click(),
      );
      await infer();
      await save('replacement_sealed_credential_published_and_invoked');
    }
    if (!state.checks.includes('disabled_catalog_and_definition_retired_history_retained')) {
      await openProvider();
      await posted(`/ai-providers/${state.provider_id}/disable`, () =>
        page.getByRole('button', { name: '停用供应商', exact: true }).click(),
      );
      const catalog = await context.request.get(ai);
      expect(catalog.status()).toBe(200);
      const modelIDs = (await catalog.json()).models.map((m: { id: string }) => m.id);
      expect(modelIDs.includes(state.alias)).toBe(false);
      expect(modelIDs).toContain('local-small');
      expect(modelIDs).toContain('local-sql');
      const value = await detail();
      expect(value.provider.state).toBe('disabled');
      expect(value.revisions.length).toBe(3);
      expect(value.publications.length).toBe(2);
      const deletion = page.waitForResponse(
        (r) =>
          r.request().method() === 'DELETE' && r.url().endsWith(`${base}/${state.provider_id}`),
      );
      await page.getByRole('button', { name: '删除定义并保留历史', exact: true }).click();
      expect((await deletion).status()).toBe(200);
      await expect(page.getByLabel('供应商定义', { exact: true })).not.toContainText(
        'ci-native-provider-' + state.alias.slice(-13),
      );
      expect((await context.request.get(`${base}/${state.provider_id}`)).status()).toBe(404);
      await save('disabled_catalog_and_definition_retired_history_retained');
    }
    state.result = 'pass';
    await save();
  } catch (e) {
    state.result = 'fail';
    await save();
    throw e;
  }
});
