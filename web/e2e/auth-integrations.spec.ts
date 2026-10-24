import { expect, test, type BrowserContext } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import path from 'node:path';

test('Approved Auth integrations: OAuth, signed events, phone UI and copied branch isolation', async ({
  page,
  context,
  browser,
  baseURL,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile || baseURL !== 'http://192.168.146.100:30788')
    throw Error('Protected exact lab fixture required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    f.project_id !== 'prj_6f3c51bd15b772e3' ||
    f.branch_id !== 'br_39167d49b9123c61' ||
    f.database !== 'owner_transfer_v1'
  )
    throw Error('Retained source fixture required');
  const source = JSON.parse(await readFile(f.source_credentials_file, 'utf8'));
  const integration = JSON.parse(await readFile(f.integrations_credentials_file, 'utf8'));
  if (
    integration.origin !== 'http://192.168.146.100:30984' ||
    !Array.isArray(f.provider_ids) ||
    f.provider_ids.length !== 3
  )
    throw Error('Exact laboratory integrations required');
  const state: any = f.resume_file
    ? JSON.parse(await readFile(f.resume_file, 'utf8'))
    : {
        project_id: f.project_id,
        source_branch_id: f.branch_id,
        nonce: String(Date.now()),
        checks: [],
        running: '',
        operations: {},
        requests: {},
        password: source.password,
        member_password: randomBytes(30).toString('base64url'),
        managed_password: randomBytes(30).toString('base64url'),
        platform_password: randomBytes(30).toString('base64url'),
        users: { admin: source.user },
      };
  if (state.project_id !== f.project_id || state.source_branch_id !== f.branch_id || state.running)
    throw Error('Inspect original pending mutation before resuming');
  const root = '/api/v1/projects/' + f.project_id,
    authPath = () => root + '/branches/' + state.branch_id + '/auth',
    appPath = () => '/auth/v1/' + state.branch_id;
  let result = 'running',
    lastApp = 0;
  const extra: BrowserContext[] = [];
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  async function save() {
    await writeFile(
      path.join(privateDir!, 'auth-advanced-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks: state.checks,
        project_id: state.project_id,
        branch_id: state.branch_id,
        writer_id: state.writer_id,
        operations: state.operations,
        root_auth_unchanged: state.root_auth_unchanged || false,
        credentials_exported: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function step(name: string, work: () => Promise<void>) {
    if (state.checks.includes(name)) return;
    state.running = name;
    await save();
    await work();
    state.checks.push(name);
    state.running = '';
    await save();
  }
  async function headers() {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(url: string) {
    const response = await context.request.get(url, { timeout: 190000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Original operation failed: ' + id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function mutate(
    url: string,
    label: string,
    data?: object,
    version?: number,
    method = 'POST',
  ) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return get(root + '/operations/' + state.operations[label]);
    }
    state.requests[label] ??= {
      url,
      method,
      data,
      version,
      key: 'a03-advanced-' + state.nonce + '-' + label,
    };
    await save();
    const request = state.requests[label];
    const response = await context.request.fetch(request.url, {
      method: request.method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': request.key,
        ...(request.version === undefined ? {} : { 'If-Match': `"${request.version}"` }),
      },
      ...(request.data === undefined ? {} : { data: request.data }),
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await observe(value.operation.id);
    return get(root + '/operations/' + value.operation.id);
  }
  async function visit() {
    await page.goto('/?auth_branch=' + state.branch_id + '#/projects/' + f.project_id + '/auth');
    await expect(page.getByLabel('Auth 分支', { exact: true })).toHaveValue(state.branch_id);
  }
  // UI writes also refresh a list. Stay inside the existing 30/minute limiter.
  async function pace() {
    const delay = lastApp + 4250 - Date.now();
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    lastApp = Date.now();
  }
  async function app(action: string, data?: object, client = context) {
    await pace();
    if (action === 'sign-in/email') {
      // The maintained library resets this special bucket after ten idle
      // seconds. Keep its limit; never retry an already committed admin write.
      if (Date.now() - (state.last_login_at || 0) >= 10500) state.login_burst_count = 0;
      if ((state.login_burst_count || 0) >= 3) {
        const delay = (state.last_login_at || 0) + 10500 - Date.now();
        if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
        state.login_burst_count = 0;
      }
      state.last_login_at = Date.now();
      state.login_burst_count = (state.login_burst_count || 0) + 1;
      await save();
      lastApp = Date.now();
    }
    const response = await client.request.fetch(appPath() + '/' + action, {
      method: data === undefined ? 'GET' : 'POST',
      headers: { Origin: baseURL! },
      ...(data === undefined ? {} : { data }),
      timeout: 190000,
    });
    const value = await response.json().catch(() => null);
    return { status: response.status(), value };
  }
  async function signIn(client = context, kind = 'admin') {
    const user = state.users[kind],
      password = kind === 'admin' ? state.password : state.member_password;
    expect((await app('sign-in/email', { email: user.email, password }, client)).status).toBe(200);
  }

  async function fixtureRequest(action: string, data?: object) {
    const response = await context.request.fetch(integration.origin + '/fixture/' + action, {
      method: data === undefined ? 'GET' : 'POST',
      headers: { Authorization: 'Bearer ' + integration.receiptToken },
      ...(data === undefined ? {} : { data }),
    });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function receipts(branchID = state.branch_id) {
    return (await fixtureRequest('events?branch=' + branchID)).items as any[];
  }
  async function sms() {
    const values = await receipts();
    const record = values
      .filter((e) => e.event.event_type === 'send.otp' && e.event.user.phone_number === state.phone)
      .at(-1);
    expect(record?.verified_signature).toBe(true);
    return record.event.event_data.otp_code as string;
  }
  async function press(
    name: string,
    action: string,
    scope: { getByRole: typeof page.getByRole } = page,
  ) {
    await pace();
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === appPath() + '/' + action,
    );
    await scope.getByRole('button', { name, exact: true }).click();
    return waiting;
  }
  async function currentUI() {
    const response = await press('检查应用会话', 'get-session');
    expect(response.status()).toBe(200);
    const value = await response.json();
    expect(value.user.id).toBe(state.users.admin.id);
    return value;
  }
  async function enableUI(label: string) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      await visit();
      return;
    }
    await visit();
    await page.getByLabel('Auth 数据库', { exact: true }).fill(f.database);
    await page.getByLabel('启用应用 TOTP 双重认证', { exact: true }).check();
    await page.getByLabel('迁移父分支双重认证', { exact: true }).check();
    await page.getByLabel('启用应用管理员接口', { exact: true }).check();
    await page.getByLabel('启用应用组织管理', { exact: true }).check();
    await page.getByLabel('应用组织用户配额', { exact: true }).fill('10');
    await page.getByLabel('应用组织成员配额', { exact: true }).fill('10');
    await page.getByLabel('Auth 通知服务', { exact: true }).selectOption('lab-events');
    for (const event of [
      'send.otp',
      'send.magic_link',
      'user.before_create',
      'user.created',
      'phone_number.verified',
      'organization.invitation.created',
      'organization.invitation.accepted',
    ])
      await page.getByLabel('订阅 Auth 事件 ' + event, { exact: true }).check();
    await page.getByLabel('Auth 通知超时秒数', { exact: true }).fill('5');
    await page.getByLabel('启用应用手机号登录', { exact: true }).check();
    await page.getByLabel('短信验证码有效秒数', { exact: true }).fill('60');
    await page.getByLabel('启用应用邮箱验证码', { exact: true }).check();
    await page.getByLabel('启用应用 Magic Link', { exact: true }).check();
    for (const id of f.provider_ids)
      await page.getByLabel('启用 OAuth ' + id, { exact: true }).check();
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === authPath() && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(202);
    state.operations[label] = (await response.json()).operation.id;
    await save();
    await observe(state.operations[label]);
    await expect(page.getByRole('button', { name: '禁用 Auth', exact: true })).toBeEnabled();
  }
  async function social(provider: string, link: boolean) {
    await pace();
    const navigation = page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 60000 });
    await page
      .getByTestId('auth-advanced-panel')
      .getByRole('button', {
        name: (link ? '绑定 ' : '') + provider + (link ? '' : ' 登录'),
        exact: true,
      })
      .click();
    await navigation;
    await expect(page).toHaveURL(new RegExp('auth_branch=' + state.branch_id), { timeout: 60000 });
    await expect(page.getByTestId('auth-advanced-panel')).toBeVisible();
    lastApp = Date.now();
    expect(new URL(page.url()).searchParams.get('error')).toBeNull();
  }
  async function disabledAndSuspended(label: string) {
    const auth = await get(authPath());
    if (auth.state !== 'disabled')
      await mutate(authPath(), label + '-disable', undefined, auth.generation, 'DELETE');
    await mutate(root + '/endpoints/' + state.writer_id + '/suspend', label + '-suspend', {});
    await expect
      .poll(async () => (await get(root + '/endpoints/' + state.writer_id)).observed_state, {
        timeout: 180000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  async function shot(name: string) {
    await page.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  try {
    await save();
    page.setDefaultTimeout(30000);
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (
      state.checks.includes('owned_parent_policy_and_signed_phone_binding') &&
      !state.checks.includes('parent_disabled_before_physical_copy')
    ) {
      await signIn();
      await visit();
      await currentUI();
    }
    await step('owned_parent_branch_preserves_retained_sources', async () => {
      state.original_source_auth = await get(root + '/branches/' + f.branch_id + '/auth');
      state.original_root_auth = await get(root + '/branches/br_6f3c51bd15b772e3/auth');
      expect(state.original_source_auth.generation).toBe(6);
      expect(state.original_source_auth.state).toBe('disabled');
      const op = await mutate(root + '/branches', 'create-parent', {
        name: 'ci-auth-integrations-' + state.nonce,
        parent_branch_id: f.branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: {
          min_cpu_milli: 1000,
          max_cpu_milli: 1000,
          min_memory_mib: 1024,
          max_memory_mib: 1024,
        },
      });
      state.branch_id = op.resource_id;
      state.writer_id = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.branch_id && e.endpoint_type === 'read_write',
      )?.id;
      if (!state.writer_id) throw Error('Owned writer required');
      state.parent_branch_id = state.branch_id;
      state.parent_writer_id = state.writer_id;
      state.phone = '+1555' + state.nonce.slice(-8);
    });
    await step('owned_parent_policy_and_signed_phone_binding', async () => {
      await enableUI('enable-parent');
      await signIn();
      await visit();
      await currentUI();
      const spec = (await get(authPath())).spec;
      expect(spec.oauth_providers).toEqual(f.provider_ids);
      expect(spec.webhook_events).toHaveLength(7);
      expect(spec.phone_number_enabled).toBe(true);
      const panel = page.getByTestId('auth-advanced-panel');
      await panel.getByLabel('应用手机号码', { exact: true }).fill(state.phone);
      expect((await press('发送短信验证码', 'phone-number/send-otp', panel)).status()).toBe(200);
      await panel.getByLabel('应用短信验证码', { exact: true }).fill(await sms());
      expect((await press('验证并绑定手机', 'phone-number/verify', panel)).status()).toBe(200);
      await expect(panel.getByTestId('auth-advanced-notice')).toContainText('手机已验证并绑定');
      const keys = await app('.well-known/jwks.json');
      expect(keys.status).toBe(200);
      state.parent_webhook_kid = keys.value.keys[0].kid;
      await shot('phone-bound-and-event-policy');
    });
    await step('UI_signup_validation_and_custom_email_deliveries', async () => {
      await page.getByLabel('应用姓名', { exact: true }).fill('Denied laboratory signup');
      await page
        .getByLabel('应用邮箱', { exact: true })
        .fill('deny-' + state.nonce + '@example.test');
      await page.getByLabel('应用密码', { exact: true }).fill(state.managed_password);
      expect((await press('注册应用用户', 'sign-up/email')).status()).toBe(403);
      expect((await app('get-session')).value.user.id).toBe(state.users.admin.id);
      const otp = page.getByTestId('auth-otp-panel');
      await otp.getByLabel('验证码邮箱', { exact: true }).fill(state.users.admin.email);
      await otp.getByLabel('验证码用途', { exact: true }).selectOption('email-verification');
      expect((await press('发送应用验证码', 'email-otp/send-verification-otp', otp)).status()).toBe(
        200,
      );
      const magic = page.getByTestId('auth-magic-link-panel');
      await magic.getByLabel('免密码登录邮箱', { exact: true }).fill(state.users.admin.email);
      expect((await press('发送免密码登录链接', 'sign-in/magic-link', magic)).status()).toBe(200);
      const values = await receipts();
      expect(
        values.some(
          (e) =>
            e.verified_signature &&
            e.event.event_type === 'send.otp' &&
            e.event.event_data.delivery_preference === 'email',
        ),
      ).toBe(true);
      expect(
        values.some(
          (e) =>
            e.verified_signature &&
            e.event.event_type === 'send.magic_link' &&
            new URL(e.event.event_data.url).pathname.startsWith(appPath() + '/'),
        ),
      ).toBe(true);
    });
    await step('UI_explicit_OAuth_bindings_and_verified_profile_adapters', async () => {
      await fixtureRequest('identity', {
        subject: 'owned-' + state.nonce,
        email: state.users.admin.email,
        name: 'Owned laboratory identity',
      });
      await social('lab-oidc', true);
      await currentUI();
      expect(
        (
          await press('读取登录方式', 'list-accounts', page.getByTestId('auth-advanced-panel'))
        ).status(),
      ).toBe(200);
      await expect(
        page.getByRole('button', { name: '解除绑定 lab-oidc', exact: true }),
      ).toBeVisible();
      await social('lab-github', true);
      await currentUI();
      expect(
        (
          await press('读取登录方式', 'list-accounts', page.getByTestId('auth-advanced-panel'))
        ).status(),
      ).toBe(200);
      expect(
        (
          await press(
            '解除绑定 lab-github',
            'unlink-account',
            page.getByTestId('auth-advanced-panel'),
          )
        ).status(),
      ).toBe(200);
      await social('lab-vercel', true);
      await currentUI();
      expect(
        (
          await press('读取登录方式', 'list-accounts', page.getByTestId('auth-advanced-panel'))
        ).status(),
      ).toBe(200);
      await expect(
        page.getByRole('button', { name: '解除绑定 lab-vercel', exact: true }),
      ).toBeVisible();
      await shot('oauth-owned-bindings');
    });
    await step('signed_user_and_organization_invitation_events', async () => {
      const email = 'auth-event-member-' + state.nonce + '@example.test';
      const created = await app('admin/create-user', {
        name: 'Owned event recipient',
        email,
        password: state.member_password,
        role: 'user',
        data: { emailVerified: true },
      });
      expect(created.status).toBe(200);
      state.users.member = { id: created.value.user.id, email };
      await save();
      const panel = page.getByRole('region', { name: '应用组织', exact: true });
      await panel.getByLabel('应用组织名称', { exact: true }).fill('Events ' + state.nonce);
      await panel.getByLabel('应用组织标识', { exact: true }).fill('events-' + state.nonce);
      const org = await press('创建应用组织', 'organization/create', panel);
      expect(org.status()).toBe(200);
      state.organization_id = (await org.json()).id;
      await save();
      await panel.getByLabel('组织邀请邮箱', { exact: true }).fill(email);
      const invitation = await press('邀请应用组织成员', 'organization/invite-member', panel);
      expect(invitation.status()).toBe(200);
      state.invitation_id = (await invitation.json()).id;
      await save();
      const recipient = await browser.newContext({ baseURL });
      extra.push(recipient);
      expect(
        (await app('sign-in/email', { email, password: state.member_password }, recipient)).status,
      ).toBe(200);
      const accepted = await app(
        'organization/accept-invitation',
        { invitationId: state.invitation_id },
        recipient,
      );
      expect(accepted.status).toBe(200);
      state.accepted_member_id = accepted.value.member.id;
      await save();
      await expect
        .poll(
          async () => {
            const values = await receipts();
            return values.some(
              (e) =>
                e.event.event_type === 'organization.invitation.accepted' &&
                e.event.event_data.member_id === state.accepted_member_id,
            );
          },
          { timeout: 30000, intervals: [1000, 2000] },
        )
        .toBe(true);
      const values = await receipts();
      expect(new Set(values.map((e) => e.event.event_type)).size).toBe(7);
      expect(values.every((e) => e.verified_signature)).toBe(true);
      expect(
        values.some(
          (e) => e.event.event_type === 'user.created' && e.event.user.id === state.users.member.id,
        ),
      ).toBe(true);
      state.seven_event_types_verified = true;
      expect(
        (
          await press('读取 Auth 事件', 'webhooks/events', page.getByTestId('auth-advanced-panel'))
        ).status(),
      ).toBe(200);
      await shot('signed-event-metadata');
    });
    await step('UI_phone_login_and_native_OAuth_login', async () => {
      if (!state.phone_login_verified) {
        expect((await press('退出应用', 'sign-out')).status()).toBe(200);
        const panel = page.getByTestId('auth-advanced-panel');
        await expect(
          panel.getByRole('button', { name: '手机验证码登录', exact: true }),
        ).toBeVisible();
        await panel.getByLabel('应用手机号码', { exact: true }).fill(state.phone);
        expect((await press('发送短信验证码', 'phone-number/send-otp', panel)).status()).toBe(200);
        await panel.getByLabel('应用短信验证码', { exact: true }).fill(await sms());
        expect((await press('手机验证码登录', 'phone-number/verify', panel)).status()).toBe(200);
        await currentUI();
        state.phone_login_verified = true;
        await save();
      }
      expect((await press('退出应用', 'sign-out')).status()).toBe(200);
      await social('lab-oidc', false);
      await currentUI();
    });
    await step('parent_disabled_before_physical_copy', async () => {
      expect((await app('phone-number/send-otp', { phoneNumber: state.phone })).status).toBe(200);
      state.pending_parent_code = await sms();
      await save();
      state.parent_spec = (await get(authPath())).spec;
      await disabledAndSuspended('parent-before-copy');
    });
    await step('physical_child_has_distinct_signatures_and_rejects_copied_SMS', async () => {
      const op = await mutate(root + '/branches', 'create-child', {
        name: 'ci-auth-integrations-child-' + state.nonce,
        parent_branch_id: state.parent_branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: {
          min_cpu_milli: 1000,
          max_cpu_milli: 1000,
          min_memory_mib: 1024,
          max_memory_mib: 1024,
        },
      });
      state.branch_id = op.resource_id;
      state.writer_id = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.branch_id && e.endpoint_type === 'read_write',
      )?.id;
      state.child_branch_id = state.branch_id;
      state.child_writer_id = state.writer_id;
      await save();
      await enableUI('enable-child');
      expect(
        (
          await app('phone-number/verify', {
            phoneNumber: state.phone,
            code: state.pending_parent_code,
          })
        ).status,
      ).toBe(400);
      const keys = await app('.well-known/jwks.json');
      expect(keys.status).toBe(200);
      expect(keys.value.keys[0].kid).not.toBe(state.parent_webhook_kid);
      await signIn();
      await visit();
      await currentUI();
      const accounts = await app('list-accounts');
      expect(accounts.status).toBe(200);
      expect(accounts.value.some((a: any) => a.providerId === 'lab-oidc')).toBe(true);
      const panel = page.getByTestId('auth-advanced-panel');
      expect((await press('读取登录方式', 'list-accounts', panel)).status()).toBe(200);
      expect((await press('解除绑定 lab-oidc', 'unlink-account', panel)).status()).toBe(200);
      await disabledAndSuspended('child-final');
    });
    await step('parent_new_generation_retains_bindings_and_discards_SMS_challenge', async () => {
      state.branch_id = state.parent_branch_id;
      state.writer_id = state.parent_writer_id;
      await save();
      if (state.operations['parent-reenable']) {
        await observe(state.operations['parent-reenable']);
      } else {
        const disabled = await get(authPath());
        expect(disabled.state).toBe('disabled');
        await mutate(authPath(), 'parent-reenable', state.parent_spec, disabled.generation);
      }
      expect(
        (
          await app('phone-number/verify', {
            phoneNumber: state.phone,
            code: state.pending_parent_code,
          })
        ).status,
      ).toBe(400);
      await visit();
      await social('lab-oidc', false);
      await currentUI();
      expect((await app('list-accounts')).value.some((a: any) => a.providerId === 'lab-oidc')).toBe(
        true,
      );
      state.parent_native_preserved = true;
      await disabledAndSuspended('parent-final');
    });
    await step('owned_services_off_original_Auth_specs_and_generations_preserved', async () => {
      for (const id of [state.parent_branch_id, state.child_branch_id])
        expect((await get(root + '/branches/' + id + '/auth')).state).toBe('disabled');
      for (const [id, original] of [
        [f.branch_id, state.original_source_auth],
        ['br_6f3c51bd15b772e3', state.original_root_auth],
      ]) {
        const current = await get(root + '/branches/' + id + '/auth');
        expect({
          generation: current.generation,
          state: current.state,
          spec: current.spec,
        }).toEqual({ generation: original.generation, state: original.state, spec: original.spec });
      }
      state.root_auth_unchanged = true;
    });
    result = 'pass';
    await save();
  } finally {
    await save();
    for (const client of extra) await client.close();
  }
});
