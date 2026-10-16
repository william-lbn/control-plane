import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
test('UI compares real Explain and Analyze snapshots without extra SQL or persistent baseline', async ({
  page,
}, info) => {
  test.setTimeout(480000);
  const fixture = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixture || !adminFile) throw new Error('Protected SQL owner fixture required');
  const owner = JSON.parse(await readFile(fixture, 'utf8')),
    project = owner.project_id,
    writer = owner.writer_id,
    checks: string[] = [];
  let result = 'running',
    queries = 0;
  page.setDefaultTimeout(20000);
  page.on('request', (r) => {
    if (r.method() === 'POST' && /\/query(-script)?$/.test(new URL(r.url()).pathname)) queries++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        project_id: project,
        sql_requests: queries,
        data_modified: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function execute(action: 'explain' | 'analyze', limit: number) {
    await page.getByRole('combobox', { name: '查询操作', exact: true }).selectOption(action);
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill(`SELECT n FROM generate_series(1,20) n ORDER BY n DESC LIMIT ${limit}`);
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(owner.password_file, 'utf8')).trim());
    const waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page
      .getByRole('button', {
        name: action === 'explain' ? '▶ Explain' : '▶ Analyze',
        exact: true,
      })
      .click();
    const response = await waiting;
    expect(response.status()).toBe(200);
    return response.json();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.goto(`/#/projects/${project}/query`);
    await page.getByRole('combobox', { name: '目标 Endpoint', exact: true }).selectOption(writer);
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    const panel = page.getByRole('region', { name: 'SQL 执行计划比较', exact: true });
    await expect(panel.getByRole('button', { name: '设为比较基线', exact: true })).toBeDisabled();
    await execute('explain', 7);
    await panel.getByRole('button', { name: '设为比较基线', exact: true }).click();
    await expect(panel.getByTestId('plan-baseline')).toContainText('Explain');
    expect(queries).toBe(1);
    await save('actual_Explain_result_saved_as_memory_baseline_without_new_SQL');
    const second = await execute('explain', 3);
    expect(second.rows[0][0][0].Plan['Plan Rows']).toBe(3);
    const estimated = panel.getByTestId('plan-delta-rows-根节点估算行数');
    await expect(estimated).toContainText('7 rows');
    await expect(estimated).toContainText('3 rows');
    await expect(estimated).toContainText('-4 rows');
    await expect(panel.getByRole('table', { name: '计划结构差异', exact: true })).toContainText(
      'Function Scan',
    );
    await expect(panel).toContainText('两次均为 Analyze');
    expect(queries).toBe(2);
    await save('second_real_Explain_compares_cost_rows_and_structure_without_invented_actuals');
    await execute('analyze', 3);
    await expect(panel).toContainText('两次均为 Analyze');
    await panel.getByRole('button', { name: '设为比较基线', exact: true }).click();
    await execute('analyze', 2);
    const actual = panel.getByTestId('plan-delta-rows/loop-根节点实际行/循环');
    await expect(actual).toContainText('3 rows/loop');
    await expect(actual).toContainText('2 rows/loop');
    await expect(actual).toContainText('-1 rows/loop');
    await expect(panel.getByRole('table', { name: '计划指标差异', exact: true })).toContainText(
      '实际执行耗时',
    );
    expect(queries).toBe(4);
    await save('both_real_Analyze_snapshots_compare_actual_metrics_without_claiming_speedup');
    await panel.getByRole('button', { name: '清除比较基线', exact: true }).click();
    await expect(panel.getByTestId('plan-baseline')).toHaveCount(0);
    await panel.getByRole('button', { name: '设为比较基线', exact: true }).click();
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database + '_not_submitted');
    await expect(panel.getByTestId('plan-baseline')).toHaveCount(0);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    expect(queries).toBe(4);
    await save('explicit_clear_and_connection_scope_change_remove_baseline_without_SQL');
    await page.reload();
    await expect(panel.getByTestId('plan-baseline')).toHaveCount(0);
    expect(queries).toBe(4);
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('combobox', { name: '选择 Endpoint', exact: true }).selectOption(writer);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('reload_keeps_no_persisted_plan_and_owned_compute_returns_zero');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
