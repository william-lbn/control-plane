import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Organization BYOK scope, own credential rotation and fee attribution', async ({
  page,
  context,
}, info) => {
  test.setTimeout(300000);
  page.setDefaultTimeout(15000);
  page.setDefaultNavigationTimeout(20000);
  const file = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !file || !admin || !privateDir)
    throw Error('Protected Linux BYOK fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8'));
  if (
    fixture.synthetic !== true ||
    !/^http:\/\/10\.43\.[0-9]+\.[0-9]+:9081$/.test(fixture.upstream_url) ||
    typeof fixture.org_id !== 'string' ||
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^\/run\/e2e-private\/[a-z-]+\.private$/.test(fixture.credential_file)
  )
    throw Error('Exact owned BYOK scope required');
  type State = {
    result: string;
    checks: string[];
    nonce: string;
    provider?: string;
    original_revision?: string;
    rotated_revision?: string;
    request_id?: string;
    credit_before?: unknown;
  };
  const state: State = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : { result: 'running', checks: [], nonce: String(Date.now()) };
  state.result = 'running';
  const base = `/api/v1/organizations/${fixture.org_id}/ai-providers`,
    ai = `/api/v1/projects/${fixture.project_id}/branches/${fixture.branch_id}/ai-gateway`,
    alias = 'ci-byok-' + state.nonce,
    name = 'Synthetic organization BYOK ' + state.nonce;
  const scope = page.getByRole('region', { name: '组织自有 AI 供应商', exact: true });
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'ai-organization-byok-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        ...state,
        synthetic_upstream: true,
        commercial_model_call: false,
        credentials_exported: false,
        compute_created: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function get(url: string) {
    const r = await context.request.get(url);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function detail() {
    return get(base + '/' + state.provider);
  }
  async function organization() {
    await page.goto('/#/organization');
    await expect(scope).toBeVisible();
  }
  async function select() {
    await organization();
    await scope.getByLabel('供应商定义', { exact: true }).selectOption(state.provider!);
    await expect(scope.getByLabel('供应商名称', { exact: true })).toHaveValue(name);
  }
  async function tested(revision: string) {
    await expect
      .poll(
        async () =>
          (await detail()).revisions.find((r: { id: string }) => r.id === revision)
            ?.operation_state,
        { timeout: 60000, intervals: [1000, 2000] },
      )
      .toBe('succeeded');
  }
  async function publish() {
    const d = await detail();
    if (
      d.publications.some(
        (p: { revision_id: string; models: { id: string }[] }) =>
          p.revision_id === d.provider.target_revision_id && p.models.some((m) => m.id === alias),
      )
    )
      return;
    await select();
    await expect(scope.getByRole('heading', { name: '发布模型别名', exact: true })).toBeVisible();
    await scope.getByLabel('上游模型 1', { exact: true }).selectOption('claude-probe-2');
    await scope.getByLabel('公开别名 1', { exact: true }).fill(alias);
    await scope.getByLabel('模型名称 1', { exact: true }).fill('Synthetic organization model');
    await scope.getByLabel('最大输出 Token 1', { exact: true }).fill('32');
    const pending = page.waitForResponse(
      (r) =>
        r.request().method() === 'POST' &&
        r.url().endsWith(base + '/' + state.provider + '/publish'),
    );
    await scope.getByRole('button', { name: '发布当前测试版本', exact: true }).click();
    expect((await pending).status()).toBe(200);
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByLabel('切换组织', { exact: true })).toBeVisible();
    const org = (await get('/api/v1/organizations')).items.find(
      (o: { id: string }) => o.id === fixture.org_id,
    );
    expect(org).toBeTruthy();
    await page.getByLabel('切换组织', { exact: true }).click();
    await page.getByRole('option', { name: org.name + ' · ' + org.role, exact: true }).click();
    if (!state.checks.includes('organization_capabilities_exclude_platform_sources')) {
      const caps = await get(base + '/capabilities');
      expect(caps.can_manage).toBe(true);
      expect(caps.credential_sources).toEqual([]);
      const contract = await get('/api/openapi.json');
      expect(contract.info.version).toBe('0.76.0');
      for (const k of Object.keys(caps))
        expect(
          Object.keys(contract.components.schemas.AIProviderCapabilities.properties),
        ).toContain(k);
      state.credit_before = (
        await get(`/api/v1/organizations/${fixture.org_id}/ai-credits`)
      ).account;
      await save('organization_capabilities_exclude_platform_sources');
    }
    if (!state.checks.includes('organization_sealed_credential_discovery')) {
      const found = (await get(base)).items.filter((p: { name: string }) => p.name === name);
      expect(found.length).toBeLessThanOrEqual(1);
      if (found.length) {
        state.provider = found[0].id;
        state.original_revision = found[0].target_revision_id;
        await save();
      }
      if (!state.provider) {
        await organization();
        await scope.getByLabel('供应商类型', { exact: true }).selectOption('anthropic');
        await scope.getByLabel('供应商名称', { exact: true }).fill(name);
        await scope.getByLabel('兼容接口地址', { exact: true }).fill(fixture.upstream_url + '/v1');
        await expect(
          scope
            .getByLabel('供应商认证方式', { exact: true })
            .locator('option[value="operator_reference"]'),
        ).toHaveCount(0);
        await scope.getByLabel('供应商认证方式', { exact: true }).selectOption('sealed');
        await scope
          .getByLabel('新供应商密钥', { exact: true })
          .fill((await readFile(fixture.credential_file, 'utf8')).trim());
        await scope.getByLabel('我确认这是已批准内网实验接口的专用测试凭据').check();
        const pending = page.waitForResponse(
          (r) => r.request().method() === 'POST' && r.url().endsWith(base),
        );
        await scope.getByRole('button', { name: '保存并测试模型', exact: true }).click();
        const r = await pending;
        expect(r.status()).toBe(202);
        const v = await r.json();
        state.provider = v.provider_id;
        state.original_revision = v.revision_id;
        await save();
      }
      await tested(state.original_revision!);
      await save('organization_sealed_credential_discovery');
    }
    if (!state.provider || !state.original_revision) throw Error('Original BYOK receipt missing');
    if (!state.checks.includes('organization_publication_and_private_catalog')) {
      await publish();
      const models = (await get(ai)).models;
      expect(models.find((m: { id: string }) => m.id === alias)?.billing_owner).toBe(
        'organization_byok',
      );
      expect(
        (await get('/api/v1/platform/ai-providers')).items.some(
          (p: { id: string }) => p.id === state.provider,
        ),
      ).toBe(false);
      await save('organization_publication_and_private_catalog');
    }
    if (!state.checks.includes('own_supplier_inference_and_fee_attribution')) {
      const existing = (await get(ai + '/usage')).items.filter(
        (v: { model_id: string }) => v.model_id === alias,
      );
      expect(existing.length).toBeLessThanOrEqual(1);
      if (!existing.length) {
        expect(state.request_id).toBeUndefined();
        await page.goto(`/#/projects/${fixture.project_id}/ai-gateway`);
        await page.getByLabel('AI 分支', { exact: true }).selectOption(fixture.branch_id);
        await page.getByLabel('AI 模型', { exact: true }).selectOption(alias);
        await page.getByLabel('AI 协议', { exact: true }).selectOption('messages');
        await page
          .getByLabel('AI 输入', { exact: true })
          .fill('Synthetic organization BYOK acceptance');
        await page.getByLabel('AI 流式输出', { exact: true }).uncheck();
        const send = page.getByRole('button', { name: '发送到模型', exact: true });
        await expect(send).toBeEnabled();
        const pending = page.waitForResponse(
          (r) => r.request().method() === 'POST' && r.url().endsWith(ai + '/request/messages'),
        );
        await send.click();
        const r = await pending;
        expect(r.status()).toBe(200);
        state.request_id = r.headers()['x-request-id'];
        expect(state.request_id).toMatch(/^air_[a-f0-9]{24}$/);
        await save();
        expect(await r.text()).toContain('Synthetic');
      } else {
        expect(existing[0].state).toBe('completed');
        state.request_id = existing[0].id;
      }
      await expect
        .poll(
          async () =>
            (await get(ai + '/usage')).items.find((v: { id: string }) => v.id === state.request_id)
              ?.total_tokens,
        )
        .toBe(5);
      const usage = (await get(ai + '/usage')).items.find(
        (v: { id: string }) => v.id === state.request_id,
      );
      expect(usage.billing_owner).toBe('organization_byok');
      expect(usage.provider_id).toBe(state.provider);
      expect(usage.provider_revision_id).toBe(state.original_revision);
      expect((await get(`/api/v1/organizations/${fixture.org_id}/ai-credits`)).account).toEqual(
        state.credit_before,
      );
      await save('own_supplier_inference_and_fee_attribution');
    }
    if (!state.checks.includes('organization_same_provider_credential_rotation')) {
      let d = await detail();
      if (d.provider.target_revision_id !== state.original_revision)
        state.rotated_revision = d.provider.target_revision_id;
      if (!state.rotated_revision) {
        await select();
        await scope.getByLabel('供应商认证方式', { exact: true }).selectOption('reuse');
        await scope
          .getByLabel('凭据来源版本', { exact: true })
          .selectOption(state.original_revision);
        const pending = page.waitForResponse(
          (r) =>
            r.request().method() === 'POST' &&
            r.url().endsWith(base + '/' + state.provider + '/revisions'),
        );
        await scope.getByRole('button', { name: '保存并测试模型', exact: true }).click();
        const r = await pending;
        expect(r.status()).toBe(202);
        state.rotated_revision = (await r.json()).revision_id;
        await save();
      }
      await tested(state.rotated_revision!);
      await publish();
      await save('organization_same_provider_credential_rotation');
    }
    if (!state.checks.includes('historical_fee_owner_survives_rotation')) {
      const usage = (await get(ai + '/usage')).items.find(
        (v: { id: string }) => v.id === state.request_id,
      );
      expect(usage.provider_revision_id).toBe(state.original_revision);
      expect(usage.billing_owner).toBe('organization_byok');
      const d = await detail();
      expect(d.revisions).toHaveLength(2);
      expect(d.publications).toHaveLength(2);
      await save('historical_fee_owner_survives_rotation');
    }
    if (!state.checks.includes('disabled_byok_withdraws_models_and_retains_history')) {
      const d = await detail();
      if (d.provider.state === 'active') {
        await select();
        const pending = page.waitForResponse(
          (r) =>
            r.request().method() === 'POST' &&
            r.url().endsWith(base + '/' + state.provider + '/disable'),
        );
        await scope.getByRole('button', { name: '停用供应商', exact: true }).click();
        expect((await pending).status()).toBe(200);
      }
      expect((await detail()).provider.state).toBe('disabled');
      expect((await get(ai)).models.some((m: { id: string }) => m.id === alias)).toBe(false);
      const stats = await get(fixture.upstream_url + '/stats');
      expect(stats.anthropic_inference).toBe(1);
      expect(stats.anthropic_metadata).toBe(4);
      expect(stats.rejected).toBe(0);
      expect((await get(`/api/v1/organizations/${fixture.org_id}/ai-credits`)).account).toEqual(
        state.credit_before,
      );
      await save('disabled_byok_withdraws_models_and_retains_history');
    }
    expect(state.checks).toHaveLength(7);
    state.result = 'pass';
    await save();
  } catch (e) {
    state.result = 'fail';
    await save();
    throw e;
  }
});
