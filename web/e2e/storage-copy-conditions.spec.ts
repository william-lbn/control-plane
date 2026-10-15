import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI owned bucket and signed S3 source conditions preserve bytes and reject failed copies', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !adminFile || !privateDir || !probe)
    throw new Error('Protected owner/admin/Linux SDK inputs required');
  const privatePath: string = privateDir;
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    database: string;
  };
  if (f.database !== 'owner_transfer_v1') throw new Error('Retained owner fixture required');
  const project = f.project_id,
    branch = f.branch_id,
    base = `/api/v1/projects/${project}/branches/${branch}`,
    bucket = 'ci-s3-copyconditions-' + Date.now();
  const keys = ['conditions/source.txt'],
    payload = Buffer.from('Neon real S3 SDK bytes 中文\n');
  const checks: string[] = [],
    operations: string[] = [],
    credentials: string[] = [];
  let result = 'running';
  const checkpoint = {
    bucket,
    keys,
    uploaded: [] as string[],
    credential: null as null | { token_id: string; s3_secret_access_key: string },
    checks,
    operations,
  };
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      path.join(privatePath, 'copy-conditions-original-case.private.json'),
      JSON.stringify(checkpoint),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        credential_ids: credentials,
        bucket,
        uploaded: checkpoint.uploaded,
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
            throw new Error('Original batch operation failed ' + op.id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  try {
    await save();
    await writeFile(path.join(privatePath, 'metadata-edit-payload.private.bin'), payload, {
      mode: 0o600,
      flag: 'wx',
    });
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/storage`);
    const state = await context.request.get(base + '/storage');
    expect(state.status()).toBe(200);
    const before = await state.json();
    expect(before.state).toBe('disabled');
    expect(before.spec.database).toBe(f.database);
    const enable = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用对象存储', exact: true }).click();
    await operation(await enable);
    await page.getByLabel('存储桶名称', { exact: true }).fill(bucket);
    const created = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/storage/buckets' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
    expect((await created).status()).toBe(201);
    await save('ui_owned_auxiliary_bucket_created_original_buckets_untouched');
    await page.getByLabel('对象键', { exact: true }).fill(keys[0]);
    await page
      .getByLabel('上传文件', { exact: true })
      .setInputFiles({ name: 'edit.txt', mimeType: 'text/plain', buffer: payload });
    await page.getByLabel('对象元数据 JSON', { exact: true }).fill('{"purpose":"ui-original"}');
    const put = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/objects` &&
        r.request().method() === 'PUT',
    );
    await page.getByRole('button', { name: '上传对象', exact: true }).click();
    expect((await put).status()).toBe(200);
    checkpoint.uploaded.push(keys[0]);
    await save('ui_owned_source_bytes_uploaded_once');
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据能力').selectOption('storage:write');
    await page.getByLabel('凭据名称').fill('Metadata ' + bucket);
    const issuing = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const response = await issuing;
    expect(response.status()).toBe(201);
    const key = await response.json();
    checkpoint.credential = {
      token_id: key.token_id,
      s3_secret_access_key: key.s3_secret_access_key,
    };
    credentials.push(key.token_id);
    await save('ui_scoped_writer_saved_before_actual_sdk_conditional_copies');
    const config = path.join(privatePath, 'copy-conditions-sdk.private.json');
    await writeFile(
      config,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: key.token_id,
        secret_key: key.s3_secret_access_key,
        bucket,
        mode: 'copy-conditions',
      }),
      { mode: 0o600, flag: 'wx' },
    );
    let output;
    try {
      output = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
    } catch (error) {
      const stdout = (error as { stdout?: string }).stdout;
      if (stdout) await writeFile(info.outputPath('sdk-result.json'), stdout);
      throw new Error('Original SDK COPY conditions failed; retained checkpoint');
    }
    const sdk = JSON.parse(output.stdout);
    expect(sdk.result).toBe('pass');
    for (const check of sdk.checks) await save(check);
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    await expect(
      page.getByRole('cell', { name: 'conditions/source.txt', exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole('cell', { name: 'conditions/case-0.txt', exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole('cell', { name: 'conditions/case-1.txt', exact: true }),
    ).toHaveCount(0);
    await page.screenshot({ path: info.outputPath('storage-copy-conditions.png'), fullPage: true });
    await save('ui_lists_only_original_and_successful_conditional_copies');
    const stop = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用对象存储', exact: true }).click();
    await operation(await stop);
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('owned_runtime_stopped_compute_zero_original_data_and_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
