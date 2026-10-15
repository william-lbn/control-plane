import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('UI visualizes real Explain and Analyze trees without inventing actual statistics', async ({
  page,
}, info) => {
  test.setTimeout(600000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !adminFile) throw new Error('Protected operator and owner fixture required');
  const owner = JSON.parse(await readFile(fixtureFile, 'utf8')),
    project = owner.project_id,
    endpoint = owner.writer_id;
  const checks: string[] = [];
  let result = 'running',
    queries = 0;
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        project_id: project,
        endpoint_id: endpoint,
        sql_requests: queries,
        production_qualified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
  }
  page.on('request', (r) => {
    if (
      new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query` &&
      r.method() === 'POST'
    )
      queries++;
  });
  async function execute(action: 'explain' | 'analyze') {
    await page.getByLabel('查询操作').selectOption(action);
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill('SELECT n FROM generate_series(1,20) AS n ORDER BY n DESC LIMIT 7');
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(owner.password_file, 'utf8')).trim());
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query`,
      { timeout: 190000 },
    );
    await page
      .getByRole('button', {
        name: action === 'explain' ? '▶ Explain' : '▶ Analyze',
        exact: true,
      })
      .click();
    const response = await pending;
    expect(response.status()).toBe(200);
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
    await page.getByLabel('目标 Endpoint').selectOption(endpoint);
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('执行模式', { exact: true }).selectOption('single');
    const estimated = await execute('explain');
    expect(estimated.rows[0][0][0].Plan['Node Type']).toBe('Limit');
    const tree = page.getByRole('region', { name: 'SQL 执行计划树' });
    await expect(tree.getByRole('heading', { name: '估算执行计划', exact: true })).toBeVisible();
    await expect(tree).toContainText('Sort');
    await expect(tree).toContainText('Function Scan');
    await expect(tree).toContainText('估算行数');
    await expect(tree).not.toContainText('实际行/循环');
    await save('ui_real_explain_tree_displays_estimates_without_fabricating_actual_rows');
    await tree.getByRole('button', { name: '折叠 Limit', exact: true }).click();
    await expect(tree.getByText('Function Scan', { exact: true })).toHaveCount(0);
    await tree.getByRole('button', { name: '展开 Limit', exact: true }).click();
    await expect(tree.getByText('Function Scan', { exact: true })).toBeVisible();
    expect(queries).toBe(1);
    await save('ui_plan_nodes_collapse_and_expand_without_additional_sql');
    const actual = await execute('analyze');
    expect(actual.rows[0][0][0].Plan['Actual Rows']).toBe(7);
    await expect(tree.getByRole('heading', { name: '实际执行计划', exact: true })).toBeVisible();
    await expect(tree).toContainText('实际行/循环');
    await expect(tree).toContainText('实际循环');
    await expect(tree).toContainText('实际 ms/循环');
    await expect(tree).toContainText('父子节点耗时有包含关系');
    await expect(page.locator('.result-panel pre')).toContainText('Actual Rows');
    expect(queries).toBe(2);
    await save('ui_real_analyze_tree_has_actual_metrics_and_retains_original_json');
    await page.screenshot({ path: info.outputPath('sql-plan-tree.png'), fullPage: true });
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByLabel('选择 Endpoint').selectOption(endpoint);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_compute_stopped_no_application_data_modified');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
