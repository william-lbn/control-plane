import { test, expect, request, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('actual UI email OTP creates identities, verifies email and revokes reset sessions', async ({
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
  const email = 'otp-native-' + Date.now() + '@example.test',
    pwEmail = 'otp-password-' + Date.now() + '@example.test',
    oldPassword = randomBytes(24).toString('base64url'),
    newPassword = randomBytes(24).toString('base64url');
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
      path.join(privateDir!, `otp-mail-${++mailCount}.private.json`),
      JSON.stringify(body),
      { flag: 'wx', mode: 0o600 },
    );
    messages.push(id);
    const value = (body.Text as string).match(/\b[0-9]{6}\b/)?.[0];
    if (!value) throw new Error('Actual private OTP absent');
    return value;
  }
  try {
    await save();
    await writeFile(
      path.join(privateDir, 'otp-accounts.private.json'),
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
    const schema = await protocol.json();
    expect(schema.openapi).toBe('3.1.0');
    expect(schema.servers[0].variables.branch_id.default).toBe(branch);
    expect(schema.components.securitySchemes.applicationSession.name).toBe(
      'neon_app_' + branch + '.session_token',
    );
    expect(schema.paths['/sign-in/email-otp'].post).toBeDefined();
    expect(schema.paths['/email-otp/get-verification-otp']).toBeUndefined();
    await save(
      'deployed_application_openapi_has_exact_branch_cookie_and_excludes_private_code_retrieval',
    );
    // The preceding accepted owner fixture already holds the CREATE grant. Do
    // not repeat its SQL test or allocate another project/endpoint.
    await page.goto(`/#/projects/${project}/auth`);
    await page.getByLabel('Auth 数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('启用应用邮箱验证码', { exact: true }).check();
    const enabled = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    await operation(await enabled);
    await expect(page.getByTestId('auth-otp-panel')).toBeVisible();
    await save('ui_native_branch_otp_policy_enabled_without_new_project_or_compute');
    await page.getByLabel('验证码邮箱', { exact: true }).fill(email);
    await page.getByLabel('验证码用途', { exact: true }).selectOption('sign-in');
    expect((await action('发送应用验证码', 'email-otp/send-verification-otp')).status()).toBe(200);
    const loginCode = await code(email, 'Your application sign-in code');
    const wrong = loginCode === '000000' ? '000001' : '000000';
    await page.getByLabel('应用邮箱验证码', { exact: true }).fill(wrong);
    expect((await action('提交应用验证码', 'sign-in/email-otp')).status()).toBeGreaterThanOrEqual(
      400,
    );
    await page.getByLabel('应用邮箱验证码', { exact: true }).fill(loginCode);
    const nativeLogin = await action('提交应用验证码', 'sign-in/email-otp');
    expect(nativeLogin.status()).toBe(200);
    const nativeUser = (await nativeLogin.json()).user;
    expect(nativeUser.email).toBe(email);
    expect(nativeUser.emailVerified).toBe(true);
    accounts.push(nativeUser.id);
    await expect(page.getByLabel('应用邮箱验证码', { exact: true })).toHaveValue('');
    await expect(page.getByTestId('auth-app-session')).toContainText('邮箱已验证');
    await save('actual_smtp_otp_wrong_code_denied_and_native_verified_signup_login');
    const replay = await context.request.post(app + '/sign-in/email-otp', {
      headers: { Origin: new URL(baseURL).origin },
      data: { email, otp: loginCode },
    });
    expect(replay.status()).toBeGreaterThanOrEqual(400);
    await save('actual_otp_signin_code_replay_denied');
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    await page.getByLabel('应用姓名', { exact: true }).fill('OTP recovery tester');
    await page.getByLabel('应用邮箱', { exact: true }).fill(pwEmail);
    await page.getByLabel('应用密码', { exact: true }).fill(oldPassword);
    const signup = await action('注册应用用户', 'sign-up/email');
    expect(signup.status()).toBe(200);
    const pwUser = (await signup.json()).user;
    accounts.push(pwUser.id);
    await page.getByLabel('验证码邮箱', { exact: true }).fill(pwEmail);
    await page.getByLabel('验证码用途', { exact: true }).selectOption('email-verification');
    expect((await action('发送应用验证码', 'email-otp/send-verification-otp')).status()).toBe(200);
    const verifyCode = await code(pwEmail, 'Your application email verification code');
    await page.getByLabel('应用邮箱验证码', { exact: true }).fill(verifyCode);
    expect((await action('提交应用验证码', 'email-otp/verify-email')).status()).toBe(200);
    await page.getByLabel('应用密码', { exact: true }).fill(oldPassword);
    const confirmed = await action('登录应用', 'sign-in/email');
    expect(confirmed.status()).toBe(200);
    const actualUser = (await confirmed.json()).user;
    expect(actualUser.id).toBe(pwUser.id);
    expect(actualUser.emailVerified).toBe(true);
    await save('actual_ui_otp_email_verification_confirms_original_password_account');
    const cookies = await context.cookies(new URL(app, baseURL).href),
      prefix = 'neon_app_' + branch + '.session_token';
    const captured = cookies
      .filter((c) => c.name === prefix || c.name === '__Secure-' + prefix)
      .map((c) => c.name + '=' + c.value)
      .join('; ');
    if (!captured) throw new Error('Original application cookie absent');
    const originalSession = await request.newContext({
      baseURL,
      extraHTTPHeaders: { Cookie: captured, Origin: new URL(baseURL).origin },
      maxRedirects: 0,
    });
    const before = await originalSession.get(app + '/get-session');
    expect(before.status()).toBe(200);
    expect((await before.json()).user.id).toBe(pwUser.id);
    await page.getByLabel('验证码用途', { exact: true }).selectOption('forget-password');
    expect((await action('发送应用验证码', 'email-otp/request-password-reset')).status()).toBe(200);
    const resetCode = await code(pwEmail, 'Your application password recovery code');
    await page.getByLabel('应用邮箱验证码', { exact: true }).fill(resetCode);
    await page.getByLabel('验证码找回后的新密码', { exact: true }).fill(newPassword);
    expect((await action('提交应用验证码', 'email-otp/reset-password')).status()).toBe(200);
    await expect(page.getByLabel('应用邮箱验证码', { exact: true })).toHaveValue('');
    await expect(page.getByLabel('验证码找回后的新密码', { exact: true })).toHaveValue('');
    const revoked = await originalSession.get(app + '/get-session');
    expect(revoked.status()).toBe(200);
    expect(await revoked.json()).toBeNull();
    expect((await originalSession.get(app + '/token')).status()).toBe(401);
    await originalSession.dispose();
    await save('actual_otp_recovery_revokes_captured_pre_reset_server_session_and_token_route');
    expect(
      (
        await context.request.post(app + '/email-otp/reset-password', {
          headers: { Origin: new URL(baseURL).origin },
          data: { email: pwEmail, otp: resetCode, password: oldPassword },
        })
      ).status(),
    ).toBeGreaterThanOrEqual(400);
    await new Promise((resolve) => setTimeout(resolve, 11000));
    await page.getByLabel('应用密码', { exact: true }).fill(oldPassword);
    expect((await action('登录应用', 'sign-in/email')).status()).toBe(401);
    await page.getByLabel('应用密码', { exact: true }).fill(newPassword);
    expect((await action('登录应用', 'sign-in/email')).status()).toBe(200);
    await save('otp_reset_one_time_old_password_denied_new_password_valid');
    await page.getByLabel('应用密码', { exact: true }).fill('');
    await page.screenshot({ path: info.outputPath('auth-otp.png'), fullPage: true });
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    const disabled = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用 Auth', exact: true }).click();
    await operation(await disabled);
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_otp_runtime_retired_compute_zero_accounts_mail_and_evidence_retained');
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
