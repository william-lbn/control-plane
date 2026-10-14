import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI object directories use real grouped S3 SDK pagination and retained bytes', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !adminFile || !privateDir || !probe)
    throw new Error('Protected Linux owner/admin/SDK inputs required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    database: string;
    role: string;
  };
  if (fixture.database !== 'owner_transfer_v1' || fixture.role !== 'owner_new_v1')
    throw new Error('Retained owner fixture required');
  const project = fixture.project_id,
    branch = fixture.branch_id,
    base = `/api/v1/projects/${project}/branches/${branch}`,
    bucket = process.env.NEON_E2E_DIRECTORY_ORIGINAL_INPUT_FILE
      ? (
          JSON.parse(
            await readFile(process.env.NEON_E2E_DIRECTORY_ORIGINAL_INPUT_FILE, 'utf8'),
          ) as { bucket: string }
        ).bucket
      : 'ci-s3-' + Date.now();
  const checks: string[] = [],
    operations: string[] = [],
    credentials: string[] = [];
  let result = 'running';
  const keys = ['directory/root.txt', 'directory/nested/a.txt', 'directory/nested/中文.txt'];
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        credential_ids: credentials,
        bucket,
        project_id: project,
        branch_id: branch,
        production_qualified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
  }
  async function operation(response: Response) {
    expect(response.status()).toBe(202);
    const body = await response.json();
    operations.push(body.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${body.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error('Original storage operation failed ' + op.id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  try {
    await save();
    await writeFile(
      path.join(privateDir, 'directory-original-case.json'),
      JSON.stringify({ bucket, keys }),
      { mode: 0o600, flag: 'wx' },
    );
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/storage`);
    const current = await context.request.get(base + '/storage');
    expect(current.status()).toBe(200);
    const state = await current.json();
    expect(state.state).toBe('disabled');
    if (state.spec) {
      expect(state.spec.database).toBe(fixture.database);
      await expect(page.getByLabel('存储数据库', { exact: true })).toHaveValue(fixture.database);
    } else {
      await page.getByLabel('存储数据库', { exact: true }).fill(fixture.database);
    }
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用对象存储', exact: true }).click();
    await operation(await pending);
    await page.getByLabel('存储桶名称').fill(bucket);
    const created = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/storage/buckets' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
    expect((await created).status()).toBe(201);
    for (const key of keys) {
      await page.getByLabel('对象键', { exact: true }).fill(key);
      await page.getByLabel('上传文件', { exact: true }).setInputFiles({
        name: key.split('/').at(-1)!,
        mimeType: 'text/plain',
        buffer: Buffer.from('retained directory bytes: ' + key),
      });
      const put = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/objects` &&
          r.request().method() === 'PUT',
      );
      await page.getByRole('button', { name: '上传对象', exact: true }).click();
      expect((await put).status()).toBe(200);
    }
    await save('ui_creates_private_bucket_and_three_retained_nested_objects');
    await page.getByLabel('对象前缀', { exact: true }).fill('directory/');
    await page.getByLabel('按目录浏览', { exact: true }).check();
    const list = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/objects` &&
        new URL(r.url()).searchParams.get('delimiter') === '/',
    );
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    const listed = await list;
    expect(listed.status()).toBe(200);
    const directory = await listed.json();
    expect(directory.common_prefixes).toEqual(['directory/nested/']);
    expect(directory.items.map((x: { key: string }) => x.key)).toEqual(['directory/root.txt']);
    await expect(page.getByRole('navigation', { name: '对象子目录' })).toBeVisible();
    await save('actual_console_grouping_returns_one_prefix_and_one_root_object');
    await page.getByRole('button', { name: '📁 directory/nested/', exact: true }).click();
    await expect(page.getByLabel('对象前缀', { exact: true })).toHaveValue('directory/nested/');
    await expect(
      page.getByRole('cell', { name: 'directory/nested/中文.txt', exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole('cell', { name: 'directory/nested/a.txt', exact: true }),
    ).toBeVisible();
    await save('ui_folder_navigation_preserves_unicode_object_keys');
    await page.getByRole('button', { name: '上级目录', exact: true }).click();
    await expect(page.getByLabel('对象前缀', { exact: true })).toHaveValue('directory/');
    await expect(page.getByRole('cell', { name: 'directory/root.txt', exact: true })).toBeVisible();
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据能力').selectOption('storage:read');
    await page.getByLabel('凭据名称').fill('Directory read-only ' + bucket);
    const issue = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const issued = await issue;
    expect(issued.status()).toBe(201);
    const credential = await issued.json();
    credentials.push(credential.token_id);
    const config = path.join(privateDir, 'directory-sdk.private.json');
    await writeFile(
      config,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: credential.token_id,
        secret_key: credential.s3_secret_access_key,
        bucket,
        mode: 'directory',
      }),
      { mode: 0o600, flag: 'wx' },
    );
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    await save('ui_issues_protected_read_only_s3_credential');
    const run = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
    const sdk = JSON.parse(run.stdout);
    expect(sdk.result).toBe('pass');
    for (const check of sdk.checks) await save(check);
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
    await page.getByLabel('对象前缀', { exact: true }).fill('directory/');
    await page.getByLabel('按目录浏览', { exact: true }).check();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    await expect(
      page.getByRole('button', { name: '📁 directory/nested/', exact: true }),
    ).toBeVisible();
    await page.screenshot({ path: info.outputPath('storage-directory.png'), fullPage: true });
    const retire = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用对象存储', exact: true }).click();
    await operation(await retire);
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_directory_runtime_disabled_compute_zero_data_and_records_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
