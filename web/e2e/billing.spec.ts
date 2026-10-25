import { expect, test } from '@playwright/test';
import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Owned billing: actual checkout, signed receipts, reversals and original recovery', async ({
  page,
  context,
}, info) => {
  test.setTimeout(600000);
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (process.platform !== 'linux' || !fixtureFile || !privateDir || !adminFile)
    throw Error('Protected Linux billing fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    !fixture.new_owned_org ||
    !/^org_[a-f0-9]{24}$/.test(fixture.org_id) ||
    !fixture.org_name.startsWith('ci-billing-') ||
    !fixture.synthetic ||
    !fixture.fixture_admin_key
  )
    throw Error('Only the newly owned synthetic billing scope is allowed');
  const state: any = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : { nonce: String(Date.now()), checks: [], requests: {} };
  if (state.running) throw Error('Observe the original unfinished step before resuming');
  let result = 'running';
  const root = '/api/v1/organizations/' + fixture.org_id + '/billing',
    credits = '/api/v1/organizations/' + fixture.org_id + '/ai-credits';
  const panel = () => page.getByRole('region', { name: '组织付款与账单', exact: true });
  async function save() {
    await writeFile(
      path.join(privateDir!, 'billing-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks: state.checks,
        org_id: fixture.org_id,
        invoice_id: state.invoice_id,
        recovery_invoice_id: state.recovery_invoice_id,
        synthetic: true,
        commercial_collection: false,
        real_card_data: false,
        AI_supplier_calls: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function step(label: string, work: () => Promise<void>) {
    if (state.checks.includes(label)) return;
    state.running = label;
    await save();
    await work();
    state.checks.push(label);
    state.running = '';
    await save();
  }
  async function get(url: string) {
    const response = await context.request.get(url, { timeout: 45000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function headers(key: string) {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Current Console session required');
    return { 'X-CSRF-Token': decodeURIComponent(csrf), 'Idempotency-Key': key };
  }
  async function request(
    label: string,
    method: string,
    url: string,
    body: unknown,
    status: number,
  ) {
    state.requests[label] ??= { method, url, body, key: 'billing-' + state.nonce + '-' + label };
    await save();
    const original = state.requests[label],
      response = await context.request.fetch(original.url, {
        method: original.method,
        data: original.body,
        headers: await headers(original.key),
        timeout: 60000,
      });
    expect(response.status()).toBe(status);
    return response.json();
  }
  async function fixtureCall(route: string, input?: unknown) {
    const response = await context.request.fetch(
      fixture.fixture_origin + '/fixture/admin/' + route,
      {
        method: input ? 'POST' : 'GET',
        data: input,
        headers: { Authorization: 'Bearer ' + fixture.fixture_admin_key },
        timeout: 30000,
      },
    );
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function ui(label: string, button: string, url: string, status: number, within = panel()) {
    if (state.requests[label]) return request(label, 'POST', url, {}, status);
    const pattern = '**' + url;
    await page.route(pattern, async (route) => {
      if (route.request().method() !== 'POST') return route.continue();
      state.requests[label] = {
        method: 'POST',
        url,
        body: route.request().postDataJSON(),
        key: route.request().headers()['idempotency-key'],
      };
      await save();
      await route.continue();
    });
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === url && r.request().method() === 'POST',
    );
    await within.getByRole('button', { name: button, exact: true }).click();
    const response = await waiting;
    await page.unroute(pattern);
    expect(response.status()).toBe(status);
    return response.json();
  }
  async function visit() {
    await page.goto('/#/organization');
    await expect(panel()).toBeVisible();
    await expect(panel().getByText(/本地合成验收/)).toBeVisible();
  }
  async function account(amount: number) {
    await expect
      .poll(async () => (await get(credits)).account.credited_micros, {
        timeout: 60000,
        intervals: [1000, 2000],
      })
      .toBe(amount);
    const current = (await get(credits)).account;
    expect([current.spent_micros, current.reserved_micros]).toEqual([0, 0]);
  }
  async function invoice(id: string, stateName: string) {
    await expect
      .poll(async () => (await get(root + '/invoices/' + id)).invoice.checkout_state, {
        timeout: 60000,
        intervals: [1000, 2000],
      })
      .toBe(stateName);
  }
  try {
    const password = (await readFile(adminFile, 'utf8')).trim();
    const login = await context.request.post('/auth/login', {
      data: { username: 'admin', password },
    });
    expect(login.status()).toBe(200);
    await page.goto('/');
    await page.getByLabel('切换组织', { exact: true }).click();
    await page.getByRole('option', { name: fixture.org_name + ' · admin', exact: true }).click();
    await step('owned_credit_account_enabled_without_manual_funding', async () => {
      const before = (await get(credits)).account;
      expect(before.version).toBe(0);
      await request(
        'enable-credit-account',
        'PUT',
        '/api/v1/platform/ai-credits/organizations/' + fixture.org_id,
        { version: before.version, enabled: true },
        200,
      );
      await account(0);
    });
    await step('GUI_fixed_offer_creates_one_original_invoice_and_same_key_receipt', async () => {
      await visit();
      await panel().getByLabel('固定额度方案', { exact: true }).selectOption(fixture.offer_id);
      const value = await ui('create-original', '创建原付款订单', root + '/invoices', 201);
      state.invoice_id = value.invoice_id;
      await save();
      const replay = await request('create-original', 'POST', root + '/invoices', {}, 201);
      expect(replay.invoice_id).toBe(state.invoice_id);
      const invoices = await get(root + '/invoices');
      expect(invoices.items).toHaveLength(1);
      expect(invoices.items[0].amount_minor).toBe(125);
      await account(0);
    });
    await step('GUI_checkout_separate_from_funding_and_actual_hosted_confirmation', async () => {
      await visit();
      const row = panel().getByRole('row').filter({ hasText: state.invoice_id });
      await ui(
        'queue-original',
        '请求原订单收银台',
        root + '/invoices/' + state.invoice_id + '/checkout',
        202,
        row,
      );
      await invoice(state.invoice_id, 'ready');
      await account(0);
      await panel().getByRole('button', { name: '刷新付款账单', exact: true }).click();
      await row.getByRole('button', { name: '读取原收银台链接', exact: true }).click();
      const link = panel().getByRole('link', { name: '前往原订单收银台', exact: true });
      await expect(link).toBeVisible();
      const href = await link.getAttribute('href');
      if (!href) throw Error('Original hosted link required');
      const hosted = await context.newPage();
      await hosted.goto(href);
      await hosted.getByRole('button', { name: '确认合成付款', exact: true }).click();
      await expect(hosted.getByRole('status')).toHaveText('合成付款回执已提交');
      await hosted.close();
      await account(1000000);
      expect((await get(root + '/invoices/' + state.invoice_id)).invoice.paid).toBe(true);
      const upstream = await fixtureCall('state');
      const original = Object.values(upstream.sessions).find(
        (v: any) => v.metadata.neon_invoice === state.invoice_id,
      ) as any;
      state.session_id = original.id;
      state.paid_event = upstream.events.find(
        (v: any) => v.type === 'checkout.session.completed',
      ).id;
      await save();
    });
    await step('duplicate_signed_delivery_retains_original_ledger_and_GUI_receipt', async () => {
      const before = await get(credits + '/ledger');
      const replay = await fixtureCall('replay', {
        session_id: state.session_id,
        event_id: state.paid_event,
      });
      expect(replay.status).toBe(200);
      await account(1000000);
      expect((await get(credits + '/ledger')).items).toEqual(before.items);
      await visit();
      await panel()
        .getByRole('row')
        .filter({ hasText: state.invoice_id })
        .getByRole('button', { name: '查看付款回执', exact: true })
        .click();
      await expect(
        panel()
          .getByLabel('原付款回执')
          .getByText(/checkout.session.completed · applied/),
      ).toBeVisible();
    });
    await step('verified_partial_refund_dispute_and_won_restore_exact_credits', async () => {
      await fixtureCall('refund', { session_id: state.session_id, amount_minor: 25 });
      await account(800000);
      const dispute = await fixtureCall('dispute', {
        session_id: state.session_id,
        amount_minor: 50,
        status: 'under_review',
      });
      state.dispute_id = dispute.dispute_id;
      await save();
      await account(400000);
      await fixtureCall('dispute', { session_id: state.session_id, status: 'won' });
      await account(800000);
      const value = (await get(root + '/invoices/' + state.invoice_id)).invoice;
      expect([value.refunded_minor, value.dispute_state, value.applied_credit_micros]).toEqual([
        25,
        'won',
        800000,
      ]);
    });
    await step(
      'unknown_checkout_retains_original_and_GUI_operator_recovers_without_POST',
      async () => {
        await fixtureCall('unknown-next', {});
        await visit();
        await panel().getByLabel('固定额度方案', { exact: true }).selectOption(fixture.offer_id);
        const created = await ui('create-recovery', '创建原付款订单', root + '/invoices', 201);
        state.recovery_invoice_id = created.invoice_id;
        await save();
        await visit();
        const row = panel().getByRole('row').filter({ hasText: state.recovery_invoice_id });
        await ui(
          'queue-recovery',
          '请求原订单收银台',
          root + '/invoices/' + state.recovery_invoice_id + '/checkout',
          202,
          row,
        );
        await invoice(state.recovery_invoice_id, 'uncertain');
        const upstream = await fixtureCall('state');
        const original = Object.values(upstream.sessions).find(
          (v: any) => v.metadata.neon_invoice === state.recovery_invoice_id,
        ) as any;
        expect(upstream.posts).toBe(2);
        state.recovery_session_id = original.id;
        await save();
        await visit();
        await panel()
          .getByLabel('待核对原订单', { exact: true })
          .selectOption(state.recovery_invoice_id);
        await panel().getByLabel('支付方原记录 ID', { exact: true }).fill(original.id);
        await panel()
          .getByLabel('付款核对原因', { exact: true })
          .fill('Recover the originally dispatched synthetic checkout');
        await panel()
          .getByLabel('付款证据 SHA-256', { exact: true })
          .fill(createHash('sha256').update(JSON.stringify(original)).digest('hex'));
        await ui(
          'reconcile-original',
          '核对原支付方记录',
          '/api/v1/platform/billing/organizations/' +
            fixture.org_id +
            '/invoices/' +
            state.recovery_invoice_id +
            '/reconcile',
          202,
        );
        await invoice(state.recovery_invoice_id, 'ready');
        await account(800000);
        expect((await fixtureCall('state')).posts).toBe(2);
      },
    );
    await step(
      'full_refund_preserves_payment_history_and_disabled_account_denies_new_purchase',
      async () => {
        await fixtureCall('refund', { session_id: state.session_id, amount_minor: 125 });
        await account(0);
        const current = (await get(credits)).account;
        await request(
          'disable-owned-credit-account',
          'PUT',
          '/api/v1/platform/ai-credits/organizations/' + fixture.org_id,
          { version: current.version, enabled: false },
          200,
        );
        const catalog = await get(root);
        await request(
          'deny-disabled-purchase',
          'POST',
          root + '/invoices',
          { offer_id: catalog.offers[0].id, offer_revision: catalog.offers[0].revision },
          409,
        );
        const retained = await get(root + '/invoices');
        expect(retained.items).toHaveLength(2);
        expect(
          retained.items.find((v: any) => v.invoice_id === state.invoice_id).refunded_minor,
        ).toBe(125);
        expect((await fixtureCall('state')).posts).toBe(2);
      },
    );
    result = 'pass';
  } catch (e) {
    result = 'fail';
    throw e;
  } finally {
    await save();
  }
});
