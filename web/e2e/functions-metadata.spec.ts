import { expect, test } from '@playwright/test';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

// The live read-only slice uses real Console/API/PG, never browser mocks or
// manually inserted fake live functions. Execution admission remains false.
test('Functions: authorized Linux UI metadata and disabled execution boundary', async ({
  page,
  context,
  baseURL,
}, info) => {
  const passwordFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const project = process.env.NEON_E2E_PROJECT_ID || process.env.NEON_E2E_CREDENTIAL_PROJECT;
  if (!baseURL || !passwordFile || !project)
    throw new Error('Protected Linux read-only E2E configuration required');
  const checks: string[] = [];
  let result = 'running';
  async function save() {
    await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          checks,
          sql_tested: false,
          deployment_tested: false,
          invocation_tested: false,
          production_qualified: false,
          credentials_in_evidence: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  async function record(name: string) {
    checks.push(name);
    await save();
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page
      .getByLabel('密码', { exact: true })
      .fill((await readFile(passwordFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await record('actual_console_login');
    await page.goto(`/#/projects/${project}`);
    await page.getByRole('link', { name: /Functions/ }).click();
    await expect(page.getByTestId('functions-page')).toBeVisible();
    await record('actual_functions_navigation');
    await expect(page.getByRole('status')).toContainText('尚未启用函数部署与调用');
    const capabilities = await context.request.get('/api/v1/capabilities');
    expect(capabilities.status()).toBe(200);
    expect((await capabilities.json()).services.functions.enabled).toBe(false);
    await record('execution_capability_not_falsely_enabled');
    const branch = await page.getByLabel('Functions 分支').inputValue();
    const apiPath = `/api/v1/projects/${project}/branches/${branch}/functions`;
    const listed = await context.request.get(apiPath);
    expect(listed.status()).toBe(200);
    const data = await listed.json();
    expect(data.driver_enabled).toBe(false);
    expect(Array.isArray(data.items)).toBe(true);
    for (const item of data.items) {
      expect(item.invocation_url).toBeNull();
      expect(item.runtime_observed).toBe(false);
    }
    if (!data.items.length)
      await expect(page.getByText('这个分支还没有函数', { exact: true })).toBeVisible();
    await expect(page.locator('[role=alert]')).toHaveCount(0);
    await record('real_pg_metadata_without_fake_runtime_url');
    const refresh = page.waitForResponse(
      (r) => r.request().method() === 'GET' && r.url().includes(apiPath),
    );
    await page.getByRole('button', { name: '刷新函数状态', exact: true }).click();
    expect((await refresh).status()).toBe(200);
    await record('ui_refresh_real_metadata');
    expect((await context.request.get(apiPath + '?limit=101')).status()).toBe(422);
    expect((await context.request.get(apiPath + '?after=with-hyphen')).status()).toBe(422);
    await record('bounded_pagination_rejected');
    expect((await context.request.get(apiPath + '/unavailable')).status()).toBe(404);
    expect((await context.request.get(apiPath + '/unavailable/deployments')).status()).toBe(404);
    await record('unknown_function_no_fabricated_history');
    const branches = await page
      .getByLabel('Functions 分支')
      .locator('option')
      .evaluateAll((options) => options.map((o) => (o as HTMLOptionElement).value));
    if (branches.length > 1) {
      const other = branches.find((id) => id !== branch)!;
      const switched = page.waitForResponse(
        (r) => r.url().includes(`/branches/${other}/functions`) && r.request().method() === 'GET',
      );
      await page.getByLabel('Functions 分支').selectOption(other);
      expect((await switched).status()).toBe(200);
      await expect(page.locator('[role=alert]')).toHaveCount(0);
      await record('ui_branch_change_clears_previous_scope');
    }
    await page.screenshot({ path: info.outputPath('functions.png') });
    result = 'pass';
  } catch (e) {
    result = 'fail';
    throw e;
  } finally {
    await save();
  }
});
