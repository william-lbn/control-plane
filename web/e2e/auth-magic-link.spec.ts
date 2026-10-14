import { test, expect, request, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('actual UI Magic Link signs in, rejects replay and controls new registration', async ({
  page,
  context,
  baseURL,
}, info) => {
  page.setDefaultTimeout(30000);
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    mailFile = process.env.NEON_E2E_MAILBOX_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!ownerFile || !mailFile || !adminFile || !privateDir || !baseURL)
    throw new Error('Protected Linux owner/mail inputs required');
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    role: string;
    password_file: string;
  };
  if (owner.database !== 'owner_transfer_v1' || owner.role !== 'owner_new_v1')
    throw new Error('Retained accepted owner fixture required');
  const box = JSON.parse(await readFile(mailFile, 'utf8')) as {
    url: string;
    username: string;
    password_file: string;
  };
  if (
    !['http://neon-auth-mailbox:8025', 'http://127.0.0.1:18025'].includes(box.url) ||
    box.username !== 'local-mail-reader'
  )
    throw new Error('Isolated mailbox fixture required');
  const password = (await readFile(box.password_file, 'utf8')).trim();
  const inbox = await request.newContext({
    baseURL: box.url,
    extraHTTPHeaders: {
      Authorization: 'Basic ' + Buffer.from(box.username + ':' + password).toString('base64'),
    },
    maxRedirects: 0,
  });
  const project = owner.project_id,
    branch = owner.branch_id,
    control = `/api/v1/projects/${project}/branches/${branch}/auth`,
    app = `/auth/v1/${branch}`;
  const originalInput = process.env.NEON_E2E_AUTH_MAGIC_ORIGINAL_INPUT_FILE;
  const inputs = originalInput
    ? (JSON.parse(await readFile(originalInput, 'utf8')) as {
        email: string;
        pwEmail: string;
        oldPassword: string;
        newPassword: string;
        original_operation_id?: string;
      })
    : null;
  const email = inputs?.email ?? 'magic-native-' + Date.now() + '@example.test',
    pwEmail = inputs?.pwEmail ?? 'magic-denied-' + Date.now() + '@example.test',
    oldPassword = inputs?.oldPassword ?? randomBytes(24).toString('base64url'),
    newPassword = inputs?.newPassword ?? randomBytes(24).toString('base64url');
  const checks: string[] = [],
    operations: string[] = [],
    messages: string[] = [],
    accounts: string[] = [];
  let result = 'running',
    mailCount = 0;
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        message_ids: messages,
        account_ids: accounts,
        project_id: project,
        branch_id: branch,
        actual_smtp: true,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function operation(response: Response) {
    expect(response.status()).toBe(202);
    const data = await response.json();
    operations.push(data.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${data.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Original OTP operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function action(button: string, route: string) {
    const waiting = page.waitForResponse((r) => new URL(r.url()).pathname === app + '/' + route, {
      timeout: 30000,
    });
    await page.getByRole('button', { name: button, exact: true }).click();
    return waiting;
  }
  async function code(recipient: string, subject: string) {
    let id = '';
    await expect
      .poll(
        async () => {
          const r = await inbox.get('/api/v1/messages?limit=100');
          expect(r.status()).toBe(200);
          const list = await r.json();
          const item = list.messages.find(
            (m: { ID: string; Subject: string; To: { Address: string }[] }) =>
              m.Subject === subject &&
              m.To.some((t) => t.Address === recipient) &&
              !messages.includes(m.ID),
          );
          id = item?.ID ?? '';
          return !!item;
        },
        { timeout: 45000 },
      )
      .toBe(true);
    const response = await inbox.get('/api/v1/message/' + id);
    expect(response.status()).toBe(200);
    const body = await response.json();
    await writeFile(
      path.join(privateDir!, `magic-mail-${++mailCount}.private.json`),
      JSON.stringify(body),
      { flag: 'wx', mode: 0o600 },
    );
    messages.push(id);
    const value = body.Text.match(/https?:\/\/[^\s]+/)?.[0];
    if (!value) throw new Error('Actual private Magic Link absent');
    const url = new URL(value);
    if (url.origin !== new URL(baseURL!).origin || url.pathname !== app + '/magic-link/verify')
      throw new Error('Original branch mail route mismatch');
    return url.href;
  }
  try {
    await save();
    await writeFile(
      path.join(privateDir, 'magic-accounts.private.json'),
      JSON.stringify({ email, pwEmail, oldPassword, newPassword }),
      { flag: 'wx', mode: 0o600 },
    );
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    const protocol = await context.request.get(
      '/api/application-auth-openapi.json?branch_id=' + branch,
    );
    expect(protocol.status()).toBe(200);
    expect((await protocol.json()).paths['/sign-in/magic-link'].post).toBeDefined();
    await page.goto(`/#/projects/${project}/auth`);
    const state = await context.request.get(control);
    expect(state.status()).toBe(200);
    const originalState = await state.json();
    if (originalState.state === 'active') {
      if (!inputs?.original_operation_id)
        throw new Error('Unknown existing Auth mutation; preserve it');
      expect(originalState.spec.database).toBe(owner.database);
      expect(originalState.spec.magic_link_enabled).toBe(true);
      expect(originalState.spec.magic_link_allow_registration).toBe(true);
      expect(originalState.spec.magic_link_expires_in).toBe(300);
      const existing = await context.request.get(
        `/api/v1/projects/${project}/operations/${inputs.original_operation_id}`,
      );
      expect(existing.status()).toBe(200);
      expect((await existing.json()).state).toBe('succeeded');
      operations.push(inputs.original_operation_id);
    } else {
      await page.getByLabel('Auth 数据库', { exact: true }).fill(owner.database);
      await page.getByLabel('启用应用 Magic Link', { exact: true }).check();
      await page.getByLabel('Magic Link 允许新用户注册', { exact: true }).check();
      await page.getByLabel('登录链接有效分钟数', { exact: true }).fill('5');
      const enabled = page.waitForResponse(
        (r) => new URL(r.url()).pathname === control && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
      await operation(await enabled);
    }
    await expect(page.getByTestId('auth-magic-link-panel')).toBeVisible();
    await save('ui_native_magic_link_policy_lifetime_registration_and_openapi');
    async function send(recipient: string) {
      await page.getByLabel('免密码登录邮箱', { exact: true }).fill(recipient);
      expect((await action('发送免密码登录链接', 'sign-in/magic-link')).status()).toBe(200);
      return code(recipient, 'Sign in to your application');
    }
    async function redeem(url: string) {
      const panel = page.getByTestId('auth-magic-link-panel');
      await panel.locator('summary').click();
      await page.getByLabel('应用登录邮件链接', { exact: true }).fill(url);
      await page.getByRole('button', { name: '打开应用登录链接', exact: true }).click();
      await expect(page.getByTestId('auth-magic-link-result')).toContainText(
        '邮箱链接已建立应用会话',
        { timeout: 30000 },
      );
      const r = await context.request.get(app + '/get-session');
      expect(r.status()).toBe(200);
      return (await r.json()).user;
    }
    const first = await send(email);
    const user = await redeem(first);
    expect(user.email).toBe(email);
    expect(user.emailVerified).toBe(true);
    accounts.push(user.id);
    await expect(page.getByTestId('auth-app-session')).toContainText('邮箱已验证');
    await expect(page.getByLabel('应用登录邮件链接', { exact: true })).toHaveValue('');
    await save('actual_smtp_magic_link_creates_verified_identity_and_browser_cookie');
    const replay = await context.request.get(first, { maxRedirects: 0 });
    expect(replay.status()).toBe(302);
    expect(new URL(replay.headers()['location']).searchParams.has('error')).toBe(true);
    await save('original_one_time_magic_link_replay_redirects_with_error');
    const stolen = new URL(first);
    stolen.searchParams.set('callbackURL', 'https://attacker.example/');
    expect((await context.request.get(stolen.href, { maxRedirects: 0 })).status()).toBe(422);
    const untrusted = await context.request.post(app + '/sign-in/magic-link', {
      headers: { Origin: new URL(baseURL!).origin },
      data: { email, callbackURL: 'https://attacker.example/' },
    });
    expect(untrusted.status()).toBe(403);
    await save('untrusted_send_and_redeem_callbacks_denied');
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    const existing = await redeem(await send(email));
    expect(existing.id).toBe(user.id);
    await save('second_actual_mail_signs_in_original_identity_without_duplicate_user');
    await page.screenshot({ path: info.outputPath('auth-magic-link.png'), fullPage: true });
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    async function disable() {
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === control && r.request().method() === 'DELETE',
      );
      await page.getByRole('button', { name: '禁用 Auth', exact: true }).click();
      await operation(await pending);
      await expect(page.getByLabel('Magic Link 允许新用户注册', { exact: true })).toBeEnabled();
    }
    await disable();
    await page.getByLabel('Magic Link 允许新用户注册', { exact: true }).uncheck();
    const restricted = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    await operation(await restricted);
    await expect(page.getByTestId('auth-magic-link-panel')).toBeVisible();
    const denied = await send(pwEmail);
    await page.getByTestId('auth-magic-link-panel').locator('summary').click();
    await page.getByLabel('应用登录邮件链接', { exact: true }).fill(denied);
    await page.getByRole('button', { name: '打开应用登录链接', exact: true }).click();
    await expect(page.getByTestId('auth-magic-link-result')).toContainText('邮箱链接未建立会话', {
      timeout: 30000,
    });
    expect(await (await context.request.get(app + '/get-session')).json()).toBeNull();
    await save('ui_registration_disabled_original_mail_cannot_create_account');
    const after = await redeem(await send(email));
    expect(after.id).toBe(user.id);
    await save('registration_disabled_existing_identity_can_still_sign_in');
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    await disable();
    expect(
      (
        await context.request.post(app + '/sign-in/magic-link', {
          headers: { Origin: new URL(baseURL!).origin },
          data: { email },
        })
      ).status(),
    ).toBe(404);
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_magic_runtime_retired_compute_zero_accounts_mail_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  } finally {
    await inbox.dispose();
  }
});
