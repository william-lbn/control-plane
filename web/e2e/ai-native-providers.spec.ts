import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Native provider discovery, immutable publication and Console inference', async ({
  page,
  context,
}, info) => {
  test.setTimeout(300000);
  page.setDefaultTimeout(15000);
  page.setDefaultNavigationTimeout(20000);
  const file = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE;
  const admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !file || !admin || !privateDir)
    throw Error('Protected Linux native discovery fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8'));
  if (
    fixture.synthetic !== true ||
    !/^http:\/\/10\.43\.[0-9]+\.[0-9]+:9081$/.test(fixture.upstream_url) ||
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^native-discovery-[a-f0-9]+$/.test(fixture.source_id)
  )
    throw Error('Exact owned fixture required');
  const base = '/api/v1/platform/ai-providers';
  const ai = `/api/v1/projects/${fixture.project_id}/branches/${fixture.branch_id}/ai-gateway`;
  type NativeProvider = {
    kind: string;
    id: string;
    revision: string;
    alias: string;
    protocol: string;
    version: number;
    request_id?: string;
  };
  const state: {
    result: string;
    checks: string[];
    providers: NativeProvider[];
    requests: string[];
  } = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : { result: 'running', checks: [], providers: [], requests: [] };
  state.result = 'running';
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    const receipt = {
      ...state,
      synthetic_upstream: true,
      real_commercial_account: false,
      compute_created: false,
      credentials_exported: false,
      performance_reliability_campaign: false,
    };
    await writeFile(
      path.join(privateDir!, 'ai-native-providers-checkpoint.private.json'),
      JSON.stringify(receipt),
      { mode: 0o600 },
    );
    await writeFile(info.outputPath('result.json'), JSON.stringify(receipt) + '\n');
  }
  async function stats() {
    const r = await context.request.get(fixture.upstream_url + '/stats');
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function csrf() {
    return decodeURIComponent(
      (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '',
    );
  }
  async function detail(id: string) {
    const r = await context.request.get(base + '/' + id);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function post(url: string, data: unknown, want: number) {
    const r = await context.request.post(url, {
      headers: {
        'X-CSRF-Token': await csrf(),
        'Idempotency-Key': 'native-' + Date.now() + '-' + state.checks.length,
      },
      data,
    });
    expect(r.status()).toBe(want);
    return r.json();
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.locator('nav a[href="#/ai-providers"]')).toBeVisible();
    const contract = await context.request.get('/api/openapi.json');
    expect(contract.status()).toBe(200);
    const schema = (await contract.json()).components.schemas.AIProviderCapabilities;
    const capabilities = await context.request.get(base + '/capabilities');
    expect(capabilities.status()).toBe(200);
    const caps = await capabilities.json();
    expect(caps.can_manage).toBe(true);
    for (const key of Object.keys(caps)) expect(Object.keys(schema.properties)).toContain(key);
    for (const key of schema.required) expect(caps[key]).not.toBeUndefined();
    expect(caps.protocols).toEqual(['openai_compatible', 'anthropic', 'gemini']);
    for (const kind of ['anthropic', 'gemini']) {
      let provider = state.providers.find((p) => p.kind === kind);
      if (!state.checks.includes(kind + '_native_discovery')) {
        await page.goto('/#/ai-providers');
        await expect(page.getByLabel('供应商类型', { exact: true })).toBeVisible();
        if (!provider) {
          await page.getByLabel('供应商类型', { exact: true }).selectOption(kind);
          await page
            .getByLabel('供应商名称', { exact: true })
            .fill('Synthetic native ' + kind + ' ' + Date.now());
          await page.getByLabel('兼容接口地址', { exact: true }).fill(fixture.upstream_url + '/v1');
          await page
            .getByLabel('供应商认证方式', { exact: true })
            .selectOption('operator_reference');
          await page.getByLabel('已批准凭据', { exact: true }).selectOption(fixture.source_id);
          const pending = page.waitForResponse(
            (r) => r.request().method() === 'POST' && r.url().endsWith(base),
          );
          await page.getByRole('button', { name: '保存并测试模型', exact: true }).click();
          const r = await pending;
          expect(r.status()).toBe(202);
          const created = await r.json();
          provider = {
            kind,
            id: created.provider_id,
            revision: created.revision_id,
            alias: 'ci-native-' + kind + '-' + Date.now(),
            protocol: kind === 'anthropic' ? 'messages' : 'gemini',
            version: 1,
          };
          state.providers.push(provider);
          await save();
        }
        const retained = provider;
        await expect
          .poll(
            async () =>
              (await detail(retained.id)).revisions.find(
                (r: { id: string }) => r.id === retained.revision,
              )?.operation_state,
            { timeout: 60000, intervals: [1000, 2000] },
          )
          .toBe('succeeded');
        const result = await detail(retained.id);
        const revision = result.revisions.find((r: { id: string }) => r.id === retained.revision);
        expect(revision.provider_type).toBe(kind);
        expect(revision.models).toEqual(
          kind === 'anthropic'
            ? ['claude-probe-1', 'claude-probe-2']
            : ['gemini-probe-1', 'gemini-probe-2'],
        );
        const counts = await stats();
        expect(counts[kind + '_metadata']).toBe(2);
        expect(counts[kind + '_inference']).toBe(0);
        await save(kind + '_native_discovery');
      }
      if (!provider) throw Error('Durable provider receipt missing');
      const retained = provider;
      if (!state.checks.includes(kind + '_publication_and_console')) {
        await page.goto('/#/ai-providers');
        await page.getByLabel('供应商定义', { exact: true }).selectOption(retained.id);
        await expect(
          page.getByRole('heading', { name: '发布模型别名', exact: true }),
        ).toBeVisible();
        await page
          .getByLabel('上游模型 1', { exact: true })
          .selectOption(kind === 'anthropic' ? 'claude-probe-2' : 'gemini-probe-2');
        await page.getByLabel('公开别名 1', { exact: true }).fill(retained.alias);
        await page.getByLabel('模型名称 1', { exact: true }).fill('Synthetic ' + kind);
        await page.getByLabel('最大输出 Token 1', { exact: true }).fill('32');
        const current = await detail(retained.id);
        const published = current.publications.some(
          (p: { revision_id: string; models: { id: string }[] }) =>
            p.revision_id === retained.revision && p.models.some((m) => m.id === retained.alias),
        );
        if (!published) {
          const pending = page.waitForResponse(
            (r) =>
              r.request().method() === 'POST' &&
              r.url().endsWith(base + '/' + retained.id + '/publish'),
          );
          await page.getByRole('button', { name: '发布当前测试版本', exact: true }).click();
          expect((await pending).status()).toBe(200);
        }
        retained.version = 2;
        await save();
        await page.goto(`/#/projects/${fixture.project_id}/ai-gateway`);
        await page.getByLabel('AI 分支', { exact: true }).selectOption(fixture.branch_id);
        await expect(page.getByLabel('AI 模型', { exact: true })).toContainText(retained.alias);
        await page.getByLabel('AI 模型', { exact: true }).selectOption(retained.alias);
        await page.getByLabel('AI 协议', { exact: true }).selectOption(retained.protocol);
        await page
          .getByLabel('AI 输入', { exact: true })
          .fill('Synthetic native discovery acceptance');
        const stream = page.getByLabel('AI 流式输出', { exact: true });
        if (await stream.isEnabled()) await stream.uncheck();
        const prior = await context.request.get(ai + '/usage');
        expect(prior.status()).toBe(200);
        const existing = (await prior.json()).items.filter(
          (v: { model_id: string }) => v.model_id === retained.alias,
        );
        expect(existing.length).toBeLessThanOrEqual(1);
        if (existing.length === 0) {
          // A retained request, including an unknown result, is never retried.
          expect(retained.request_id).toBeUndefined();
          const pendingRequest = page.waitForResponse(
            (r) =>
              r.request().method() === 'POST' &&
              r.url().endsWith(ai + '/request/' + retained.protocol),
          );
          const send = page.getByRole('button', { name: '发送到模型', exact: true });
          await expect(send).toBeEnabled();
          await send.click();
          const r = await pendingRequest;
          expect(r.status()).toBe(200);
          retained.request_id = r.headers()['x-request-id'];
          expect(retained.request_id).toMatch(/^air_[a-f0-9]{24}$/);
          await save();
          expect(await r.text()).toContain('Synthetic');
        } else {
          expect(existing[0].state).toBe('completed');
          expect(existing[0].protocol).toBe(retained.protocol);
          if (retained.request_id) expect(existing[0].id).toBe(retained.request_id);
          retained.request_id = existing[0].id;
        }
        const id = retained.request_id;
        if (!id) throw Error('Original native inference receipt missing');
        if (!state.requests.includes(id)) state.requests.push(id);
        const usage = await context.request.get(ai + '/usage');
        expect(usage.status()).toBe(200);
        const row = (await usage.json()).items.find((v: { id: string }) => v.id === id);
        expect(row?.state).toBe('completed');
        expect(row?.total_tokens).toBe(5);
        await save(kind + '_publication_and_console');
      }
      if (!state.checks.includes(kind + '_type_isolation')) {
        await post(
          base + '/' + retained.id + '/revisions',
          {
            version: 2,
            name: 'Cross-type reuse rejected',
            base_url: fixture.upstream_url + '/v1',
            provider_type: 'openai_compatible',
            credential: { mode: 'reuse', reuse_revision_id: retained.revision },
          },
          422,
        );
        expect((await detail(retained.id)).provider.state).toBe('active');
        expect((await stats())[kind + '_inference']).toBe(1);
        await save(kind + '_type_isolation');
      }
    }
    if (!state.checks.includes('invalid_types_do_not_contact_supplier')) {
      for (const provider_type of ['arbitrary', 'gemini'])
        await post(
          base,
          {
            version: 0,
            name: 'Rejected native fixture',
            base_url: fixture.upstream_url + '/v1',
            provider_type,
            credential: { mode: 'none' },
          },
          422,
        );
      const counts = await stats();
      expect(counts.rejected).toBe(0);
      expect(counts.anthropic_metadata).toBe(2);
      expect(counts.gemini_metadata).toBe(2);
      await save('invalid_types_do_not_contact_supplier');
    }
    if (!state.checks.includes('definitions_disabled_history_retained')) {
      for (const p of state.providers) {
        const value = await detail(p.id);
        if (value.provider.state === 'active')
          await post(base + '/' + p.id + '/disable', { version: value.provider.version }, 200);
        const retained = await detail(p.id);
        expect(retained.provider.state).toBe('disabled');
        expect(retained.revisions.some((r: { id: string }) => r.id === p.revision)).toBe(true);
      }
      await save('definitions_disabled_history_retained');
    }
    expect(state.checks).toHaveLength(8);
    state.result = 'pass';
    await save();
  } catch (e) {
    state.result = 'fail';
    await save();
    throw e;
  }
});
