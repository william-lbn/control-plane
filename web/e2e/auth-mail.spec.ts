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
  const resumePath = process.env.NEON_E2E_AUTH_MAIL_RESUME;
  const resume = resumePath
    ? (JSON.parse(await readFile(resumePath, 'utf8')) as {
        email: string;
        account_id: string;
        original_failure_job: string;
        checks: string[];
        operations: string[];
      })
    : null;
  if (
    resume &&
    (!/^mail-[0-9]+@example\.test$/.test(resume.email) ||
      !/^[A-Za-z0-9]{20,64}$/.test(resume.account_id) ||
      resume.original_failure_job !== 'publication-ui-20261014125012' ||
      resume.checks.length !== 3 ||
      resume.operations.length !== 1)
  )
    throw new Error('Exact original SMTP failure checkpoint required');
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
  const email = resume?.email ?? 'mail-' + Date.now() + '@example.test',
    password = randomBytes(24).toString('base64url'),
    nextPassword = randomBytes(24).toString('base64url');
  const checks: string[] = [...(resume?.checks ?? [])],
    operations: string[] = [...(resume?.operations ?? [])],
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
        original_failure_job: resume?.original_failure_job ?? null,
        original_account_id: resume?.account_id ?? null,
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
  async function mail(subject: string, recipient = email) {
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
              m.To.some((t) => t.Address === recipient) &&
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
  async function login(value: string, recipient = email) {
    await page.getByLabel('应用邮箱', { exact: true }).fill(recipient);
    await page.getByLabel('应用密码', { exact: true }).fill(value);
    return action('登录应用', 'sign-in/email');
  }
  async function recoverPassword(value: string) {
    await page.getByLabel('应用收信邮箱', { exact: true }).fill(email);
    expect((await action('发送密码找回邮件', 'request-password-reset')).status()).toBe(200);
    const reset = await mail('Reset your application password'),
      resetToken = reset.pathname.split('/').at(-1)!;
    await page.getByLabel('密码找回凭据', { exact: true }).fill(resetToken);
    await page.getByLabel('找回后的新密码', { exact: true }).fill(value);
    expect((await action('完成密码找回', 'reset-password')).status()).toBe(200);
    await expect(page.getByLabel('密码找回凭据', { exact: true })).toHaveValue('');
    await expect(page.getByLabel('找回后的新密码', { exact: true })).toHaveValue('');
    return resetToken;
  }
  try {
    await save();
    await writeFile(
      path.join(privateDir, 'auth-mail-account.private.json'),
      JSON.stringify({
        email,
        password,
        nextPassword,
        account_id: resume?.account_id ?? null,
        original_failure_job: resume?.original_failure_job ?? null,
      }),
      { flag: 'wx', mode: 0o600 },
    );
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!resume) {
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
        (r) =>
          new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query`,
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
    } else {
      const current = await context.request.get(control);
      expect(current.status()).toBe(200);
      const instance = await current.json();
      expect(instance.state).toBe('active');
      expect(instance.spec.require_email_verification).toBe(true);
      expect(instance.mail_runtime_enabled).toBe(true);
      const previous = await context.request.get(
        `/api/v1/projects/${project}/operations/${resume.operations[0]}`,
      );
      expect(previous.status()).toBe(200);
      expect((await previous.json()).state).toBe('succeeded');
      await page.goto(`/#/projects/${project}/auth`);
      await page.getByLabel('应用收信邮箱', { exact: true }).fill(email);
      expect((await action('发送邮箱验证邮件', 'send-verification-email')).status()).toBe(200);
      await save('original_unverified_account_retained_and_mail_redelivery_requested');
    }
    const verification = await mail('Verify your application email'),
      verifyToken = verification.searchParams.get('token');
    if (!verifyToken) throw new Error('Actual verification token absent');
    await page.getByText('使用邮件中的验证凭据', { exact: true }).click();
    await page.getByLabel('邮箱验证凭据', { exact: true }).fill(verifyToken);
    await action('验证应用邮箱', 'verify-email');
    await expect(page.getByLabel('邮箱验证凭据', { exact: true })).toHaveValue('');
    if (resume) {
      await recoverPassword(password);
      await save(
        'original_memory_only_test_password_recovered_through_actual_mail_without_account_recreation',
      );
    }
    const initialLogin = await login(password);
    expect(initialLogin.status()).toBe(200);
    if (resume) expect((await initialLogin.json()).user.id).toBe(resume.account_id);
    await expect(page.getByTestId('auth-app-session')).toContainText('邮箱已验证');
    await save('actual_smtp_message_token_verifies_ui_identity_and_unlocks_login');
    const resetToken = await recoverPassword(nextPassword);
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
    // The failed original signup is preserved. A distinct second account
    // verifies automatic signup delivery after the receiver configuration fix.
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    const autoEmail = 'auto-send-' + Date.now() + '@example.test',
      autoPassword = randomBytes(24).toString('base64url');
    await writeFile(
      path.join(privateDir, 'auth-mail-autosignup.private.json'),
      JSON.stringify({ email: autoEmail, password: autoPassword }),
      { flag: 'wx', mode: 0o600 },
    );
    await page.getByLabel('应用姓名', { exact: true }).fill('Automatic delivery verification');
    await page.getByLabel('应用邮箱', { exact: true }).fill(autoEmail);
    await page.getByLabel('应用密码', { exact: true }).fill(autoPassword);
    expect((await action('注册应用用户', 'sign-up/email')).status()).toBe(200);
    await expect(page.getByTestId('auth-app-session')).toHaveCount(0);
    const autoVerify = await mail('Verify your application email', autoEmail),
      autoToken = autoVerify.searchParams.get('token');
    if (!autoToken) throw new Error('Actual automatic-signup mail absent');
    await page.getByLabel('邮箱验证凭据', { exact: true }).fill(autoToken);
    await action('验证应用邮箱', 'verify-email');
    const autoLogin = await login(autoPassword, autoEmail);
    expect(autoLogin.status()).toBe(200);
    expect((await autoLogin.json()).user.email).toBe(autoEmail);
    await expect(page.getByTestId('auth-app-session')).toContainText('邮箱已验证');
    await page.getByLabel('应用密码', { exact: true }).fill('');
    await save(
      'actual_ui_signup_automatic_delivery_and_verified_login_for_second_controlled_account',
    );
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
