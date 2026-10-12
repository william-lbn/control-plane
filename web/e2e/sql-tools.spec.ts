import { expect, test, type Download } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test.use({ actionTimeout: 30_000 });
test('SQL plans and downloads use real PostgreSQL, preserve values and disclose Analyze writes', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!input || !admin) throw new Error('Protected accepted SQL fixture required');
  const fixture = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    role: string;
    password_file: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(fixture.writer_id) ||
    fixture.database !== 'owner_transfer_v1' ||
    fixture.role !== 'owner_new_v1'
  )
    throw new Error('Exact accepted SQL scope required');
  const password = (await readFile(fixture.password_file, 'utf8')).trim(),
    project = fixture.project_id,
    writer = fixture.writer_id;
  const checks: string[] = [];
  let result = 'running';
  async function save(name?: string) {
    if (name) checks.push(name);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        writer_id: writer,
        database: fixture.database,
        role: fixture.role,
        checks,
        credentials_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function execute(
    sql: string,
    action: 'query' | 'explain' | 'analyze' = 'query',
    script = false,
  ) {
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(writer);
    await page.getByLabel('数据库角色', { exact: true }).fill(fixture.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(fixture.database);
    await page.getByLabel('执行模式').selectOption(script ? 'script' : 'single');
    if (!script) await page.getByLabel('查询操作').selectOption(action);
    if (action === 'analyze') await expect(page.getByRole('alert')).toContainText('会实际执行');
    await page.getByLabel('SQL 查询').fill(sql);
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname ===
          `/api/v1/projects/${project}/endpoints/${writer}/${script ? 'query-script' : 'query'}` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page
      .getByRole('button', {
        name: script
          ? '▶ 执行脚本'
          : action === 'explain'
            ? '▶ Explain'
            : action === 'analyze'
              ? '▶ Analyze'
              : '▶ 执行 SQL',
        exact: true,
      })
      .click();
    const response = await pending;
    expect(response.status()).toBe(200);
    await expect(page.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    return response.json();
  }
  async function content(download: Download) {
    const path = await download.path();
    if (!path) throw new Error('Linux download path required');
    expect(await download.failure()).toBeNull();
    return readFile(path, 'utf8');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    expect((await execute('SELECT id FROM public.script_ui_probe ORDER BY id')).rows).toEqual([
      [1],
      [3],
    ]);
    const explained = await execute(
      "INSERT INTO public.script_ui_probe VALUES(4,'analyze-write')",
      'explain',
    );
    expect(explained.rows[0][0][0].Plan.Operation).toBe('Insert');
    await expect(page.locator('.result-panel pre')).toContainText('ModifyTable');
    expect(
      (await execute('SELECT count(*)::integer FROM public.script_ui_probe WHERE id=4')).rows,
    ).toEqual([[0]]);
    await save('ui_explain_returns_real_plan_without_executing_insert');
    const analyzed = await execute(
      "INSERT INTO public.script_ui_probe VALUES(4,'analyze-write')",
      'analyze',
    );
    expect(analyzed.rows[0][0][0].Plan.Operation).toBe('Insert');
    expect(typeof analyzed.rows[0][0][0]['Execution Time']).toBe('number');
    expect((await execute('SELECT marker FROM public.script_ui_probe WHERE id=4')).rows).toEqual([
      ['analyze-write'],
    ]);
    await save('ui_analyze_explicit_warning_real_execution_and_retained_write');
    const data = await execute(
      `SELECT '=1+2'::text AS same, 'a,"b"'::text AS same, NULL::text AS empty`,
    );
    const jsonPending = page.waitForEvent('download');
    await page.getByRole('button', { name: '下载 JSON', exact: true }).click();
    const json = await jsonPending;
    expect(json.suggestedFilename()).toBe('query-result.json');
    expect(JSON.parse(await content(json))).toEqual({ format: 'neon-console-result-v1', ...data });
    const csvPending = page.waitForEvent('download');
    await page.getByRole('button', { name: '下载 CSV', exact: true }).click();
    const csv = await csvPending;
    expect(csv.suggestedFilename()).toBe('query-result.csv');
    expect((await content(csv)).replace(/^\ufeff/, '')).toBe(
      '"same","same","empty"\r\n"\'=1+2","a,""b""",""\r\n',
    );
    await save('real_browser_csv_json_download_duplicate_columns_nulls_formula_protection');
    const bounded = await execute('SELECT generate_series(1,205)', 'query', true);
    expect(bounded.results[0].truncated).toBe(true);
    const statement = page.getByTestId('sql-statement-1');
    await expect(statement).toContainText('仅导出已显示的截断结果');
    const boundedPending = page.waitForEvent('download');
    await statement.getByRole('button', { name: '下载 JSON', exact: true }).click();
    const exported = JSON.parse(await content(await boundedPending));
    expect(exported.rows).toHaveLength(200);
    expect(exported.truncated).toBe(true);
    await save('script_result_download_discloses_real_display_truncation');
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/suspend` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const op = (await response.json()).operation;
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${project}/operations/${op.id}`);
          expect(r.status()).toBe(200);
          return (await r.json()).state;
        },
        { timeout: 150000, intervals: [2500] },
      )
      .toBe('succeeded');
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠');
    await save('original_records_and_analyze_write_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
