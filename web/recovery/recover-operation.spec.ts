import { expect, test } from '@playwright/test';
import { readFile, writeFile, mkdir } from 'node:fs/promises';

type Fixture = {
  project_id: string;
  operation_id: string;
  writer_id: string;
  child_endpoint_id: string;
  database_password_file: string;
  // Opt-in recovery of a known failed lifecycle reader. Keep the original
  // writable-branch recovery contract unchanged when this field is absent.
  purpose?: 'lifecycle-reader';
  previous_attempts?: number;
};

test('UI recovers the existing operation after infrastructure failure', async ({
  page,
  context,
  baseURL,
}, info) => {
  const source = process.env.NEON_E2E_RECOVERY_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!source || !adminFile || !baseURL)
    throw new Error('Explicit recovery fixture and trusted login required');
  const fixture: Fixture = JSON.parse(await readFile(source, 'utf8'));
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^op_[a-f0-9]+$/.test(fixture.operation_id) ||
    ![fixture.writer_id, fixture.child_endpoint_id].every((id) => /^ep_[a-f0-9]{16}$/.test(id)) ||
    fixture.writer_id === fixture.child_endpoint_id ||
    (fixture.purpose !== undefined && fixture.purpose !== 'lifecycle-reader')
  )
    throw new Error('Invalid explicit recovery identity');
  if (
    fixture.purpose === 'lifecycle-reader' &&
    (!Number.isSafeInteger(fixture.previous_attempts) ||
      fixture.previous_attempts! < 1 ||
      fixture.previous_attempts! > 10)
  )
    throw new Error('Reader recovery requires the observed failed attempt count');
  const password = (await readFile(fixture.database_password_file, 'utf8')).trim();
  const adminPassword = (await readFile(adminFile, 'utf8')).trim();
  const checks: string[] = [];
  let result = 'running';
  await mkdir(info.outputDir, { recursive: true });
  async function save() {
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          checks,
          project_id: fixture.project_id,
          operation_id: fixture.operation_id,
          purpose: fixture.purpose || 'writable-branch',
          credentials_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  async function shot(name: string) {
    await page.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  async function sql(endpoint: string, statement: string) {
    await page.goto('/#/projects/' + fixture.project_id + '/query');
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const response = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + endpoint + '/query') && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const completed = await response;
    expect(completed.status()).toBe(200);
    return completed.json() as Promise<{ rows: unknown[][] }>;
  }
  async function suspend(endpoint: string) {
    await page.goto('/#/projects/' + fixture.project_id + '/compute');
    await page.locator('select').first().selectOption(endpoint);
    const response = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + endpoint + '/suspend') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    expect((await response).status()).toBe(202);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145_000,
    });
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill(adminPassword);
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const original = await context.request.get(
      baseURL + '/api/v1/projects/' + fixture.project_id + '/operations/' + fixture.operation_id,
    );
    expect(original.status()).toBe(200);
    const failed = await original.json();
    expect(failed.state).toBe('failed');
    expect(failed.retryable).toBe(true);
    expect(failed.resource_id).toBe(fixture.child_endpoint_id);
    expect(failed.action).toBe('create_endpoint');
    if (fixture.purpose === 'lifecycle-reader') {
      expect(failed.attempts).toBe(fixture.previous_attempts);
      const response = await context.request.get(
        baseURL + '/api/v1/projects/' + fixture.project_id + '/endpoints',
      );
      expect(response.status()).toBe(200);
      const endpoints = (await response.json()).items;
      const reader = endpoints.find((e: { id: string }) => e.id === fixture.child_endpoint_id);
      const writer = endpoints.find((e: { id: string }) => e.id === fixture.writer_id);
      expect(reader.endpoint_type).toBe('read_only');
      expect(writer.endpoint_type).toBe('read_write');
      expect(reader.branch_id).toBe(writer.branch_id);
      checks.push('explicit_failed_reader_and_writer_identity_verified_before_retry');
      await save();
    }
    await page.goto('/#/projects/' + fixture.project_id + '/operations');
    await page
      .getByRole('button', { name: '查看操作 ' + fixture.operation_id + ' 的步骤', exact: true })
      .click();
    const dialog = page.getByRole('dialog', { name: '操作详情' });
    await expect(dialog.getByRole('button', { name: '重试原操作', exact: true })).toBeVisible();
    await shot('failed-operation');
    const accepted = page.waitForResponse(
      (r) =>
        r.url().endsWith('/operations/' + fixture.operation_id + '/retry') &&
        r.request().method() === 'POST',
    );
    await dialog.getByRole('button', { name: '重试原操作', exact: true }).click();
    expect((await accepted).status()).toBe(202);
    await expect(dialog.locator('.detail-row').last().locator('.status')).toContainText('已完成', {
      timeout: 490_000,
    });
    const operation = await context.request.get(
      baseURL + '/api/v1/projects/' + fixture.project_id + '/operations/' + fixture.operation_id,
    );
    expect(operation.status()).toBe(200);
    const recovered = await operation.json();
    expect(recovered.id).toBe(fixture.operation_id);
    expect(recovered.resource_id).toBe(fixture.child_endpoint_id);
    expect(recovered.attempts).toBe((fixture.previous_attempts ?? 1) + 1);
    checks.push('same_operation_and_endpoint_recovered_from_ui');
    await save();
    await shot('operation-recovered');

    if (fixture.purpose === 'lifecycle-reader') {
      expect(
        (await sql(fixture.child_endpoint_id, 'SELECT id, marker FROM public.lifecycle_probe'))
          .rows,
      ).toEqual([[1, 'retained-parent']]);
      expect(
        (
          await sql(
            fixture.child_endpoint_id,
            "SELECT pg_is_in_recovery(),current_setting('transaction_read_only')",
          )
        ).rows,
      ).toEqual([[true, 'on']]);
      checks.push('recovered_reader_inherits_retained_data_and_is_read_only');
      await save();
      await suspend(fixture.child_endpoint_id);
      expect(
        (await sql(fixture.writer_id, 'SELECT id, marker FROM public.lifecycle_probe')).rows,
      ).toEqual([[1, 'retained-parent']]);
      await suspend(fixture.writer_id);
      checks.push('writer_cold_wake_retains_data_and_both_computes_return_to_zero');
      await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
      await expect(
        page.getByRole('heading', { name: '监控与运行洞察', exact: true }),
      ).toBeVisible();
      await shot('monitoring-after-recovery');
      checks.push('monitoring_after_recovery');
      result = 'pass';
      return;
    }

    expect(
      (await sql(fixture.child_endpoint_id, 'SELECT id, marker FROM public.ui_release_probe')).rows,
    ).toEqual([[1, 'parent']]);
    await sql(fixture.child_endpoint_id, "INSERT INTO public.ui_release_probe VALUES(2, 'child')");
    checks.push('recovered_child_inherits_parent_and_accepts_writes');
    await save();
    await shot('child-data');
    await suspend(fixture.child_endpoint_id);
    expect(
      (await sql(fixture.writer_id, 'SELECT id, marker FROM public.ui_release_probe ORDER BY id'))
        .rows,
    ).toEqual([[1, 'parent']]);
    checks.push('parent_cold_resume_preserves_branch_isolation');
    await suspend(fixture.writer_id);
    checks.push('both_computes_suspended_via_ui');
    await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
    await expect(page.getByRole('heading', { name: '监控与运行洞察', exact: true })).toBeVisible();
    await shot('monitoring-after-recovery');
    checks.push('monitoring_after_recovery');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    await shot('failure');
    throw error;
  } finally {
    await save();
  }
});
