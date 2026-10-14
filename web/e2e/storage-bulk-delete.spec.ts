import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI atomic versioned batch delete and actual S3 DeleteObjects retain control data', async ({
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
    bucket = 'ci-s3-bulk-' + Date.now();
  const keys = ['keep.txt', 'aux/ui-a.txt', 'aux/ui-中文.txt', 'aux/sdk-a.txt', 'aux/sdk-中文.txt'],
    payload = Buffer.from(
      'Original batch-delete fixture bytes; retained control and physical history.',
    );
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
      path.join(privatePath, 'bulk-original-case.private.json'),
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
    await writeFile(path.join(privatePath, 'bulk-payload.private.bin'), payload, {
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
    for (const key of keys) {
      await page.getByLabel('对象键', { exact: true }).fill(key);
      await page
        .getByLabel('上传文件', { exact: true })
        .setInputFiles({ name: key.split('/').at(-1)!, mimeType: 'text/plain', buffer: payload });
      const put = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/objects` &&
          r.request().method() === 'PUT',
      );
      await page.getByRole('button', { name: '上传对象', exact: true }).click();
      expect((await put).status()).toBe(200);
      checkpoint.uploaded.push(key);
      await save();
    }
    await save('five_real_uploaded_auxiliary_objects_and_private_original_bytes_recorded');
    const listing = await context.request.get(base + `/storage/buckets/${bucket}/objects`);
    expect(listing.status()).toBe(200);
    const items = (await listing.json()).items as { key: string; sha256: string }[];
    const stale = [
      { key: 'aux/ui-a.txt', sha256: items.find((x) => x.key === 'aux/ui-a.txt')!.sha256 },
      { key: 'aux/ui-中文.txt', sha256: '0'.repeat(64) },
    ];
    const denied = await page.evaluate(
      async ({ url, objects }) => {
        const token =
          document.cookie
            .split(';')
            .map((v) => v.trim())
            .find((v) => v.startsWith('neon_v2_csrf='))
            ?.split('=')[1] ?? '';
        return (
          await fetch(url, {
            method: 'POST',
            credentials: 'same-origin',
            headers: {
              'Content-Type': 'application/json',
              'X-CSRF-Token': decodeURIComponent(token),
            },
            body: JSON.stringify({ objects }),
          })
        ).status;
      },
      { url: base + `/storage/buckets/${bucket}/delete-batch`, objects: stale },
    );
    expect(denied).toBe(412);
    const intact = await context.request.get(base + `/storage/buckets/${bucket}/objects`);
    expect((await intact.json()).items).toHaveLength(5);
    await save('one_stale_version_rejects_entire_batch_without_partial_deletion');
    await page.getByLabel('选择对象 aux/ui-a.txt', { exact: true }).check();
    await page.getByLabel('选择对象 aux/ui-中文.txt', { exact: true }).check();
    const deleted = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/delete-batch` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '删除所选对象', exact: true }).click();
    const reply = await deleted;
    expect(reply.status()).toBe(200);
    expect((await reply.json()).deleted_keys.sort()).toEqual(
      ['aux/ui-a.txt', 'aux/ui-中文.txt'].sort(),
    );
    await expect(page.getByRole('cell', { name: 'aux/ui-a.txt', exact: true })).toHaveCount(0);
    await expect(page.getByRole('cell', { name: 'aux/ui-中文.txt', exact: true })).toHaveCount(0);
    await expect(page.getByRole('cell', { name: 'keep.txt', exact: true })).toBeVisible();
    await save('actual_ui_selected_batch_deletes_only_two_observed_auxiliary_versions');
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据能力').selectOption('storage:write');
    await page.getByLabel('凭据名称').fill('Bulk delete ' + bucket);
    const issuing = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const keyResponse = await issuing;
    expect(keyResponse.status()).toBe(201);
    const key = await keyResponse.json();
    checkpoint.credential = {
      token_id: key.token_id,
      s3_secret_access_key: key.s3_secret_access_key,
    };
    credentials.push(key.token_id);
    await save('original_writer_credential_saved_privately_before_sdk_mutation');
    const config = path.join(privatePath, 'bulk-sdk.private.json');
    await writeFile(
      config,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: key.token_id,
        secret_key: key.s3_secret_access_key,
        bucket,
        mode: 'bulk',
      }),
      { mode: 0o600, flag: 'wx' },
    );
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    let sdkOutput;
    try {
      sdkOutput = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
    } catch (error) {
      const stdout = (error as { stdout?: string }).stdout;
      if (stdout) await writeFile(info.outputPath('sdk-result.json'), stdout);
      throw new Error('Original SDK batch failed; sanitized process result retained');
    }
    const sdk = JSON.parse(sdkOutput.stdout);
    expect(sdk.result).toBe('pass');
    for (const check of sdk.checks) await save(check);
    const keep = await context.request.get(
      base + `/storage/buckets/${bucket}/objects?key=keep.txt`,
    );
    expect(keep.status()).toBe(200);
    expect(await keep.body()).toEqual(payload);
    const after = await context.request.get(base + `/storage/buckets/${bucket}/objects`);
    expect((await after.json()).items.map((x: { key: string }) => x.key)).toEqual(['keep.txt']);
    await save('real_sdk_deleted_only_auxiliary_keys_retained_control_bytes_match');
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    await expect(page.getByRole('cell', { name: 'keep.txt', exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath('storage-bulk-delete.png'), fullPage: true });
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
