import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('UI session error is replaced by confirmed success for retained test account', async ({
  page,
  context,
  browser,
  baseURL,
}, info) => {
  test.setTimeout(180000);
  const file = process.env.NEON_E2E_CONSOLE_ACCOUNT_FIXTURE;
  if (!file || !baseURL) throw new Error('Protected retained test account required');
  const account = JSON.parse(await readFile(file, 'utf8'));
  if (!/^sessions_[0-9]+$/.test(account.username)) throw new Error('Owned test account only');
  const other = await browser.newContext({ baseURL });
  let result = 'running';
  const checks: string[] = [];
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        compute_created: false,
        sql_requests: 0,
        retained_account: true,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function login(p: typeof page) {
    await p.goto('/#/projects');
    await p.getByLabel('用户名').fill(account.username);
    await p.getByLabel('密码', { exact: true }).fill(account.password);
    await p.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(p.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
  }
  try {
    await save();
    await login(page);
    await login(await other.newPage());
    await page.goto('/#/organization');
    page.on('dialog', (d) => void d.accept());
    const panel = page.getByRole('region', { name: '我的控制台会话' });
    await panel.getByRole('button', { name: '加载我的会话', exact: true }).click();
    await expect(panel.getByRole('row')).toHaveCount(3);
    await panel.getByLabel('会话管理密码', { exact: true }).fill('incorrect-password');
    let wait = page.waitForResponse(
      (r) => new URL(r.url()).pathname === '/api/v1/account/sessions/revoke-others',
    );
    await panel.getByRole('button', { name: '撤销其他全部会话', exact: true }).click();
    expect((await wait).status()).toBe(401);
    await expect(panel.getByRole('alert')).toContainText('Current password');
    await panel.getByLabel('会话管理密码', { exact: true }).fill(account.password);
    wait = page.waitForResponse(
      (r) => new URL(r.url()).pathname === '/api/v1/account/sessions/revoke-others',
    );
    await panel.getByRole('button', { name: '撤销其他全部会话', exact: true }).click();
    expect((await wait).status()).toBe(200);
    await expect(panel.getByRole('alert')).toHaveCount(0);
    await expect(panel).toContainText('已撤销 1 个会话');
    await expect(panel.getByLabel('会话管理密码', { exact: true })).toHaveValue('');
    expect((await other.request.get('/api/v1/session')).status()).toBe(401);
    expect((await context.request.get('/api/v1/session')).status()).toBe(200);
    await save('ui_replaces_local_error_with_success_and_clears_password_using_retained_account');
    await panel.screenshot({
      path: info.outputPath('session-feedback.png'),
      mask: [panel.locator('input[type=password]')],
    });
    await page.getByRole('button', { name: '退出', exact: true }).click();
    await save('owned_ui_feedback_sessions_logged_out_no_compute_or_sql');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  } finally {
    await other.close();
  }
});
