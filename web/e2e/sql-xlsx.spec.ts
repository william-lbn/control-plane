import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { unzipSync, strFromU8 } from 'fflate';

test('UI SQL result downloads real XLSX with exact text, no formulas and no additional query', async ({
  page,
}, info) => {
  test.setTimeout(600000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !adminFile)
    throw new Error('Protected operator and original SQL fixture required');
  const owner = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const checks: string[] = [];
  let result = 'running',
    queries = 0;
  const endpoint = owner.writer_id,
    project = owner.project_id;
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
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill(
        `SELECT '=1+2'::text AS same, '9223372036854775807'::text AS same, NULL::text AS empty, '中文😀<&>'::text AS unicode`,
      );
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(owner.password_file, 'utf8')).trim());
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query`,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(200);
    const data = await response.json();
    expect(data.columns).toEqual(['same', 'same', 'empty', 'unicode']);
    expect(data.rows).toEqual([['=1+2', '9223372036854775807', null, '中文😀<&>']]);
    await save('ui_real_sql_result_has_duplicate_headers_precision_null_and_unicode');
    const downloadPending = page.waitForEvent('download');
    await page.getByRole('button', { name: '下载 XLSX', exact: true }).click();
    const download = await downloadPending;
    expect(download.suggestedFilename()).toBe('query-result.xlsx');
    const file = await download.path();
    expect(file).toBeTruthy();
    expect(await download.failure()).toBeNull();
    const bytes = await readFile(file!);
    await writeFile(info.outputPath('query-result.xlsx'), bytes);
    const entries = unzipSync(bytes);
    const sheet = strFromU8(entries['xl/worksheets/sheet1.xml']),
      meta = strFromU8(entries['xl/worksheets/sheet2.xml']);
    expect(sheet).toContain('=1+2');
    expect(sheet).toContain('9223372036854775807');
    expect(sheet).toContain('中文😀&lt;&amp;&gt;');
    expect(sheet).toContain('<c r="C2"/>');
    expect(sheet).not.toContain('<f>');
    expect((sheet.match(/>same<\/t>/g) || []).length).toBe(2);
    expect(meta).toContain('neon-console-xlsx-v1');
    expect(meta).toContain('Result truncated');
    // Use the browser's independent XML parser, not just the ZIP library used
    // by the exporter, to reject malformed SpreadsheetML/relationships.
    for (const [name, value] of Object.entries(entries)) {
      const xml = strFromU8(value);
      expect(
        await page.evaluate((x) => {
          const doc = new DOMParser().parseFromString(x, 'application/xml');
          return doc.querySelector('parsererror') === null;
        }, xml),
        name,
      ).toBe(true);
      expect(xml).not.toContain('TargetMode="External"');
    }
    expect(queries).toBe(1);
    await save('ui_downloaded_xlsx_zip_and_all_xml_valid_no_formula_precision_loss_or_second_sql');
    const license = await page.request.get('/licenses/fflate-0.8.3-MIT.txt');
    expect(license.status()).toBe(200);
    expect(await license.text()).toContain('Permission is hereby granted');
    await save('deployed_web_retains_public_dependency_license');
    await page.screenshot({ path: info.outputPath('sql-xlsx.png'), fullPage: true });
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByLabel('选择 Endpoint').selectOption(endpoint);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_compute_stopped_original_data_and_workbook_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
