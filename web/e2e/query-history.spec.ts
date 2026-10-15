import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
test('UI real SQL execution history completed failed truncated and metadata reads at zero', async ({
  page,
  context,
}, info) => {
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!ownerFile || !adminFile) throw new Error('Protected Linux owner fixture required');
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')),
    project = owner.project_id,
    writer = owner.writer_id,
    base = `/api/v1/projects/${project}/query-history`,
    password = (await readFile(owner.password_file, 'utf8')).trim();
  if (owner.database !== 'owner_transfer_v1' || owner.role !== 'owner_new_v1')
    throw new Error('Retained owner fixture required');
  const checks: string[] = [],
    ids: string[] = [];
  let result = 'running',
    submissions = 0;
  page.setDefaultTimeout(30000);
  page.on('request', (r) => {
    if (r.method() === 'POST' && /\/query(-script)?$/.test(new URL(r.url()).pathname))
      submissions++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        history_ids: ids,
        project_id: project,
        writer_id: writer,
        sql_submissions: submissions,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function execute(sql: string, script = false) {
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('执行模式', { exact: true }).selectOption(script ? 'script' : 'single');
    await page.getByLabel('SQL 查询', { exact: true }).fill(sql);
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    const waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname ===
          `/api/v1/projects/${project}/endpoints/${writer}/${script ? 'query-script' : 'query'}` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page
      .getByRole('button', { name: script ? '▶ 执行脚本' : '▶ 执行 SQL', exact: true })
      .click();
    const response = await waiting;
    expect(response.status()).toBe(200);
    const id = response.headers()['x-neon-sql-history-id'];
    expect(id).toMatch(/^qh_[a-f0-9]{24}$/);
    ids.push(id);
    await save();
    await expect(page.getByTestId('query-history-' + id)).toBeVisible();
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    return response.json();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.goto(`/#/projects/${project}/query`);
    const config = await context.request.get(base);
    expect(config.status()).toBe(200);
    expect((await config.json()).enabled).toBe(true);
    await expect(page.getByTestId('query-history-panel')).toContainText('仅自己可读');
    const sql = "SELECT 'history single 中文'::text AS marker";
    await execute(sql);
    await expect(page.getByTestId('query-history-' + ids[0])).toContainText('已完成');
    const list = await (await context.request.get(base)).json();
    expect(list.items.find((v: { id: string }) => v.id === ids[0]).sql).toBeUndefined();
    const detail = await context.request.get(base + '/' + ids[0]);
    expect(detail.headers()['cache-control']).toBe('no-store');
    expect((await detail.json()).sql).toBe(sql);
    await save('ui_real_single_query_durably_recorded_completed_with_metadata_only_list');
    const failed = await execute('SELECT 1; SELECT FROM;', true);
    expect(failed.outcome).toBe('failed');
    await expect(page.getByTestId('query-history-' + ids[1])).toContainText('SQL 失败');
    expect((await (await context.request.get(base + '/' + ids[1])).json()).sqlstate).toBe('42601');
    await save('ui_real_script_sql_failure_is_recorded_without_false_success_or_replay');
    const long = 'SELECT 73 AS history_truncation; /*' + '中文'.repeat(2200) + '*/';
    await execute(long);
    const truncated = await (await context.request.get(base + '/' + ids[2])).json();
    expect(truncated.truncated).toBe(true);
    expect(Buffer.byteLength(truncated.sql, 'utf8')).toBeLessThanOrEqual(9216);
    expect(truncated.sql.startsWith('-- QUERY TRUNCATED\n')).toBe(true);
    await expect(page.getByTestId('query-history-' + ids[2])).toContainText('截断');
    await page.getByLabel('数据库角色密码', { exact: true }).fill('synthetic-not-submitted');
    await page.getByRole('button', { name: '加载历史 ' + ids[2], exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(truncated.sql);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    await expect(page.getByTestId('query-history-notice')).toContainText('前 9 KiB');
    expect(submissions).toBe(3);
    await save(
      'ui_utf8_history_truncation_is_visible_loads_only_recorded_text_and_clears_password',
    );
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await page.goto(`/#/projects/${project}/query`);
    await expect(page.getByTestId('query-history-' + ids[0])).toBeVisible();
    await page.getByRole('button', { name: '加载历史 ' + ids[0], exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(sql);
    await expect(page.getByLabel('数据库角色', { exact: true })).toHaveValue(owner.role);
    await expect(page.getByLabel('目标数据库', { exact: true })).toHaveValue(owner.database);
    expect(submissions).toBe(3);
    await expect(page.getByTestId('query-history-' + ids[0])).toContainText('已完成');
    await page.screenshot({ path: info.outputPath('sql-query-history.png'), fullPage: true });
    await save('ui_history_survives_reload_and_loads_at_compute_zero_without_sql_submission');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
