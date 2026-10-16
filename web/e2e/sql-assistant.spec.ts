import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('UI local-model SQL draft and plan advice require consent and manual SQL execution', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !privateDir)
    throw new Error('Protected retained owner/admin/private inputs required');
  const protectedPath: string = privateDir;
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
    project = f.project_id,
    writer = f.writer_id;
  const aiPath = `/api/v1/projects/${project}/branches/${f.branch_id}/ai-gateway/request`,
    queryPath = `/api/v1/projects/${project}/endpoints/${writer}/query`;
  const checks: string[] = [],
    requestIds: string[] = [];
  let result = 'running',
    queries = 0,
    inferences = 0;
  const checkpoint: { draft: string; advice: string; checks: string[]; request_ids: string[] } = {
    draft: '',
    advice: '',
    checks,
    request_ids: requestIds,
  };
  page.on('request', (r) => {
    const p = new URL(r.url()).pathname;
    if (r.method() === 'POST' && p === queryPath) queries++;
    if (r.method() === 'POST' && p === aiPath) inferences++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      path.join(protectedPath, 'sql-assistant-original.private.json'),
      JSON.stringify(checkpoint),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        request_ids: requestIds,
        sql_requests: queries,
        ai_requests: inferences,
        project_id: project,
        branch_id: f.branch_id,
        endpoint_id: writer,
        production_qualified: false,
        model_accuracy_certified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
  }
  async function sql(action: 'query' | 'explain') {
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
    await page.getByLabel('数据库角色', { exact: true }).fill(f.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(f.database);
    const panel = page.getByTestId('sql-assistant');
    await expect(panel).toBeVisible();
    const catalog = await context.request.get(aiPath.replace(/\/request$/, ''));
    expect(catalog.status()).toBe(200);
    const models = await catalog.json();
    expect(models.enabled).toBe(true);
    expect(models.models.length).toBeGreaterThan(0);
    await panel
      .getByRole('combobox', { name: 'SQL 助手模型', exact: true })
      .selectOption(models.models[0].id);
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill("SELECT 'assistant_sensitive_literal_not_selected'::text");
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    await panel
      .getByLabel('AI 请求说明', { exact: true })
      .fill(
        'Generate exactly this one PostgreSQL statement: SELECT 42::integer AS ai_draft_value; Return the SQL draft, no other statements or functions.',
      );
    const preview = panel.getByLabel('发给模型的内容', { exact: true });
    await expect(preview).toHaveValue(/ai_draft_value/);
    const before = await preview.inputValue();
    expect(before).not.toContain(password);
    expect(before).not.toContain('assistant_sensitive_literal_not_selected');
    expect(before).not.toContain(f.database);
    expect(before).not.toContain(f.role);
    expect(queries).toBe(0);
    expect(inferences).toBe(0);
    await expect(panel.getByRole('button', { name: '生成 SQL 草稿', exact: true })).toBeDisabled();
    await save(
      'UI_preview_excludes_connection_password_role_database_current_SQL_and_no_inference_before_consent',
    );
    await panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }).check();
    await panel.getByText('可选 Schema 上下文 · 受控读取或手工提供', { exact: true }).click();
    const schemaRead = page.waitForResponse(
      (r) => new URL(r.url()).pathname === queryPath && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await panel.getByRole('button', { name: '加载 public Schema', exact: true }).click();
    const schemaResponse = await schemaRead;
    expect(schemaResponse.status()).toBe(200);
    const schemaRequest = schemaResponse.request().postDataJSON();
    expect(schemaRequest.sql).toContain('information_schema.columns');
    expect(schemaRequest.sql).toContain("table_schema='public'");
    expect(schemaRequest.sql).not.toContain('column_default');
    await expect(panel.getByTestId('sql-assistant-schema-notice')).toBeVisible();
    const schema = JSON.parse(
      await panel.getByLabel('AI Schema 上下文', { exact: true }).inputValue(),
    );
    expect(schema.source).toBe('role_filtered_information_schema');
    expect(schema.schema).toBe('public');
    expect(typeof schema.truncated).toBe('boolean');
    expect(schema.columns.length).toBeLessThanOrEqual(60);
    expect(queries).toBe(1);
    expect(inferences).toBe(0);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(
      "SELECT 'assistant_sensitive_literal_not_selected'::text",
    );
    await expect(
      panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }),
    ).not.toBeChecked();
    await save(
      'explicit_role_filtered_catalog_read_clears_password_preserves_editor_and_resets_sending_consent',
    );
    await panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }).check();
    const generated = page.waitForResponse(
      (r) => new URL(r.url()).pathname === aiPath && r.request().method() === 'POST',
      { timeout: 200000 },
    );
    await panel.getByRole('button', { name: '生成 SQL 草稿', exact: true }).click();
    const generation = await generated;
    requestIds.push(generation.headers()['x-request-id'] || '');
    await writeFile(
      path.join(protectedPath, 'sql-assistant-generation.private.json'),
      JSON.stringify(await generation.json()),
      { mode: 0o600 },
    );
    expect(generation.status()).toBe(200);
    await expect(
      panel
        .getByLabel('AI SQL 草稿', { exact: true })
        .or(panel.getByTestId('sql-assistant-error'))
        .first(),
    ).toBeVisible({ timeout: 200000 });
    await expect(panel.getByTestId('sql-assistant-error')).toHaveCount(0);
    checkpoint.draft = await panel.getByLabel('AI SQL 草稿', { exact: true }).inputValue();
    expect(checkpoint.draft.trim()).toMatch(
      /^SELECT\s+42(?:::\s*(?:integer|int))?(?:\s+AS\s+[a-z_][a-z0-9_]*)?\s*;?$/i,
    );
    expect(queries).toBe(1);
    expect(inferences).toBe(1);
    await save('actual_local_model_returns_bounded_draft_without_database_execution');
    await panel.getByRole('button', { name: '载入 SQL 编辑器', exact: true }).click();
    await expect(page.getByLabel('SQL 查询', { exact: true })).toHaveValue(checkpoint.draft);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    expect(queries).toBe(1);
    await save('explicit_UI_load_changes_editor_clears_password_and_does_not_execute_SQL');
    const execution = await sql('query');
    expect(execution.rows).toEqual([[42]]);
    expect(queries).toBe(2);
    await save('manual_UI_submit_runs_original_AI_draft_through_real_Neon_Proxy');
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill('SELECT generate_series(1,3) AS assistant_plan_private_literal');
    await sql('explain');
    expect(queries).toBe(3);
    await panel.getByLabel('AI Schema 上下文', { exact: true }).fill('');
    await panel
      .getByRole('combobox', { name: 'AI 建议类型', exact: true })
      .selectOption('plan_advice');
    await panel
      .getByLabel('AI 请求说明', { exact: true })
      .fill(
        'Give a brief observation and one manual verification step. Identify what is not measured; do not claim any change was tested.',
      );
    const planPayload = await preview.inputValue();
    expect(JSON.parse(JSON.parse(planPayload).messages[1].content).observed_plan.analyzed).toBe(
      false,
    );
    expect(planPayload).not.toContain('assistant_plan_private_literal');
    expect(planPayload).not.toContain(password);
    expect(planPayload).not.toContain('Actual Total Time');
    await panel.getByRole('checkbox', { name: '确认发送模型上下文', exact: true }).check();
    const advising = page.waitForResponse(
      (r) => new URL(r.url()).pathname === aiPath && r.request().method() === 'POST',
      { timeout: 200000 },
    );
    await panel.getByRole('button', { name: '获取计划建议', exact: true }).click();
    const advice = await advising;
    requestIds.push(advice.headers()['x-request-id'] || '');
    await writeFile(
      path.join(protectedPath, 'sql-assistant-advice.private.json'),
      JSON.stringify(await advice.json()),
      { mode: 0o600 },
    );
    expect(advice.status()).toBe(200);
    await expect(panel.getByTestId('sql-assistant-plan-advice')).toBeVisible({ timeout: 200000 });
    checkpoint.advice = await panel.getByTestId('sql-assistant-plan-advice').innerText();
    expect(checkpoint.advice.trim().length).toBeGreaterThan(0);
    expect(queries).toBe(3);
    expect(inferences).toBe(2);
    await save('actual_local_model_explains_bounded_numeric_plan_without_query_literal_or_new_SQL');
    await page.screenshot({
      path: info.outputPath('sql-assistant-plan-advice.png'),
      fullPage: true,
    });
    await page.getByLabel('目标数据库', { exact: true }).fill('postgres');
    await expect(panel.getByTestId('sql-assistant-plan-advice')).toHaveCount(0);
    await expect(panel.getByLabel('确认发送模型上下文', { exact: true })).not.toBeChecked();
    expect(queries).toBe(3);
    await page.reload();
    await expect(panel.getByTestId('sql-assistant-plan-advice')).toHaveCount(0);
    await expect(panel.getByLabel('AI SQL 草稿', { exact: true })).toHaveCount(0);
    await save('connection_scope_change_and_reload_discard_model_context_and_results');
    await page.screenshot({ path: info.outputPath('sql-assistant.png'), fullPage: true });
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('test_compute_zero_original_business_data_and_model_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
