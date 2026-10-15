import { test, expect, type Page } from '@playwright/test';
import { randomBytes, createHmac } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

function totp(secret: string, offset = 0) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567',
    bytes: number[] = [];
  let bits = 0,
    value = 0;
  for (const ch of secret) {
    value = (value << 5) | alphabet.indexOf(ch);
    bits += 5;
    if (bits >= 8) {
      bytes.push((value >> (bits - 8)) & 255);
      bits -= 8;
    }
  }
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000) + offset));
  const hash = createHmac('sha1', Buffer.from(bytes)).update(counter).digest(),
    at = hash[19] & 15;
  return String((hash.readUInt32BE(at) & 0x7fffffff) % 1000000).padStart(6, '0');
}

test('UI Console MFA enrollment, real login, recovery replay and disable isolate operator', async ({
  page,
  browser,
  baseURL,
}, info) => {
  test.setTimeout(180000);
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!adminFile || !privateDir || !baseURL)
    throw new Error('Protected Linux operator input required');
  const resumeFile = process.env.NEON_E2E_CONSOLE_MFA_RESUME_FILE;
  const resume = resumeFile ? JSON.parse(await readFile(resumeFile, 'utf8')) : null;
  const username = resume?.username ?? 'mfa_' + Date.now(),
    password = resume?.password ?? randomBytes(30).toString('base64url'),
    checks: string[] = resume?.checks ?? [],
    usedRecovery: number[] = resume?.used_recovery_indexes ?? [];
  let result = 'running',
    organization = resume?.organization ?? '',
    secret = resume?.secret ?? '',
    codes: string[] = resume?.recovery_codes ?? [];
  let waiting: ReturnType<Page['waitForResponse']>;
  const fixture = path.join(privateDir, 'console-mfa-account.private.json');
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    await writeFile(
      fixture,
      JSON.stringify({
        username,
        password,
        organization,
        secret,
        recovery_codes: codes,
        checks,
        used_recovery_indexes: usedRecovery,
        original_activation_display_verified: resume?.original_activation_display_verified ?? false,
      }),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        organization,
        compute_created: false,
        sql_requests: 0,
        secrets_in_public_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  const a = await browser.newContext({ baseURL }),
    b = await browser.newContext({ baseURL }),
    c = await browser.newContext({ baseURL });
  const member = await a.newPage(),
    other = await b.newPage(),
    fresh = await c.newPage();
  async function login(p: Page, code = '', recovery = false, want = 200) {
    p.setDefaultTimeout(15000);
    await p.goto('/#/projects');
    await p.reload();
    await p.getByLabel('用户名', { exact: true }).fill(username);
    await p.getByLabel('密码', { exact: true }).fill(password);
    if (code) {
      if (!(await p.getByLabel('登录验证码', { exact: true }).isVisible())) {
        await p.getByText('已启用双重验证？', { exact: true }).click();
      }
      await p.getByLabel('登录验证码', { exact: true }).fill(code);
      await p.getByLabel('使用恢复码登录', { exact: true }).setChecked(recovery);
      if (recovery) await p.getByLabel('登录验证码', { exact: true }).fill(code);
    }
    const waiting = p.waitForResponse(
      (r) => new URL(r.url()).pathname === '/auth/login' && r.request().method() === 'POST',
    );
    await p.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(want);
    if (want === 200)
      await expect(p.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!resume) {
      await page.goto('/#/organization');
      await page.getByLabel('组织名称', { exact: true }).fill('MFA UI ' + Date.now());
      waiting = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === '/api/v1/organizations' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建组织', exact: true }).click();
      const org = await waiting;
      expect(org.status()).toBe(201);
      organization = (await org.json()).id;
      await page.getByLabel('受邀账号', { exact: true }).fill(username);
      await page.getByLabel('邀请角色', { exact: true }).selectOption('viewer');
      waiting = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/api/v1/organizations/${organization}/invitations` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建邀请', exact: true }).click();
      const invite = await waiting;
      expect(invite.status()).toBe(201);
      const invitation = await invite.json();
      await member.goto('/#/signup');
      await member.getByLabel('受邀用户名', { exact: true }).fill(username);
      await member.getByLabel('邀请凭据', { exact: true }).fill(invitation.token);
      await member.getByLabel('设置密码', { exact: true }).fill(password);
      await member.getByLabel('确认密码', { exact: true }).fill(password);
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/auth/signup' && r.request().method() === 'POST',
      );
      await member.getByRole('button', { name: '注册并加入组织', exact: true }).click();
      expect((await waiting).status()).toBe(201);
      await member.getByRole('link', { name: '返回登录', exact: true }).click();
      await login(member);
      await login(other);
      await save('isolated_viewer_account_and_second_session_created_from_UI');
      await member.goto('/#/organization');
      const panel = member.getByRole('region', { name: '控制台双重验证' });
      await panel.getByRole('button', { name: '加载双重验证状态', exact: true }).click();
      await expect(panel.getByTestId('console-mfa-state')).toContainText('未启用');
      await panel.getByLabel('双重验证操作密码', { exact: true }).fill('incorrect-password');
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/enroll',
      );
      await panel.getByRole('button', { name: '设置验证器', exact: true }).click();
      expect((await waiting).status()).toBe(401);
      await expect(panel.getByRole('alert')).toBeVisible();
      await save('wrong_password_does_not_create_factor');
      await panel.getByLabel('双重验证操作密码', { exact: true }).fill(password);
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/enroll',
      );
      await panel.getByRole('button', { name: '设置验证器', exact: true }).click();
      const enrolled = await waiting;
      expect(enrolled.status()).toBe(200);
      secret = (await enrolled.json()).secret;
      await expect(panel.getByLabel('验证器密钥', { exact: true })).toHaveValue(secret);
      await expect(panel.getByRole('status')).toContainText('请在验证器手动添加此密钥');
      await save('UI_enrollment_returns_own_manual_authenticator_key');
      await panel.getByLabel('双重验证操作密码', { exact: true }).fill(password);
      await panel.getByLabel('双重验证操作验证码', { exact: true }).fill('invalid');
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/confirm',
      );
      await panel.getByRole('button', { name: '确认启用双重验证', exact: true }).click();
      expect((await waiting).status()).toBe(401);
      await expect(panel.getByRole('alert')).toBeVisible();
      await save('wrong_authenticator_cannot_activate_factor');
      await panel.getByLabel('双重验证操作密码', { exact: true }).fill(password);
      await panel.getByLabel('双重验证操作验证码', { exact: true }).fill(totp(secret, -1));
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/confirm',
      );
      await panel.getByRole('button', { name: '确认启用双重验证', exact: true }).click();
      const confirmed = await waiting;
      expect(confirmed.status()).toBe(200);
      codes = (await confirmed.json()).recovery_codes;
      expect(codes).toHaveLength(10);
      await expect(panel.getByTestId('console-mfa-state')).toContainText('已启用');
      const displayed = await panel
        .getByRole('textbox', { name: '一次性恢复码', exact: true })
        .inputValue();
      expect(displayed === codes.join('\n')).toBe(true);
      await panel.getByRole('button', { name: '已保存，隐藏恢复码', exact: true }).click();
      expect((await b.request.get('/api/v1/session')).status()).toBe(401);
      expect((await a.request.get('/api/v1/session')).status()).toBe(200);
      expect((await page.request.get('/api/v1/session')).status()).toBe(200);
      await save('actual_activation_revokes_other_own_session_and_preserves_operator');
    } else {
      expect(resume.original_activation_display_verified).toBe(true);
      const index = [9, 8, 7, 6, 5, 4, 3, 2].find((i) => !usedRecovery.includes(i));
      if (index === undefined) throw new Error('No unused original recovery code available');
      await login(member, codes[index], true);
      usedRecovery.push(index);
      await member.goto('/#/organization');
      expect((await page.request.get('/api/v1/session')).status()).toBe(200);
      await save('original_activation_display_verified_from_retained_UI_and_same_account_resumed');
    }
    if (!checks.includes('password_alone_cannot_create_session')) {
      await login(fresh, '', false, 401);
      await save('password_alone_cannot_create_session');
    }
    if (!checks.includes('wrong_login_proof_is_denied')) {
      await login(fresh, 'invalid', false, 401);
      await save('wrong_login_proof_is_denied');
    }
    const current = totp(secret);
    await login(fresh, current);
    if (!checks.includes('authenticator_login_succeeds_and_same_counter_replay_is_denied')) {
      await login(other, current, false, 401);
      await save('authenticator_login_succeeds_and_same_counter_replay_is_denied');
    }
    const recoveryIndex = checks.includes('recovery_login_consumes_code_and_replay_is_denied')
      ? [9, 8, 7, 6, 5, 4, 3, 2].find((i) => !usedRecovery.includes(i))
      : 0;
    if (recoveryIndex === undefined) throw new Error('No original recovery proof remains');
    await login(other, codes[recoveryIndex], true);
    usedRecovery.push(recoveryIndex);
    const d = await browser.newContext({ baseURL });
    try {
      await login(await d.newPage(), codes[recoveryIndex], true, 401);
    } finally {
      await d.close();
    }
    await save('recovery_login_consumes_code_and_replay_is_denied');
    await member.reload();
    const currentPanel = member.getByRole('region', { name: '控制台双重验证' });
    await currentPanel.getByRole('button', { name: '加载双重验证状态', exact: true }).click();
    await expect(currentPanel.getByTestId('console-mfa-state')).toContainText(
      '剩余恢复码 ' + (10 - usedRecovery.length),
    );
    await expect(currentPanel.getByTestId('console-mfa-recovery')).toHaveCount(0);
    await save('reload_never_reveals_factor_or_recovery_codes');
    await currentPanel.getByLabel('双重验证操作密码', { exact: true }).fill(password);
    await currentPanel.getByLabel('使用一次性恢复码', { exact: true }).check();
    await currentPanel.getByLabel('双重验证操作验证码', { exact: true }).fill(codes[1]);
    waiting = member.waitForResponse(
      (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/disable',
    );
    await currentPanel.getByRole('button', { name: '确认关闭双重验证', exact: true }).click();
    expect((await waiting).status()).toBe(200);
    await expect(currentPanel.getByTestId('console-mfa-state')).toContainText('未启用');
    expect((await c.request.get('/api/v1/session')).status()).toBe(401);
    expect((await b.request.get('/api/v1/session')).status()).toBe(401);
    await save('password_plus_unused_recovery_disables_factor_and_revokes_other_logins');
    await login(fresh);
    await save('password_login_works_after_explicit_disable');
    result = 'pass';
    await save();
  } finally {
    if (result !== 'pass') result = 'fail';
    await save();
    await a.close();
    await b.close();
    await c.close();
  }
});
