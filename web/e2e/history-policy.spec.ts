import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { createHmac } from 'node:crypto';
import path from 'node:path';

function authenticatorCode(secret: string) {
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
  const message = Buffer.alloc(8);
  message.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const hash = createHmac('sha1', Buffer.from(bytes)).update(message).digest(),
    offset = hash[19] & 15;
  return String((hash.readUInt32BE(offset) & 0x7fffffff) % 1000000).padStart(6, '0');
}

test('UI personal SQL history policy persists opt-out and explicit retention without old data deletion', async ({
  page,
  browser,
  baseURL,
}, info) => {
  test.setTimeout(180000);
  const fixture = process.env.NEON_E2E_CONSOLE_ACCOUNT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixture || !adminFile || !baseURL)
    throw new Error('Protected completed MFA account required');
  const account = JSON.parse(await readFile(fixture, 'utf8')),
    checks: string[] = [];
  if (!account.checks?.includes('password_login_works_after_explicit_disable'))
    throw new Error('Original account has not finished MFA acceptance');
  let result = 'running';
  const context = await browser.newContext({ baseURL }),
    member = await context.newPage();
  member.setDefaultTimeout(15000);
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        organization: account.organization,
        compute_created: false,
        sql_requests: 0,
        existing_history_deleted: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const adminBefore = await (
      await page.request.get('/api/v1/account/query-history-policy')
    ).json();
    await member.goto('/#/projects');
    await member.getByLabel('用户名', { exact: true }).fill(account.username);
    await member.getByLabel('密码', { exact: true }).fill(account.password);
    await member.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(member.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await member.goto('/#/organization');
    const panel = member.getByRole('region', { name: '我的 SQL 历史策略' });
    await panel.getByRole('button', { name: '加载我的历史策略', exact: true }).click();
    await expect(panel.getByTestId('history-policy-current')).toContainText(
      '采集开启 · 保留全部 · 版本 0',
    );
    await save('fresh_own_account_defaults_to_capture_and_hold_all_without_mutation');
    await panel.getByLabel('采集我后续的 SQL 执行历史', { exact: true }).uncheck();
    await panel.getByRole('combobox', { name: '我的历史保留期限', exact: true }).selectOption('7');
    await expect(
      panel.getByRole('button', { name: '保存我的历史策略', exact: true }),
    ).toBeDisabled();
    await save('UI_requires_explicit_confirmation_for_expiring_existing_completed_history');
    await panel.getByLabel('我确认允许物理清理本人已完成的到期历史', { exact: true }).check();
    let waiting = member.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === '/api/v1/account/query-history-policy' &&
        r.request().method() === 'PATCH',
    );
    await panel.getByRole('button', { name: '保存我的历史策略', exact: true }).click();
    const changed = await waiting;
    expect(changed.status()).toBe(200);
    const request = changed.request(),
      original = request.postDataJSON(),
      originalKey = request.headers()['idempotency-key'];
    await expect(panel.getByRole('status')).toContainText('已保存本账号的历史策略');
    await expect(panel.getByTestId('history-policy-current')).toContainText(
      '采集关闭 · 保留 7 天 · 版本 1',
    );
    await save('UI_saves_actual_owner_opt_out_and_retention_with_version_and_idempotency');
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('Authenticated browser CSRF required');
    const headers = { 'X-CSRF-Token': csrf, 'Idempotency-Key': originalKey, 'If-Match': '"0"' };
    const replay = await context.request.patch('/api/v1/account/query-history-policy', {
      headers,
      data: original,
    });
    expect(replay.status()).toBe(200);
    expect((await replay.json()).version_token).toBe('1');
    const conflict = await context.request.patch('/api/v1/account/query-history-policy', {
      headers,
      data: { ...original, capture_enabled: true },
    });
    expect(conflict.status()).toBe(409);
    const stale = await context.request.patch('/api/v1/account/query-history-policy', {
      headers: { ...headers, 'Idempotency-Key': 'history-ui-stale-' + Date.now() },
      data: original,
    });
    expect(stale.status()).toBe(412);
    await save('real_metadata_replay_conflict_and_stale_version_are_enforced');
    await member.reload();
    const current = member.getByRole('region', { name: '我的 SQL 历史策略' });
    await current.getByRole('button', { name: '加载我的历史策略', exact: true }).click();
    await expect(current.getByTestId('history-policy-current')).toContainText(
      '采集关闭 · 保留 7 天 · 版本 1',
    );
    await expect(
      current.getByLabel('采集我后续的 SQL 执行历史', { exact: true }),
    ).not.toBeChecked();
    await save('browser_reload_reads_durable_personal_policy');
    await current.getByLabel('我确认允许物理清理本人已完成的到期历史', { exact: true }).check();
    waiting = member.waitForResponse(
      (r) => new URL(r.url()).pathname === '/api/v1/account/query-history-cleanup',
    );
    await current.getByRole('button', { name: '执行我的到期历史清理', exact: true }).click();
    const clean = await waiting;
    expect(clean.status()).toBe(200);
    expect((await clean.json()).deleted_count).toBe(0);
    await expect(current.getByRole('status')).toContainText('已清理本账号 0 条到期历史');
    await save('explicit_UI_cleanup_of_new_empty_account_deletes_no_existing_records');
    await current.getByLabel('采集我后续的 SQL 执行历史', { exact: true }).check();
    await current
      .getByRole('combobox', { name: '我的历史保留期限', exact: true })
      .selectOption('0');
    waiting = member.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === '/api/v1/account/query-history-policy' &&
        r.request().method() === 'PATCH',
    );
    await current.getByRole('button', { name: '保存我的历史策略', exact: true }).click();
    expect((await waiting).status()).toBe(200);
    await expect(current.getByTestId('history-policy-current')).toContainText(
      '采集开启 · 保留全部 · 版本 2',
    );
    await save('UI_restores_capture_and_hold_all_after_test');
    const adminAfter = await (
      await page.request.get('/api/v1/account/query-history-policy')
    ).json();
    expect(adminAfter).toEqual(adminBefore);
    await save('operator_history_policy_remains_unchanged');
    // Close the activation-session negative that the original MFA selector
    // interruption could not observe. Reuse the completed account normally.
    const second = await browser.newContext({ baseURL });
    try {
      const target = await second.newPage();
      target.setDefaultTimeout(15000);
      await target.goto('/#/projects');
      await target.getByLabel('用户名', { exact: true }).fill(account.username);
      await target.getByLabel('密码', { exact: true }).fill(account.password);
      await target.getByRole('button', { name: '登录控制台 →', exact: true }).click();
      await expect(target.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
      expect((await second.request.get('/api/v1/session')).status()).toBe(200);
      const mfa = member.getByRole('region', { name: '控制台双重验证' });
      await mfa.getByRole('button', { name: '加载双重验证状态', exact: true }).click();
      await expect(mfa.getByTestId('console-mfa-state')).toContainText('未启用');
      await mfa.getByLabel('双重验证操作密码', { exact: true }).fill(account.password);
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/enroll',
      );
      await mfa.getByRole('button', { name: '设置验证器', exact: true }).click();
      const enrolled = await waiting;
      expect(enrolled.status()).toBe(200);
      const secret = (await enrolled.json()).secret;
      const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
      if (!privateDir) throw new Error('Private checkpoint required');
      const privateFactor = path.join(privateDir, 'history-mfa-activation.private.json');
      await writeFile(
        privateFactor,
        JSON.stringify({
          username: account.username,
          password: account.password,
          secret,
          state: 'pending',
        }),
        { mode: 0o600 },
      );
      await expect(mfa.getByRole('status')).toContainText('请在验证器手动添加此密钥');
      await mfa.getByLabel('双重验证操作密码', { exact: true }).fill(account.password);
      await mfa.getByLabel('双重验证操作验证码', { exact: true }).fill(authenticatorCode(secret));
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/confirm',
      );
      await mfa.getByRole('button', { name: '确认启用双重验证', exact: true }).click();
      const enabled = await waiting;
      expect(enabled.status()).toBe(200);
      const codes = (await enabled.json()).recovery_codes;
      await writeFile(
        privateFactor,
        JSON.stringify({
          username: account.username,
          password: account.password,
          secret,
          recovery_codes: codes,
          state: 'enabled',
        }),
        { mode: 0o600 },
      );
      const displayed = await mfa
        .getByRole('textbox', { name: '一次性恢复码', exact: true })
        .inputValue();
      expect(displayed === codes.join('\n')).toBe(true);
      await mfa.getByRole('button', { name: '已保存，隐藏恢复码', exact: true }).click();
      await save('original_account_reenrollment_displays_actual_one_time_recovery_codes');
      expect((await second.request.get('/api/v1/session')).status()).toBe(401);
      expect((await context.request.get('/api/v1/session')).status()).toBe(200);
      expect((await page.request.get('/api/v1/session')).status()).toBe(200);
      await save('actual_UI_activation_revokes_previously_valid_other_login_and_keeps_operator');
      await mfa.getByLabel('双重验证操作密码', { exact: true }).fill(account.password);
      await mfa.getByLabel('使用一次性恢复码', { exact: true }).check();
      await mfa.getByLabel('双重验证操作验证码', { exact: true }).fill(codes[0]);
      waiting = member.waitForResponse(
        (r) => new URL(r.url()).pathname === '/api/v1/account/mfa/disable',
      );
      await mfa.getByRole('button', { name: '确认关闭双重验证', exact: true }).click();
      expect((await waiting).status()).toBe(200);
      await expect(mfa.getByTestId('console-mfa-state')).toContainText('未启用');
      await target.reload();
      await target.getByLabel('用户名', { exact: true }).fill(account.username);
      await target.getByLabel('密码', { exact: true }).fill(account.password);
      await target.getByRole('button', { name: '登录控制台 →', exact: true }).click();
      await expect(target.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
      await target.getByRole('button', { name: '退出', exact: true }).click();
      await writeFile(
        privateFactor,
        JSON.stringify({
          username: account.username,
          password: account.password,
          state: 'disabled',
        }),
        { mode: 0o600 },
      );
      await save('additional_activation_gate_retires_factor_and_restores_password_login');
    } finally {
      await second.close().catch(() => {});
    }
    await member.getByRole('button', { name: '退出', exact: true }).click();
    result = 'pass';
    await save();
  } finally {
    if (result !== 'pass') result = 'fail';
    await save();
    await context.close().catch(() => {});
  }
});
