import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

// This slice checks the new Worker admission guard. The separate fractional CPU
// suite owns quota transitions; do not repeat those transitions here.
test('UI new fractional Reader passes real runner admission and retains database identity', async ({
  page,
  context,
}, info) => {
  test.setTimeout(600000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !adminFile) throw new Error('Protected operator and owner fixture required');
  const owner = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const project = owner.project_id,
    branch = owner.branch_id;
  let endpoint = '',
    result = 'running';
  const checks: string[] = [],
    operations: string[] = [];
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        project_id: project,
        branch_id: branch,
        endpoint_id: endpoint,
        production_qualified: false,
        secrets_in_evidence: false,
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
    await page.goto(`/#/projects/${project}/branches/${branch}`);
    await page.getByRole('button', { name: /^＋ (添加 Compute|创建 Endpoint)$/ }).click();
    await page.getByLabel('Compute 类型', { exact: true }).selectOption('read_only');
    await page.getByLabel('计算资源预设', { exact: true }).selectOption('fractional');
    const creation = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const response = await creation;
    expect(response.status()).toBe(202);
    const created = await response.json();
    endpoint = created.resource.id;
    operations.push(created.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const response = await context.request.get(
            `/api/v1/projects/${project}/operations/${created.operation.id}`,
          );
          expect(response.status()).toBe(200);
          const op = await response.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Fractional admission ${op.id}: ${op.error_code}`);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    const r = await context.request.get(`/api/v1/projects/${project}/endpoints/${endpoint}`);
    expect(r.status()).toBe(200);
    expect(await r.json()).toMatchObject({
      endpoint_type: 'read_only',
      branch_id: branch,
      min_cpu_milli: 250,
      max_cpu_milli: 500,
    });
    await save('new_ui_reader_operation_passes_worker_real_cgroup_runner_admission');
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(endpoint);
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('执行模式', { exact: true }).selectOption('single');
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill('SELECT pg_is_in_recovery(), current_database()');
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(owner.password_file, 'utf8')).trim());
    const query = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query`,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const sql = await query;
    expect(sql.status()).toBe(200);
    expect((await sql.json()).rows).toEqual([[true, owner.database]]);
    await save('new_fractional_reader_real_proxy_sql_uses_existing_database_and_role');
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByLabel('选择 Endpoint').selectOption(endpoint);
    await page.screenshot({ path: info.outputPath('fractional-admission.png'), fullPage: true });
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('new_reader_suspended_after_admission_without_deleting_database_or_evidence');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
