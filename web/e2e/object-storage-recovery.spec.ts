import { expect, test } from '@playwright/test';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Explicit continuation of a retained failure. Never rewrite the original
// attempt, create another project or purge any object/timeline/credential.
test('Object Storage retained fixture: fixed edge headers and UI lifecycle recovery', async ({
  page,
  context,
  baseURL,
}, info) => {
  const input = process.env.NEON_E2E_STORAGE_RECOVERY_FIXTURE;
  const admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!input || !admin || !baseURL)
    throw new Error('Explicit protected Linux recovery inputs required');
  const fixture = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    project_name: string;
    branch_id: string;
    child_id: string;
    child_name: string;
    writer_id: string;
    child_writer_id: string;
    original_failure_job: string;
  };
  if (
    !/^ci-publication-ui-20261009\d+-storage$/.test(fixture.project_name) ||
    !fixture.original_failure_job.startsWith('publication-ui-20261009') ||
    !fixture.project_id.startsWith('prj_') ||
    !fixture.branch_id.startsWith('br_') ||
    !fixture.child_id.startsWith('br_') ||
    fixture.branch_id === fixture.child_id
  ) {
    throw new Error('Invalid original fixture identity');
  }
  const project = fixture.project_id;
  const branches = [fixture.branch_id, fixture.child_id];
  const endpoints = [fixture.writer_id, fixture.child_writer_id];
  const prefix = `/api/v1/projects/${project}`;
  const key = new URLSearchParams({ key: 'documents/report.txt' });
  const parentBytes = Buffer.from('Immutable parent content · 初始文件\n');
  const childBytes = Buffer.from('Independent child content · 子分支文件\n');
  const checks: string[] = [];
  let result = 'running';
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  const save = () =>
    writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          branches,
          endpoints,
          checks,
          original_failure_job: fixture.original_failure_job,
          original_attempt_unchanged: true,
          data_retained: true,
          physical_gc: false,
          production_qualified: false,
        },
        null,
        2,
      ) + '\n',
    );
  const record = async (name: string) => {
    checks.push(name);
    await save();
  };
  const service = (branch: string) => `${prefix}/branches/${branch}/storage`;
  async function storage(branch: string, enabled: boolean) {
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByLabel('存储分支').selectOption(branch);
    const button = page.getByRole('button', {
      name: enabled ? '启用对象存储' : '禁用对象存储',
      exact: true,
    });
    await expect(button).toBeEnabled({ timeout: 490000 });
    const pending = page.waitForResponse(
      (r) =>
        r.url().endsWith(service(branch)) && r.request().method() === (enabled ? 'POST' : 'DELETE'),
    );
    await button.click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const op = (await response.json()).operation;
    await expect(page.getByTestId('storage-operation')).toContainText(op.id);
    await expect(page.getByTestId('storage-operation')).toContainText(
      /succeeded|failed|cancelled/,
      { timeout: 490000 },
    );
    await expect(page.getByTestId('storage-operation')).toContainText('succeeded');
  }
  async function files() {
    const parent = await context.request.get(
      `${service(branches[0])}/buckets/uploads/objects?${key}`,
    );
    expect(parent.status()).toBe(200);
    expect(await parent.body()).toEqual(parentBytes);
    const child = await context.request.get(`/storage/v1/${branches[1]}/assets?${key}`);
    expect(child.status()).toBe(200);
    expect(await child.body()).toEqual(childBytes);
    expect(child.headers()['x-content-type-options']).toBe('nosniff');
    expect(child.headers()['x-frame-options']).toBe('DENY');
    expect(child.headers()['referrer-policy']).toBe('no-referrer');
    expect(child.headers()['content-security-policy']).toContain('sandbox');
    expect(child.headers()['content-disposition']).toContain('attachment');
  }
  async function suspend(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
  }
  async function lifecycle(button: string, name: string, branch = '') {
    const row = page.getByTestId(branch ? 'lifecycle-' + branch : 'project-lifecycle');
    await row.getByRole('button', { name: button, exact: true }).click();
    await page.getByLabel('资源名称确认').fill(name);
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    await expect(page.getByRole('dialog', { name: '确认生命周期操作' })).toHaveCount(0, {
      timeout: 490000,
    });
  }
  await save();
  try {
    await page.goto('/');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const before = await (await context.request.get(prefix + '/lifecycle')).json();
    expect(before.project.name).toBe(fixture.project_name);
    expect(before.project.state).toBe('ready');
    expect(before.branches.map((b: { id: string }) => b.id).sort()).toEqual([...branches].sort());
    for (const branch of branches) {
      expect((await (await context.request.get(service(branch))).json()).state).toBe('disabled');
      await storage(branch, true);
    }
    await files();
    await record('original_parent_child_bytes_retained_and_all_security_headers_valid');
    for (const branch of branches) await storage(branch, false);
    for (const endpoint of endpoints) await suspend(endpoint);
    await page.goto(`/#/projects/${project}/lifecycle`);
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
    if (before.project.protected) await lifecycle('解除项目保护', fixture.project_name);
    for (const [branch, name] of [
      [branches[0], 'main'],
      [branches[1], fixture.child_name],
    ]) {
      if (
        await page
          .getByTestId('lifecycle-' + branch)
          .getByRole('button', { name: '解除分支保护', exact: true })
          .count()
      )
        await lifecycle('解除分支保护', name, branch);
    }
    await lifecycle('删除项目', fixture.project_name);
    expect((await context.request.get(`/storage/v1/${branches[1]}/assets?${key}`)).status()).toBe(
      404,
    );
    await record('ui_retained_delete_closed_original_public_file_admission');
    await lifecycle('恢复项目', fixture.project_name);
    for (const branch of branches) {
      expect((await (await context.request.get(service(branch))).json()).state).toBe('disabled');
      await storage(branch, true);
    }
    await files();
    await record(
      'ui_recovered_original_project_and_explicit_reenable_preserved_both_file_versions',
    );
    await page.screenshot({ path: info.outputPath('storage-recovery.png'), fullPage: true });
    for (const branch of branches) await storage(branch, false);
    for (const endpoint of endpoints) await suspend(endpoint);
    await page.goto(`/#/projects/${project}/lifecycle`);
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
    await lifecycle('删除项目', fixture.project_name);
    const retained = await (await context.request.get(prefix + '/lifecycle')).json();
    expect(retained.project.state).toBe('deleted');
    expect(retained.physical_gc_enabled).toBe(false);
    expect(
      retained.tombstones.some(
        (v: { physical_gc_state: string }) => v.physical_gc_state === 'held',
      ),
    ).toBe(true);
    await record('ui_retained_tombstone_and_released_original_fixture_quota');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    throw error;
  } finally {
    await save();
  }
});
