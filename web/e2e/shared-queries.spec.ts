import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
test('UI explicitly shares and revokes encrypted query drafts without executing SQL', async ({
  page,
  context,
}, info) => {
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!ownerFile || !adminFile) throw new Error('Protected Linux project fixture required');
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')),
    project = owner.project_id,
    base = `/api/v1/projects/${project}/saved-queries`;
  let result = 'running',
    id = '',
    executions = 0;
  const checks: string[] = [];
  const name = 'ci-shared-' + Date.now(),
    sql = "SELECT 'shared local draft 中文';\nSELECT 73;";
  page.setDefaultTimeout(30000);
  page.on('request', (r) => {
    if (r.method() === 'POST' && /\/query(-script)?$/.test(new URL(r.url()).pathname)) executions++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        project_id: project,
        query_id: id,
        sql_executed: executions !== 0,
        compute_created: false,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('SQL 查询', { exact: true }).fill(sql);
    await page.getByLabel('保存查询名称', { exact: true }).fill(name);
    const creating = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '另存为新查询', exact: true }).click();
    const created = await creating;
    expect(created.status()).toBe(201);
    const draft = (await created.json()).query;
    id = draft.id;
    expect(draft.is_shared).toBe(false);
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '';
    expect(
      (
        (await (await context.request.get(base + '?view=shared')).json()).items as { id: string }[]
      ).some((v) => v.id === id),
    ).toBe(false);
    await save('ui_new_draft_private_and_absent_from_project_shared_view');
    const sharing = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/' + id + '/sharing' &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '共享到项目', exact: true }).click();
    const shared = await sharing;
    expect(shared.status()).toBe(200);
    expect((await shared.json()).query.is_shared).toBe(true);
    expect(shared.request().postDataJSON().shared).toBe(true);
    await expect(page.getByRole('button', { name: '撤销项目共享', exact: true })).toBeEnabled();
    await save('ui_author_explicit_consent_shares_with_project_sql_editors');
    const listing = await context.request.get(base + '?view=shared');
    expect(listing.status()).toBe(200);
    const listed = (await listing.json()).items.find((v: { id: string }) => v.id === id);
    expect(listed.can_edit).toBe(true);
    expect(listed.sql).toBeUndefined();
    await page.reload();
    await page.getByLabel('保存查询视图', { exact: true }).selectOption('shared');
    await expect(
      page.getByLabel('已保存查询', { exact: true }).locator(`option[value="${id}"]`),
    ).toHaveCount(1);
    await page.getByLabel('已保存查询', { exact: true }).selectOption(id);
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill('synthetic-not-submitted-password');
    await page.getByRole('button', { name: '加载到编辑器', exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(sql);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    expect(executions).toBe(0);
    await save('ui_project_shared_view_loads_exact_sql_clears_password_and_does_not_execute');
    const stale = await context.request.post(base + '/' + id + '/sharing', {
      headers: {
        'X-CSRF-Token': csrf,
        'Idempotency-Key': 'stale-unshare-' + id,
        'If-Match': '"1"',
      },
      data: { shared: false },
    });
    expect(stale.status()).toBe(412);
    await save('stale_sharing_generation_denied_without_overwriting_concurrent_state');
    const revoking = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/' + id + '/sharing' &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '撤销项目共享', exact: true }).click();
    expect((await revoking).status()).toBe(200);
    await expect(
      page.getByLabel('已保存查询', { exact: true }).locator(`option[value="${id}"]`),
    ).toHaveCount(0);
    const detail = await context.request.get(base + '/' + id);
    expect(detail.status()).toBe(200);
    expect((await detail.json()).sql).toBe(sql);
    await save('ui_revoke_removes_shared_listing_but_keeps_original_private_draft');
    await page.getByLabel('保存查询视图', { exact: true }).selectOption('personal');
    await page.getByLabel('已保存查询', { exact: true }).selectOption(id);
    const deleting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/' + id && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '删除保存查询', exact: true }).click();
    expect((await deleting).status()).toBe(200);
    expect((await context.request.get(base + '/' + id)).status()).toBe(404);
    expect(executions).toBe(0);
    await expect(page.getByTestId('saved-query-error')).toHaveCount(0);
    await page.screenshot({ path: info.outputPath('shared-queries.png'), fullPage: true });
    await save('owned_draft_retired_with_tombstone_and_zero_sql_or_compute_activity');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
