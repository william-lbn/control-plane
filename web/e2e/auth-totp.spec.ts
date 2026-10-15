import { test, expect, type Response } from '@playwright/test';
import { randomBytes, createHmac } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
function code(uri: string) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567',
    secret = new URL(uri).searchParams.get('secret')!;
  let bits = 0,
    value = 0;
  const bytes: number[] = [];
  for (const ch of secret.toUpperCase().replace(/=/g, '')) {
    const n = alphabet.indexOf(ch);
    if (n < 0) throw new Error('Invalid private enrollment URI');
    value = (value << 5) | n;
    bits += 5;
    if (bits >= 8) {
      bytes.push((value >> (bits - 8)) & 255);
      bits -= 8;
    }
  }
  const message = Buffer.alloc(8);
  message.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const digest = createHmac('sha1', Buffer.from(bytes)).update(message).digest(),
    offset = digest[19] & 15;
  return String((digest.readUInt32BE(offset) & 0x7fffffff) % 1000000).padStart(6, '0');
}
test('UI TOTP enrollment challenge recovery replay and service restart preserve real identity', async ({
  page,
  context,
  baseURL,
}, info) => {
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!ownerFile || !adminFile || !privateDir || !baseURL)
    throw new Error('Protected Linux inputs required');
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')),
    project = owner.project_id,
    branch = owner.branch_id,
    app = `/auth/v1/${branch}`,
    control = `/api/v1/projects/${project}/branches/${branch}/auth`;
  if (owner.database !== 'owner_transfer_v1') throw new Error('Retained owner fixture required');
  const resumeFile = process.env.NEON_E2E_AUTH_TOTP_RESUME_FILE;
  const resume = resumeFile ? JSON.parse(await readFile(resumeFile, 'utf8')) : null;
  const email = resume?.email ?? 'totp-native-' + Date.now() + '@example.test',
    password = resume?.password ?? randomBytes(24).toString('base64url'),
    checks: string[] = resume?.checks ?? [],
    operations: string[] = resume?.operations ?? [],
    accounts: string[] = resume?.account_ids ?? [];
  let user: { id: string } = { id: resume?.account_ids?.[0] ?? '' },
    uri: string = resume?.totpURI ?? '',
    backups: string[] = resume?.backupCodes ?? [];
  let result = 'running';
  page.setDefaultTimeout(30000);
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        account_ids: accounts,
        project_id: project,
        branch_id: branch,
        actual_totp: true,
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
            throw new Error(`Original MFA operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function action(button: string, route: string) {
    const waiting = page.waitForResponse((r) => new URL(r.url()).pathname === app + '/' + route);
    await page.getByRole('button', { name: button, exact: true }).click();
    return waiting;
  }
  async function login() {
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(password);
    const r = await action('登录应用', 'sign-in/email');
    expect(r.status()).toBe(200);
    expect((await r.json()).twoFactorRedirect).toBe(true);
    await expect(page.getByTestId('auth-app-result')).toContainText('请在下方完成 TOTP');
    expect(await (await context.request.get(app + '/get-session')).json()).toBeNull();
    expect((await context.request.get(app + '/token')).status()).toBeGreaterThanOrEqual(400);
  }
  async function stop() {
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用 Auth', exact: true }).click();
    await operation(await waiting);
    await expect(page.getByLabel('启用应用 TOTP 双重认证', { exact: true })).toBeEnabled();
  }
  async function enable() {
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    await operation(await waiting);
    await expect(page.getByTestId('auth-totp-panel')).toBeVisible();
  }
  try {
    await save();
    await writeFile(
      path.join(privateDir, 'totp-account.private.json'),
      JSON.stringify({ email, password }),
      { flag: 'wx', mode: 0o600 },
    );
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (resume) {
      expect(resume.original_job).toBe('publication-ui-20261015114439');
      expect(checks.length).toBe(4);
      expect(operations.length).toBe(3);
      expect(accounts).toHaveLength(1);
      const state = await (await context.request.get(control)).json();
      expect(state.state).toBe('active');
      expect(state.spec.two_factor_enabled).toBe(true);
      await page.goto(`/#/projects/${project}/auth`);
      await expect(page.getByTestId('auth-totp-panel')).toHaveCount(1);
    } else {
      const state = await context.request.get(control);
      expect(state.status()).toBe(200);
      const original = await state.json();
      if (original.state === 'active') throw new Error('Unknown existing Auth mutation; retain it');
      await writeFile(
        path.join(privateDir, 'totp-original-state.private.json'),
        JSON.stringify(original),
        { flag: 'wx', mode: 0o600 },
      );
      await page.goto(`/#/projects/${project}/auth`);
      await page.getByLabel('Auth 数据库', { exact: true }).fill(owner.database);
      await page.getByLabel('要求应用邮箱验证', { exact: true }).uncheck();
      await page.getByLabel('启用应用 TOTP 双重认证', { exact: true }).check();
      await enable();
      const protocol = await (
        await context.request.get('/api/application-auth-openapi.json?branch_id=' + branch)
      ).json();
      expect(protocol.paths['/two-factor/verify-totp'].post).toBeDefined();
      await save('ui_native_opt_in_and_branch_protocol');
      await page.getByLabel('应用姓名', { exact: true }).fill('TOTP local acceptance');
      await page.getByLabel('应用邮箱', { exact: true }).fill(email);
      await page.getByLabel('应用密码', { exact: true }).fill(password);
      const signup = await action('注册应用用户', 'sign-up/email');
      expect(signup.status()).toBe(200);
      user = (await signup.json()).user;
      accounts.push(user.id);
      await page.getByLabel('MFA 确认密码', { exact: true }).fill(password);
      const enrollment = await action('生成 TOTP 绑定资料', 'two-factor/enable');
      expect(enrollment.status()).toBe(200);
      const secrets = await enrollment.json();
      uri = secrets.totpURI;
      backups = secrets.backupCodes;
      expect(backups.length).toBeGreaterThan(1);
      await writeFile(
        path.join(privateDir, 'totp-enrollment.private.json'),
        JSON.stringify(secrets),
        { flag: 'wx', mode: 0o600 },
      );
      expect(await page.getByLabel('TOTP 绑定地址', { exact: true }).getAttribute('type')).toBe(
        'password',
      );
      expect(
        (await (await context.request.get(app + '/get-session')).json()).user.twoFactorEnabled,
      ).toBe(false);
      await page.getByLabel('TOTP 验证码', { exact: true }).fill(code(uri));
      expect((await action('确认 TOTP 绑定或登录', 'two-factor/verify-totp')).status()).toBe(200);
      await expect(page.getByTestId('auth-totp-panel')).toContainText('验证完成');
      await save('real_ui_pending_enrollment_confirmed_by_rfc6238_totp_and_masked_material');
      expect((await action('退出应用', 'sign-out')).status()).toBe(200);
      await login();
      const cookies = await context.cookies();
      expect(
        cookies.some(
          (c) => c.name === `neon_app_${branch}.two_factor` && c.httpOnly && c.path === app,
        ),
      ).toBe(true);
      expect(cookies.some((c) => c.name === 'neon_v2_session')).toBe(true);
      await page.getByLabel('TOTP 验证码', { exact: true }).fill(code(uri));
      expect((await action('确认 TOTP 绑定或登录', 'two-factor/verify-totp')).status()).toBe(200);
      expect((await (await context.request.get(app + '/get-session')).json()).user.id).toBe(
        user.id,
      );
      await save('password_only_has_no_session_or_jwt_challenge_cookie_relay_and_totp_login');
      expect((await action('退出应用', 'sign-out')).status()).toBe(200);
      await login();
      await page.getByLabel('MFA 恢复码', { exact: true }).fill(backups[0]);
      expect((await action('使用恢复码完成登录', 'two-factor/verify-backup-code')).status()).toBe(
        200,
      );
      expect((await (await context.request.get(app + '/get-session')).json()).user.id).toBe(
        user.id,
      );
      await save('one_time_recovery_code_establishes_original_identity');
      expect((await action('退出应用', 'sign-out')).status()).toBe(200);
      await stop();
      await enable();
    }
    await login();
    await page.getByLabel('MFA 恢复码', { exact: true }).fill(backups[0]);
    expect(
      (await action('使用恢复码完成登录', 'two-factor/verify-backup-code')).status(),
    ).toBeGreaterThanOrEqual(400);
    await page.getByLabel('TOTP 验证码', { exact: true }).fill(code(uri));
    expect((await action('确认 TOTP 绑定或登录', 'two-factor/verify-totp')).status()).toBe(200);
    expect((await (await context.request.get(app + '/get-session')).json()).user.id).toBe(user.id);
    await save(
      'service_disable_reenable_preserves_encryption_identity_and_used_recovery_code_rejected',
    );
    await page.getByLabel('MFA 确认密码', { exact: true }).fill(password);
    const reread = await action('重新读取 TOTP 绑定地址', 'two-factor/get-totp-uri');
    expect(reread.status()).toBe(200);
    expect(new URL((await reread.json()).totpURI).searchParams.get('secret')).toBe(
      new URL(uri).searchParams.get('secret'),
    );
    await page.getByLabel('MFA 确认密码', { exact: true }).fill(password);
    const regenerated = await action('重新生成 MFA 恢复码', 'two-factor/generate-backup-codes');
    expect(regenerated.status()).toBe(200);
    const newer = (await regenerated.json()).backupCodes as string[];
    expect(newer.some((c) => backups.includes(c))).toBe(false);
    await save('ui_retrieves_existing_binding_and_rotates_recovery_codes');
    await page.getByLabel('MFA 确认密码', { exact: true }).fill(password);
    expect((await action('关闭应用用户双重认证', 'two-factor/disable')).status()).toBe(200);
    expect(
      (await (await context.request.get(app + '/get-session')).json()).user.twoFactorEnabled,
    ).toBe(false);
    await page.screenshot({ path: info.outputPath('auth-totp.png'), fullPage: true });
    await save('ui_user_disables_mfa_and_clears_visible_binding_material');
    expect((await action('退出应用', 'sign-out')).status()).toBe(200);
    await stop();
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_auth_runtime_stopped_compute_zero_identity_and_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
