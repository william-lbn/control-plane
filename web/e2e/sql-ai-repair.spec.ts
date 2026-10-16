import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('UI repairs a real PostgreSQL failure with explicit context and manual execution', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !privateDir)
    throw new Error('Retained owner/admin/private inputs required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    role: string;
    password_file: string;
  };
  if (f.database !== 'owner_transfer_v1' || f.role !== 'owner_new_v1')
    throw new Error('Original owner fixture required');
  const password = (await readFile(f.password_file, 'utf8')).trim(),
    queryPath = `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}/query`,
    aiPath = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}/ai-gateway/request`;
  let result = 'running',
    queries = 0,
    inferences = 0;
  const checks: string[] = [],
    requestIds: string[] = [],
    historyIds: string[] = [];
  page.on('request', (r) => {
    const p = new URL(r.url()).pathname;
    if (r.method() === 'POST' && p === queryPath) queries++;
    if (r.method() === 'POST' && p === aiPath) inferences++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        request_ids: requestIds,
        history_ids: historyIds,
        sql_requests: queries,
        ai_requests: inferences,
        project_id: f.project_id,
        endpoint_id: f.writer_id,
        production_qualified: false,
        model_accuracy_certified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
  }
  async function submit(action: 'query' | 'explain', status = 200) {
    await page.getByRole('combobox', { name: '查询操作', exact: true }).selectOption(action);
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === queryPath && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page
      .getByRole('button', { name: action === 'query' ? '▶ 执行 SQL' : '▶ Explain', exact: true })
      .click();
    const response = await pending;
    requestIds.push(response.headers()['x-request-id'] || '');
    historyIds.push(response.headers()['x-neon-sql-history-id'] || '');
    expect(response.status()).toBe(status);
    return response.json();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.goto(`/#/projects/${f.project_id}/query`);
    await page
      .getByRole('combobox', { name: '目标 Endpoint', exact: true })
      .selectOption(f.writer_id);
    await page.getByLabel('数据库角色', { exact: true }).fill(f.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(f.database);
    const panel = page.getByTestId('sql-assistant'),
      catalog = await context.request.get(aiPath.replace(/\/request$/, ''));
    expect(catalog.status()).toBe(200);
    const models = await catalog.json();
    expect(models.enabled).toBe(true);
    await panel
      .getByRole('combobox', { name: 'SQL 助手模型', exact: true })
      .selectOption(
        models.models.find((model: { id: string }) => model.id === 'local-sql')?.id ||
          models.models[0].id,
      );
    const broken = 'SELEC 43::integer AS ai_fix_value';
    await page.getByLabel('SQL 查询', { exact: true }).fill(broken);
    expect(queries).toBe(0);
    expect(inferences).toBe(0);
    await expect(panel.getByRole('button', { name: '修复当前 SQL', exact: true })).toHaveCount(0);
    const rejected = await submit('query', 422);
    expect(rejected.code).toBe('sql_failed');
    await expect(page.getByTestId('sql-query-failure')).toBeVisible();
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    expect(queries).toBe(1);
    expect(inferences).toBe(0);
    await save(
      'real_PG_failure_retained_with_request_and_history_identity_no_automatic_model_or_SQL_retry',
    );
    await page.getByLabel('SQL 查询', { exact: true }).fill('SELECT 0');
    await expect(panel.getByRole('button', { name: '修复当前 SQL', exact: true })).toHaveCount(0);
    await page.getByLabel('SQL 查询', { exact: true }).fill(broken);
    await panel.getByRole('button', { name: '修复当前 SQL', exact: true }).click();
    await panel
      .getByLabel('AI 请求说明', { exact: true })
      .fill(
        'Fix only the misspelled SELECT keyword in the supplied SQL. Preserve the integer literal, cast and alias. Return only one SQL fenced block.',
      );
    const preview = panel.getByLabel('发给模型的内容', { exact: true }),
      payload = await preview.inputValue(),
      reference = JSON.parse(JSON.parse(payload).messages[1].content);
    expect(reference.user_selected_sql).toBe(broken);
    expect(reference.user_selected_error.code).toBe('sql_failed');
    expect(reference.user_selected_error.message).toBe(rejected.message);
    for (const denied of [password, f.database, f.role]) expect(payload).not.toContain(denied);
    await expect(panel.getByRole('button', { name: '生成修复草稿', exact: true })).toBeDisabled();
    await panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }).check();
    await panel.getByRole('checkbox', { name: '附带当前错误信息', exact: true }).uncheck();
    await expect(
      panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }),
    ).not.toBeChecked();
    await expect(panel.getByRole('button', { name: '生成修复草稿', exact: true })).toBeDisabled();
    await panel.getByRole('checkbox', { name: '附带当前错误信息', exact: true }).check();
    await save(
      'repair_scope_matches_original_failure_and_requires_selected_SQL_error_and_fresh_consent',
    );
    await panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }).check();
    const generated = page.waitForResponse(
      (r) => new URL(r.url()).pathname === aiPath && r.request().method() === 'POST',
      { timeout: 200000 },
    );
    await panel.getByRole('button', { name: '生成修复草稿', exact: true }).click();
    const response = await generated;
    requestIds.push(response.headers()['x-request-id'] || '');
    await writeFile(
      path.join(privateDir, 'sql-ai-repair-response.private.json'),
      JSON.stringify(await response.json()),
      { mode: 0o600 },
    );
    expect(response.status()).toBe(200);
    await expect(
      panel
        .getByLabel('AI SQL 草稿', { exact: true })
        .or(panel.getByTestId('sql-assistant-error'))
        .first(),
    ).toBeVisible({ timeout: 200000 });
    expect(await panel.getByTestId('sql-assistant-error').count()).toBe(0);
    const repaired = await panel.getByLabel('AI SQL 草稿', { exact: true }).inputValue();
    expect(repaired.trim()).toMatch(
      /^SELECT\s+43(?:::\s*(?:integer|int|int4))?\s+AS\s+ai_fix_value\s*;?$/i,
    );
    expect(queries).toBe(1);
    expect(inferences).toBe(1);
    await save('actual_local_model_returns_a_repair_without_dispatching_it');
    await panel.getByRole('button', { name: '载入 SQL 编辑器', exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(repaired);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    await expect(page.getByTestId('sql-query-failure')).toHaveCount(0);
    expect(queries).toBe(1);
    const executed = await submit('query');
    expect(executed.rows).toEqual([[43]]);
    expect(queries).toBe(2);
    await save('manual_UI_submission_of_original_model_repair_returns_43_through_real_Proxy');
    await submit('explain');
    await panel
      .getByRole('combobox', { name: 'AI 建议类型', exact: true })
      .selectOption('plan_advice');
    await expect(panel.getByTestId('sql-assistant-plan-measurement')).toContainText(
      '当前仅估算：未采集执行耗时或实际行数',
    );
    expect(queries).toBe(3);
    expect(inferences).toBe(1);
    await save(
      'deterministic_plan_measurement_label_does_not_depend_on_model_claims_or_call_the_model',
    );
    await page.screenshot({ path: info.outputPath('sql-ai-repair.png'), fullPage: true });
    await page.goto(`/#/projects/${f.project_id}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('original_failed_history_and_business_data_preserved_compute_zero');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
