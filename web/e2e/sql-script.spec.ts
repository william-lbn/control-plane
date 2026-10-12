import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test.use({ actionTimeout: 30_000 });
test('SQL scripts: real parsing, result sets, transaction outcomes and bounded display', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!input || !admin) throw new Error('Protected Linux SQL owner fixture required');
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
    throw new Error('Exact accepted owner scope required');
  const { project_id: project, writer_id: writer } = fixture,
    password = (await readFile(fixture.password_file, 'utf8')).trim();
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
        production_qualified: false,
        credentials_in_evidence: false,
      }) + '\n',
    );
  }
  async function execute(sql: string, script: boolean) {
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(writer);
    await page.getByLabel('数据库角色', { exact: true }).fill(fixture.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(fixture.database);
    await page.getByLabel('执行模式').selectOption(script ? 'script' : 'single');
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
      .getByRole('button', { name: script ? '▶ 执行脚本' : '▶ 执行 SQL', exact: true })
      .click();
    const response = await pending;
    expect(response.status()).toBe(200);
    return response.json();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    expect(
      (await execute("SELECT to_regclass('public.script_ui_probe')::text", false)).rows,
    ).toEqual([[null]]);
    const created = await execute(
      "CREATE TABLE public.script_ui_probe(id integer PRIMARY KEY,marker text); INSERT INTO public.script_ui_probe VALUES(1,'semi;literal'); DO $$BEGIN PERFORM 1; END$$; SELECT id,marker FROM public.script_ui_probe",
      true,
    );
    expect(created.outcome).toBe('completed');
    expect(created.statements_completed).toBe(4);
    expect(created.results[3].rows).toEqual([[1, 'semi;literal']]);
    await expect(page.getByTestId('sql-statement-4')).toContainText('semi;literal');
    await save('ui_native_script_parsing_actual_write_and_four_results');
    const failed = await execute(
      "INSERT INTO public.script_ui_probe VALUES(2,'must-rollback'); SELECT missing_column FROM public.script_ui_probe",
      true,
    );
    expect(failed.outcome).toBe('failed');
    expect(failed.error.sqlstate).toBe('42703');
    await expect(page.getByTestId('sql-script-results')).toContainText('未全部完成');
    expect(
      (await execute('SELECT count(*)::integer FROM public.script_ui_probe WHERE id=2', false))
        .rows,
    ).toEqual([[0]]);
    await save('actual_implicit_transaction_error_does_not_claim_success_or_replay');
    const partial = await execute(
      "BEGIN; INSERT INTO public.script_ui_probe VALUES(3,'explicit-commit'); COMMIT; SELECT missing_column FROM public.script_ui_probe",
      true,
    );
    expect(partial.outcome).toBe('failed');
    expect(partial.open_transaction_discarded).toBe(false);
    expect(
      (await execute('SELECT marker FROM public.script_ui_probe WHERE id=3', false)).rows,
    ).toEqual([['explicit-commit']]);
    await save('prior_explicit_commit_remains_visible_after_later_error');
    const open = await execute(
      "BEGIN; INSERT INTO public.script_ui_probe VALUES(99,'discard-open')",
      true,
    );
    expect(open.open_transaction_discarded).toBe(true);
    expect(open.error.code).toBe('sql_script_transaction_open');
    await expect(page.getByTestId('sql-script-results')).toContainText('未关闭事务');
    expect(
      (await execute('SELECT count(*)::integer FROM public.script_ui_probe WHERE id=99', false))
        .rows,
    ).toEqual([[0]]);
    await save('ui_unclosed_transaction_reported_and_actual_connection_rollback_verified');
    const bounded = await execute(
      "SELECT generate_series(1,205); SELECT 'NaN'::float8,9223372036854775807::bigint",
      true,
    );
    expect(bounded.results[0].rows).toHaveLength(200);
    expect(bounded.results_truncated).toBe(true);
    expect(bounded.statements_completed).toBe(2);
    const exact = await execute("SELECT 'NaN'::float8,9223372036854775807::bigint", true);
    expect(exact.results[0].rows).toEqual([['NaN', '9223372036854775807']]);
    await expect(page.getByTestId('sql-statement-1')).toContainText('9223372036854775807');
    await save('bounded_display_and_exact_bigint_nonfinite_projection');
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/suspend` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    const response: Response = await pending;
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
    await save('script_records_and_data_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
