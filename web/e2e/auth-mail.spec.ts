import { expect, test, request, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Auth mailbox delivery verifies the actual UI account and consumes reset links once', async ({
  page,
  context,
  baseURL,
}, info) => {
  page.setDefaultTimeout(30000);
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    mailFile = process.env.NEON_E2E_MAILBOX_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!ownerFile || !admin || !mailFile || !privateDir || !baseURL)
    throw new Error('Protected Linux owner and mailbox inputs required');
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    role: string;
    password_file: string;
  };
  if (
    owner.database !== 'owner_transfer_v1' ||
    owner.role !== 'owner_new_v1' ||
    !/^prj_[a-f0-9]{16}$/.test(owner.project_id)
  )
    throw new Error('Retained owner scope required');
  const mailbox = JSON.parse(await readFile(mailFile, 'utf8')) as {
    url: string;
    username: string;
    password_file: string;
  };
  if (
    !['http://neon-auth-mailbox:8025', 'http://127.0.0.1:18025'].includes(mailbox.url) ||
    mailbox.username !== 'local-mail-reader'
  )
    throw new Error('Exact isolated receiver or protected local port-forward required');
  const credential = (await readFile(mailbox.password_file, 'utf8')).trim();
  const inbox = await request.newContext({
    baseURL: mailbox.url,
    extraHTTPHeaders: {
      Authorization: 'Basic ' + Buffer.from(mailbox.username + ':' + credential).toString('base64'),
    },
    maxRedirects: 0,
  });
  const { project_id: project, branch_id: branch, writer_id: writer } = owner,
    control = `/api/v1/projects/${project}/branches/${branch}/auth`,
    app = `/auth/v1/${branch}`;
  const email = 'mail-' + Date.now() + '@example.test',
    password = randomBytes(24).toString('base64url'),
    nextPassword = randomBytes(24).toString('base64url');
  const checks: string[] = [],
    operations: string[] = [],
    messages: string[] = [];
  let result = 'running',
    count = 0;
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        message_ids: messages,
        project_id: project,
        branch_id: branch,
        mail_transport: 'actual SMTP to isolated Mailpit',
        private_mail_bodies_retained: true,
        credentials_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function operation(response: Response) {
    expect(response.status()).toBe(202);
    const body = await response.json();
    operations.push(body.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${body.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const o = await r.json();
          if (['failed', 'cancelled'].includes(o.state))
            throw new Error(`Original Auth operation ${o.id} ${o.error_code}`);
          return o.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function mail(subject: string) {
    let id = '';
    await expect
      .poll(
        async () => {
          const r = await inbox.get('/api/v1/messages?limit=100');
          expect(r.status()).toBe(200);
          const list = await r.json();
          const found = list.messages.find(
            (m: { ID: string; Subject: string; To: { Address: string }[] }) =>
              m.Subject === subject &&
              m.To.some((t) => t.Address === email) &&
              !messages.includes(m.ID),
          );
          id = found?.ID ?? '';
          return !!found;
        },
        { timeout: 45000 },
      )
      .toBe(true);
    const response = await inbox.get('/api/v1/message/' + id);
    expect(response.status()).toBe(200);
    const body = await response.json();
    // Raw MIME/URLs contain bearer links. Keep them in the protected Linux
    // fixture directory, never the public report, screenshot or trace.
    await writeFile(path.join(privateDir!, `mail-${++count}.private.json`), JSON.stringify(body), {
      flag: 'wx',
      mode: 0o600,
    });
    messages.push(id);
    const text = body.Text as string,
      link = text.match(/http:\/\/192\.168\.146\.100:30788\/auth\/v1\/[^\s]+/)?.[0];
    if (!link) throw new Error('Private Auth mail link absent');
    const url = new URL(link);
    expect(url.pathname.startsWith(app + '/')).toBe(true);
    return url;
  }
  async function action(button: string, route: string) {
    const pending = page.waitForResponse((r) => new URL(r.url()).pathname === app + '/' + route, {
      timeout: 30000,
    });
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function login(value: string) {
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(value);
    return action('登录应用', 'sign-in/email');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(writer);
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('执行模式').selectOption('single');
    await page.getByLabel('查询操作').selectOption('query');
    await page
      .getByLabel('SQL 查询')
      .fill(
        `GRANT CREATE, CONNECT ON DATABASE "${owner.database}" TO control_probe WITH GRANT OPTION`,
      );
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(owner.password_file, 'utf8')).trim());
    const grant = page.waitForResponse(
      (r) => new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query`,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    expect((await grant).status()).toBe(200);
    await page.goto(`/#/projects/${project}/auth`);
    await page.getByLabel('Auth 数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('要求应用邮箱验证', { exact: true }).check();
    const enabling = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    await operation(await enabling);
    await expect(page.getByTestId('auth-recovery-panel')).toBeVisible();
    await save('ui_owner_grant_native_auth_and_required_email_policy_enabled');
    const anonymous = await request.newContext({ baseURL: mailbox.url, maxRedirects: 0 });
    expect((await anonymous.get('/api/v1/messages')).status()).toBe(401);
    await anonymous.dispose();
    await save('isolated_receiver_rejects_unauthenticated_inbox_access');
    await page.getByLabel('应用姓名', { exact: true }).fill('Mail workflow user');
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(password);
    expect((await action('注册应用用户', 'sign-up/email')).status()).toBe(200);
    await expect(page.getByTestId('auth-app-session')).toHaveCount(0);
    expect((await login(password)).status()).toBe(403);
    await save('ui_required_verification_blocks_login_without_faking_a_session');
    const verification = await mail('Verify your application email'),
      verifyToken = verification.searchParams.get('token');
    if (!verifyToken) throw new Error('Actual verification token absent');
    await page.getByText('使用邮件中的验证凭据', { exact: true }).click();
    await page.getByLabel('邮箱验证凭据', { exact: true }).fill(verifyToken);
    await action('验证应用邮箱', 'verify-email');
    await expect(page.getByLabel('邮箱验证凭据', { exact: true })).toHaveValue('');
    expect((await login(password)).status()).toBe(200);
    await expect(page.getByTestId('auth-app-session')).toContainText('邮箱已验证');
    await save('actual_smtp_message_token_verifies_ui_identity_and_unlocks_login');
    await page.getByLabel('应用收信邮箱', { exact: true }).fill(email);
    expect((await action('发送密码找回邮件', 'request-password-reset')).status()).toBe(200);
    const reset = await mail('Reset your application password'),
      resetToken = reset.pathname.split('/').at(-1)!;
    await page.getByLabel('密码找回凭据', { exact: true }).fill(resetToken);
    await page.getByLabel('找回后的新密码', { exact: true }).fill(nextPassword);
    expect((await action('完成密码找回', 'reset-password')).status()).toBe(200);
    await expect(page.getByLabel('密码找回凭据', { exact: true })).toHaveValue('');
    await expect(page.getByLabel('找回后的新密码', { exact: true })).toHaveValue('');
    await new Promise((resolve) => setTimeout(resolve, 11000));
    expect((await login(password)).status()).toBe(401);
    expect((await login(nextPassword)).status()).toBe(200);
    await save('ui_real_mail_password_reset_old_password_denied_new_password_valid');
    const reused = await context.request.post(app + '/reset-password', {
      headers: { Origin: new URL(baseURL).origin },
      data: { token: resetToken, newPassword: password },
    });
    expect(reused.status()).toBeGreaterThanOrEqual(400);
    await save('actual_mail_reset_token_cannot_be_consumed_twice');
    await page.screenshot({ path: info.outputPath('auth-mail.png'), fullPage: true });
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    const disabling = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用 Auth', exact: true }).click();
    await operation(await disabling);
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_auth_retired_compute_zero_accounts_mail_records_and_evidence_retained');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  } finally {
    await inbox.dispose();
  }
});
