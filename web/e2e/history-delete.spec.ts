import { test, expect, type APIResponse } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('UI explicitly deletes only its new terminal history at compute zero', async ({
  page,
  context,
}, info) => {
  test.setTimeout(300000);
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!ownerFile || !adminFile) throw new Error('Protected retained SQL owner fixture required');
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')),
    project = owner.project_id,
    writer = owner.writer_id,
    base = `/api/v1/projects/${project}/query-history`,
    checks: string[] = [];
  let result = 'running',
    id = '',
    submissions = 0,
    uiDeletes = 0;
  const before: string[] = [];
  page.setDefaultTimeout(20000);
  page.on('request', (r) => {
    const pathname = new URL(r.url()).pathname;
    if (r.method() === 'POST' && /\/query(-script)?$/.test(pathname)) submissions++;
    if (r.method() === 'DELETE' && pathname.startsWith(base + '/')) uiDeletes++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        history_id: id,
        project_id: project,
        writer_id: writer,
        prior_history_ids: before,
        sql_submissions: submissions,
        ui_delete_requests: uiDeletes,
        deletes_only_this_test_entry: true,
        existing_history_deleted: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function ids() {
    const values: string[] = [];
    let cursor: string | null = null;
    for (let count = 0; count < 100; count++) {
      const response: APIResponse = await context.request.get(
        base + (cursor ? '?cursor=' + encodeURIComponent(cursor) : ''),
      );
      expect(response.status()).toBe(200);
      const value: { enabled: boolean; items: { id: string }[]; next_cursor: string | null } =
        await response.json();
      expect(value.enabled).toBe(true);
      values.push(...value.items.map((item: { id: string }) => item.id));
      cursor = value.next_cursor;
      if (!cursor) return values.sort();
    }
    throw new Error('History listing exceeds bounded acceptance fixture; no deletion performed');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    before.push(...(await ids()));
    await save('all_existing_personal_history_ids_recorded_before_new_query');
    await page.goto(`/#/projects/${project}/query`);
    const sql = `SELECT 'history-delete-${Date.now()}'::text AS marker`;
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('SQL 查询', { exact: true }).fill(sql);
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(owner.password_file, 'utf8')).trim());
    const waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(200);
    id = response.headers()['x-neon-sql-history-id'];
    expect(id).toMatch(/^qh_[a-f0-9]{24}$/);
    await save();
    await expect(page.getByTestId('query-history-' + id)).toContainText('已完成');
    const detail = await context.request.get(base + '/' + id);
    expect(detail.status()).toBe(200);
    const created = await detail.json();
    expect(created.sql).toBe(sql);
    await writeFile(
      info.outputPath('created-history-before-delete.json'),
      JSON.stringify(created) + '\n',
    );
    expect(await ids()).toEqual([...before, id].sort());
    await save('one_real_UI_SQL_query_created_and_its_original_history_receipt_preserved');
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await page.goto(`/#/projects/${project}/query`);
    await page.getByRole('button', { name: '加载历史 ' + id, exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(sql);
    await page.getByRole('button', { name: '删除历史 ' + id, exact: true }).click();
    const confirmation = page.getByRole('group', { name: '确认删除历史记录', exact: true });
    await expect(confirmation).toContainText(id);
    await confirmation.getByRole('button', { name: '取消删除历史', exact: true }).click();
    expect(uiDeletes).toBe(0);
    expect((await context.request.get(base + '/' + id)).status()).toBe(200);
    await save('UI_cancel_preserves_exact_entry_without_DELETE_or_Compute_wake');
    await page.getByRole('button', { name: '删除历史 ' + id, exact: true }).click();
    const deletedResponse = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/' + id && r.request().method() === 'DELETE',
    );
    await confirmation.getByRole('button', { name: '确认删除此历史', exact: true }).click();
    expect((await deletedResponse).status()).toBe(204);
    await expect(page.getByTestId('query-history-' + id)).toHaveCount(0);
    await expect(page.getByTestId('query-history-notice')).toContainText('已删除本人的历史记录');
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(sql);
    expect((await context.request.get(base + '/' + id)).status()).toBe(404);
    expect(await ids()).toEqual(before);
    expect(uiDeletes).toBe(1);
    await save(
      'confirmed_UI_DELETE_removed_only_new_owned_entry_and_left_editor_and_prior_history',
    );
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('Authenticated browser CSRF required');
    const replay = await context.request.delete(
      base + '/' + id + '?acknowledge_history_deletion=true',
      { headers: { 'X-CSRF-Token': csrf } },
    );
    expect(replay.status()).toBe(204);
    await page.reload();
    await expect(page.getByTestId('query-history-panel')).toBeVisible();
    expect(await ids()).toEqual(before);
    expect(submissions).toBe(1);
    await page.goto(`/#/projects/${project}/compute`);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠');
    await save(
      'DELETE_retry_and_browser_reload_are_idempotent_with_all_old_records_and_compute_zero',
    );
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
