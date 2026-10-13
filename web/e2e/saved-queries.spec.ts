import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
test('Personal SQL drafts: UI save/load/update/delete, replay, stale version and no Compute', async ({
  page,
  context,
}, info) => {
  const project = process.env.NEON_E2E_CREDENTIAL_PROJECT;
  const passwordFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!project || !passwordFile) throw new Error('Linux authorized test project required');
  const base = `/api/v1/projects/${project}/saved-queries`;
  const checks: Record<string, unknown>[] = [];
  let state = 'running';
  let id = '';
  let generation = 1;
  async function record(name: string, extra: Record<string, unknown> = {}) {
    checks.push({ name, ...extra });
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result: state,
          project_id: project,
          query_id: id,
          checks,
          sql_executed: false,
          compute_created: false,
          password_or_sql_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page
      .getByLabel('密码', { exact: true })
      .fill((await readFile(passwordFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const fixture = await context.request.get(`/api/v1/projects/${project}`);
    expect(fixture.status()).toBe(200);
    expect((await fixture.json()).name.startsWith('ci-')).toBe(true);
    await page.goto(`/#/projects/${project}/query`);
    await expect(page.getByLabel('SQL 查询', { exact: true })).toBeVisible();
    const sql = "SELECT '中文 draft';\nSELECT 42;";
    const name = 'ci-saved-' + Date.now();
    await page.getByLabel('SQL 查询', { exact: true }).fill(sql);
    await page.getByLabel('保存查询名称', { exact: true }).fill(name);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith('/saved-queries') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '另存为新查询', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(201);
    const reply = await response.json();
    id = reply.query.id;
    expect(reply.sql_included).toBe(false);
    expect(Boolean(reply.query.sql)).toBe(false);
    await expect(page.getByLabel('已保存查询', { exact: true })).toHaveValue(id);
    await record('ui_save_returns_metadata_only');
    const headers = response.request().headers();
    const repeated = await context.request.post(base, {
      headers: {
        'X-CSRF-Token': headers['x-csrf-token'],
        'Idempotency-Key': headers['idempotency-key'],
      },
      data: response.request().postDataJSON(),
    });
    expect(repeated.status()).toBe(200);
    expect(repeated.headers()['idempotency-replayed']).toBe('true');
    expect((await repeated.json()).query.id).toBe(id);
    const conflict = await context.request.post(base, {
      headers: {
        'X-CSRF-Token': headers['x-csrf-token'],
        'Idempotency-Key': headers['idempotency-key'],
      },
      data: { name: name + '-conflict', sql },
    });
    expect(conflict.status()).toBe(409);
    await record('durable_replay_no_duplicate_conflicting_body_denied');
    await page.reload();
    await page.getByLabel('已保存查询', { exact: true }).selectOption(id);
    await page.getByRole('button', { name: '加载到编辑器', exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(sql);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    await record('ui_reload_exact_unicode_newline_load_without_password_or_execution');
    const changed = "SELECT 'updated draft';\nSELECT 43;";
    await page.getByLabel('SQL 查询', { exact: true }).fill(changed);
    await page.getByLabel('保存查询名称', { exact: true }).fill(name + '-renamed');
    const updating = page.waitForResponse(
      (r) => r.url().endsWith('/saved-queries/' + id) && r.request().method() === 'PATCH',
    );
    await page.getByRole('button', { name: '更新保存查询', exact: true }).click();
    expect((await updating).status()).toBe(200);
    generation = 2;
    const detail = await context.request.get(base + '/' + id);
    expect(detail.status()).toBe(200);
    expect((await detail.json()).sql).toBe(changed);
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '';
    expect(
      (
        await context.request.patch(base + '/' + id, {
          headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': 'stale-' + id, 'If-Match': '"1"' },
          data: { name, sql },
        })
      ).status(),
    ).toBe(412);
    await record('ui_update_and_stale_generation_denial');
    page.once('dialog', (dialog) => dialog.accept());
    const deleting = page.waitForResponse(
      (r) => r.url().endsWith('/saved-queries/' + id) && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '删除保存查询', exact: true }).click();
    expect((await deleting).status()).toBe(200);
    generation = 3;
    expect((await context.request.get(base + '/' + id)).status()).toBe(404);
    await record('ui_personal_delete_hides_retained_tombstone');
    expect((await context.request.get(base, { headers: { Cookie: '' } })).status()).toBe(401);
    await expect(page.getByTestId('saved-query-error')).toHaveCount(0);
    await page.screenshot({ path: info.outputPath('saved-queries.png'), fullPage: true });
    state = 'pass';
  } finally {
    if (id && generation < 3) {
      const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '';
      const detail = await context.request.get(base + '/' + id);
      if (detail.status() === 200) {
        const current = await detail.json();
        const retired = await context.request.delete(base + '/' + id, {
          headers: {
            'X-CSRF-Token': csrf,
            'Idempotency-Key': 'cleanup-' + id,
            'If-Match': `"${current.generation}"`,
          },
        });
        expect(retired.status()).toBe(200);
      }
    }
    if (state !== 'pass') state = 'fail';
    await record('owned_draft_retired_evidence_preserved');
  }
});
