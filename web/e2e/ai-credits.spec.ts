import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Organization credits retain unknown funds and settle immutable evidence', async ({
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
    throw Error('Protected Linux credit fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8'));
  if (
    fixture.synthetic !== true ||
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^ci-credit-[a-f0-9]+$/.test(fixture.model_id) ||
    !/^http:\/\/10\.43\.[0-9]+\.[0-9]+:9081$/.test(fixture.upstream_url) ||
    typeof fixture.org_id !== 'string'
  )
    throw Error('Exact owned credit fixture required');
  const root = `/api/v1/organizations/${fixture.org_id}/ai-credits`,
    platform = `/api/v1/platform/ai-credits/organizations/${fixture.org_id}`;
  const ai = `/api/v1/projects/${fixture.project_id}/branches/${fixture.branch_id}/ai-gateway`;
  type Mutation = { body: unknown; key: string; entry_id: string };
  const state: {
    result: string;
    checks: string[];
    nonce: string;
    requests: { known?: string; unknown?: string };
    second?: Mutation;
  } = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : { result: 'running', checks: [], nonce: String(Date.now()), requests: {} };
  state.result = 'running';
  const sources = {
    initial: 'ci-credit-initial-' + state.nonce,
    second: 'ci-credit-second-' + state.nonce,
    correction: 'ci-credit-correction-' + state.nonce,
  };
  const evidence = (value: unknown) =>
    createHash('sha256').update(JSON.stringify(value)).digest('hex');
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'ai-credits-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result: state.result,
        checks: state.checks,
        request_ids: state.requests,
        synthetic_credits: true,
        real_payment: false,
        synthetic_supplier: true,
        commercial_model_call: false,
        credentials_exported: false,
        compute_created: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function csrf() {
    return decodeURIComponent(
      (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '',
    );
  }
  async function account() {
    const r = await context.request.get(root);
    expect(r.status()).toBe(200);
    expect(r.headers()['cache-control']).toBe('no-store');
    return (await r.json()).account;
  }
  async function entries() {
    const r = await context.request.get(root + '/ledger');
    expect(r.status()).toBe(200);
    return (await r.json()).items;
  }
  async function stats() {
    const r = await context.request.get(fixture.upstream_url + '/stats');
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function openOrganization() {
    const r = await context.request.get('/api/v1/organizations');
    expect(r.status()).toBe(200);
    const org = (await r.json()).items.find((v: { id: string }) => v.id === fixture.org_id);
    expect(org).toBeTruthy();
    await page.getByLabel('切换组织', { exact: true }).click();
    await page.getByRole('option', { name: org.name + ' · ' + org.role, exact: true }).click();
    await page.goto('/#/organization');
    await expect(page.getByRole('region', { name: '组织 AI 额度', exact: true })).toBeVisible();
    await expect(page.getByLabel('启用组织 AI 额度', { exact: true })).toBeVisible();
  }
  async function policy(enabled: boolean) {
    await page.getByLabel('启用组织 AI 额度', { exact: true }).setChecked(enabled);
    const pending = page.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().endsWith(platform),
    );
    await page.getByRole('button', { name: '保存组织额度策略', exact: true }).click();
    expect((await pending).status()).toBe(200);
    await expect(page.getByRole('button', { name: '保存组织额度策略', exact: true })).toBeEnabled();
    expect((await account()).enabled).toBe(enabled);
  }
  async function fillEntry(kind: string, amount: string, source: string, reason: string) {
    await page.getByLabel('AI 额度操作', { exact: true }).selectOption(kind);
    await page.getByLabel('AI 授信金额', { exact: true }).fill(amount);
    await page.getByLabel('AI 授信来源', { exact: true }).fill(source);
    await page.getByLabel('AI 授信原因', { exact: true }).fill(reason);
    await page
      .getByLabel('AI 授信凭证', { exact: true })
      .fill(evidence({ kind, amount, source, synthetic: true }));
  }
  async function submitEntry(
    kind: string,
    amount: string,
    source: string,
    reason: string,
    delta: number,
  ) {
    const prior = (await entries()).find(
      (v: { source_event_id: string }) => v.source_event_id === source,
    );
    if (prior) {
      expect(prior.credit_delta).toBe(delta);
      return;
    }
    await fillEntry(kind, amount, source, reason);
    const pending = page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith(platform + '/entries'),
    );
    await page.getByRole('button', { name: '提交 AI 额度记录', exact: true }).click();
    expect((await pending).status()).toBe(200);
    await expect(page.getByRole('button', { name: '提交 AI 额度记录', exact: true })).toBeEnabled();
  }
  async function openModel(prompt: string) {
    await page.goto(`/#/projects/${fixture.project_id}/ai-gateway`);
    await page.getByLabel('AI 分支', { exact: true }).selectOption(fixture.branch_id);
    await expect(page.getByLabel('AI 模型', { exact: true })).toContainText(fixture.model_id);
    await page.getByLabel('AI 模型', { exact: true }).selectOption(fixture.model_id);
    await page.getByLabel('AI 协议', { exact: true }).selectOption('responses');
    await page.getByLabel('AI 流式输出', { exact: true }).uncheck();
    await page.getByLabel('AI 输入', { exact: true }).fill(prompt);
    await expect(page.getByRole('button', { name: '发送到模型', exact: true })).toBeEnabled();
  }
  async function callModel(prompt: string, want: number) {
    await openModel(prompt);
    const pending = page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith(ai + '/request/responses'),
    );
    await page.getByRole('button', { name: '发送到模型', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(want);
    return r;
  }
  async function usage() {
    const r = await context.request.get(ai + '/usage');
    expect(r.status()).toBe(200);
    return (await r.json()).items.filter(
      (v: { model_id: string }) => v.model_id === fixture.model_id,
    );
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByLabel('切换组织', { exact: true })).toBeVisible();
    const contract = await context.request.get('/api/openapi.json');
    expect(contract.status()).toBe(200);
    const schema = (await contract.json()).components.schemas.AICreditReadResult;
    const actual = await context.request.get(root);
    expect(actual.status()).toBe(200);
    const body = await actual.json();
    expect(body.can_manage).toBe(true);
    for (const key of Object.keys(body)) expect(Object.keys(schema.properties)).toContain(key);
    for (const key of schema.required) expect(body[key]).not.toBeUndefined();
    if (!state.checks.includes('operator_policy_and_initial_manual_grant')) {
      await openOrganization();
      const a = await account();
      if (a.version === 0) await policy(true);
      else expect(a.enabled).toBe(true);
      await submitEntry(
        'grant',
        '0.000020',
        sources.initial,
        'Synthetic initial operator credit fixture',
        20,
      );
      expect((await account()).credited_micros).toBe(20);
      await save('operator_policy_and_initial_manual_grant');
    }
    if (!state.checks.includes('insufficient_credits_denied_before_supplier')) {
      const r = await callModel('credit-before-admission', 429);
      expect((await r.json()).error.code).toBe('organization_credits_exhausted');
      expect((await stats()).calls).toBe(0);
      expect(await usage()).toHaveLength(0);
      await save('insufficient_credits_denied_before_supplier');
    }
    if (!state.checks.includes('lost_grant_replays_same_key_without_double_credit')) {
      await openOrganization();
      const existing = (await entries()).find(
        (v: { source_event_id: string }) => v.source_event_id === sources.second,
      );
      if (!existing) {
        await fillEntry(
          'grant',
          '0.000050',
          sources.second,
          'Synthetic lost response operator credit fixture',
        );
        let intercepted = false;
        await page.route('**/api/v1/platform/ai-credits/organizations/*/entries', async (route) => {
          if (intercepted || route.request().method() !== 'POST') {
            await route.continue();
            return;
          }
          intercepted = true;
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          const receipt = await response.json();
          state.second = {
            body: route.request().postDataJSON(),
            key: route.request().headers()['idempotency-key'],
            entry_id: receipt.entry_id,
          };
          await save();
          await route.abort('failed');
        });
        await page.getByRole('button', { name: '提交 AI 额度记录', exact: true }).click();
        await expect(
          page.getByRole('button', { name: '重试原额度提交', exact: true }),
        ).toBeVisible();
        const pending = page.waitForResponse(
          (r) => r.request().method() === 'POST' && r.url().endsWith(platform + '/entries'),
        );
        await page.getByRole('button', { name: '重试原额度提交', exact: true }).click();
        const replay = await pending;
        expect(replay.status()).toBe(200);
        expect(replay.headers()['idempotency-replayed']).toBe('true');
        expect(replay.request().headers()['idempotency-key']).toBe(state.second!.key);
        expect((await replay.json()).entry_id).toBe(state.second!.entry_id);
        await page.unroute('**/api/v1/platform/ai-credits/organizations/*/entries');
      } else {
        if (!state.second)
          throw Error('Original grant receipt/key missing; do not issue another grant');
        const replay = await context.request.post(platform + '/entries', {
          headers: { 'X-CSRF-Token': await csrf(), 'Idempotency-Key': state.second.key },
          data: state.second.body,
        });
        expect(replay.status()).toBe(200);
        expect(replay.headers()['idempotency-replayed']).toBe('true');
        expect((await replay.json()).entry_id).toBe(state.second.entry_id);
      }
      expect((await account()).credited_micros).toBe(70);
      expect(
        (await entries()).filter(
          (v: { source_event_id: string }) => v.source_event_id === sources.second,
        ),
      ).toHaveLength(1);
      await save('lost_grant_replays_same_key_without_double_credit');
    }
    if (!state.checks.includes('known_usage_settles_org_credit')) {
      let rows = await usage();
      expect(rows.length).toBeLessThanOrEqual(1);
      if (!rows.length) {
        expect(state.requests.known).toBeUndefined();
        const r = await callModel('credit-known-' + state.nonce, 200);
        state.requests.known = r.headers()['x-request-id'];
        expect(state.requests.known).toMatch(/^air_[a-f0-9]{24}$/);
        await save();
        expect(await r.text()).toContain('synthetic-native-reply');
        rows = await usage();
      }
      expect(rows).toHaveLength(1);
      expect(rows[0].state).toBe('completed');
      expect(rows[0].total_tokens).toBe(5);
      state.requests.known = rows[0].id;
      const a = await account();
      expect([a.credited_micros, a.spent_micros, a.reserved_micros, a.available_micros]).toEqual([
        70, 7, 0, 63,
      ]);
      await save('known_usage_settles_org_credit');
    }
    if (!state.checks.includes('accepted_unknown_retains_credit_reservation')) {
      let unknown = (await usage()).filter((v: { id: string }) => v.id !== state.requests.known);
      if (!unknown.length) {
        expect(state.requests.unknown).toBeUndefined();
        const r = await callModel('__uncertain__ credit-' + state.nonce, 200);
        state.requests.unknown = r.headers()['x-request-id'];
        expect(state.requests.unknown).toMatch(/^air_[a-f0-9]{24}$/);
        await save();
        await r.body();
        unknown = (await usage()).filter((v: { id: string }) => v.id !== state.requests.known);
      }
      expect(unknown).toHaveLength(1);
      expect(unknown[0].total_tokens).toBeNull();
      state.requests.unknown = unknown[0].id;
      const a = await account();
      expect([a.spent_micros, a.reserved_micros, a.available_micros]).toEqual([7, 32, 31]);
      await save('accepted_unknown_retains_credit_reservation');
    }
    if (!state.checks.includes('operator_statement_reconciles_credit_once')) {
      await openOrganization();
      const id = state.requests.unknown;
      if (!id) throw Error('Original unknown request missing');
      const pending = await context.request.get(root + '/requests');
      expect(pending.status()).toBe(200);
      if ((await pending.json()).items.some((v: { request_id: string }) => v.request_id === id)) {
        const statement = await context.request.get(
          fixture.upstream_url + '/statement?request_id=' + id,
        );
        expect(statement.status()).toBe(200);
        const receipt = await statement.json();
        expect(receipt.synthetic).toBe(true);
        expect(receipt.request_id).toBe(id);
        await page.getByLabel('AI 额度对账请求', { exact: true }).selectOption(id);
        await page
          .getByLabel('AI 额度对账用量', { exact: true })
          .fill(JSON.stringify(receipt.usage));
        await page.getByLabel('AI 额度对账凭证', { exact: true }).fill(evidence(receipt));
        const response = page.waitForResponse(
          (r) =>
            r.request().method() === 'POST' &&
            r.url().endsWith('/api/v1/platform/ai-credits/requests/' + id + '/reconcile'),
        );
        await page.getByRole('button', { name: '确认 AI 额度对账', exact: true }).click();
        expect((await response).status()).toBe(200);
      }
      const a = await account();
      expect([a.spent_micros, a.reserved_micros, a.available_micros]).toEqual([14, 0, 56]);
      await save('operator_statement_reconciles_credit_once');
    }
    if (!state.checks.includes('duplicate_source_refused_and_correction_preserved')) {
      const a = await account();
      const duplicate = await context.request.post(platform + '/entries', {
        headers: {
          'X-CSRF-Token': await csrf(),
          'Idempotency-Key': 'duplicate-source-' + state.nonce,
        },
        data: {
          version: a.version,
          kind: 'grant',
          amount_micros: 50,
          source_event_id: sources.second,
          reason: 'Duplicate synthetic event must not grant again',
          evidence_sha256: evidence(sources.second),
        },
      });
      expect(duplicate.status()).toBe(409);
      await openOrganization();
      await submitEntry(
        'adjustment',
        '-0.000001',
        sources.correction,
        'Synthetic explicit credit evidence correction',
        -1,
      );
      const current = await account();
      expect([
        current.credited_micros,
        current.spent_micros,
        current.reserved_micros,
        current.available_micros,
      ]).toEqual([69, 14, 0, 55]);
      await save('duplicate_source_refused_and_correction_preserved');
    }
    if (!state.checks.includes('disabled_policy_preserves_ledger_and_original_scope')) {
      await openOrganization();
      await expect(page.getByRole('region', { name: '组织 AI 额度', exact: true })).toContainText(
        sources.initial,
      );
      if ((await account()).enabled) await policy(false);
      const a = await account();
      expect([a.enabled, a.credited_micros, a.spent_micros, a.reserved_micros]).toEqual([
        false,
        69,
        14,
        0,
      ]);
      expect(await entries()).toHaveLength(8);
      expect((await stats()).calls).toBe(2);
      expect(await usage()).toHaveLength(2);
      const budget = await context.request.get(ai + '/budget');
      expect(budget.status()).toBe(200);
      const b = (await budget.json()).policy;
      expect([b.enabled, b.spent_micros, b.reserved_micros]).toEqual([false, 65, 0]);
      await save('disabled_policy_preserves_ledger_and_original_scope');
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
