import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI exact-byte capability upload and actual S3 SDK presigned PUT', async ({
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
    bucket = 'ci-s3-presign-' + Date.now();
  const keys = ['presigned/ui-中文.txt'],
    payload = Buffer.from('Original presigned upload bytes; retained and repeatable.');
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
      path.join(privatePath, 'presign-original-case.private.json'),
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
            throw new Error('Original presign operation failed ' + op.id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  try {
    await save();
    await writeFile(path.join(privatePath, 'presign-payload.private.bin'), payload, {
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
    await save('ui_presigned_case_created_original_data_untouched');
    await page.getByLabel('对象键', { exact: true }).fill(keys[0]);
    await page
      .getByLabel('上传文件', { exact: true })
      .setInputFiles({ name: 'ui-中文.txt', mimeType: 'text/plain', buffer: payload });
    const mint = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/presign-upload` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '生成临时上传地址', exact: true }).click();
    const minted = await mint;
    expect(minted.status()).toBe(200);
    const cap = await minted.json();
    await writeFile(
      path.join(privatePath, 'presign-capability.private.json'),
      JSON.stringify(cap),
      { mode: 0o600, flag: 'wx' },
    );
    await expect(page.getByLabel('临时上传地址', { exact: true })).toHaveAttribute(
      'type',
      'password',
    );
    await save('ui_minted_masked_exact_byte_create_only_capability_saved_privately');
    const anonymous = await context
      .browser()!
      .newContext({ baseURL: process.env.NEON_E2E_BASE_URL });
    try {
      const changed = await anonymous.request.put(cap.url, {
        headers: cap.headers,
        data: Buffer.from('changed bytes'),
      });
      expect(changed.status()).toBe(400);
      const missing = await anonymous.request.put(cap.url, {
        headers: { 'Content-Type': 'text/plain' },
        data: payload,
      });
      expect(missing.status()).toBe(403);
      const tampered = await anonymous.request.put(cap.url.replace('key=', 'key=other'), {
        headers: cap.headers,
        data: payload,
      });
      expect(tampered.status()).toBe(403);
      await save('anonymous_wrong_bytes_missing_condition_and_changed_key_denied');
      const actual = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/storage/v1/${branch}/${bucket}` &&
          r.request().method() === 'PUT',
      );
      await page.getByRole('button', { name: '使用临时地址上传', exact: true }).click();
      expect((await actual).status()).toBe(200);
      checkpoint.uploaded.push(keys[0]);
      await save('real_browser_fetch_without_cookie_committed_exact_authorized_bytes');
      const replay = await anonymous.request.put(cap.url, { headers: cap.headers, data: payload });
      expect(replay.status()).toBe(412);
      await save('replay_cannot_overwrite_created_object');
    } finally {
      await anonymous.close();
    }
    const actualBytes = await context.request.get(
      base + `/storage/buckets/${bucket}/objects?` + new URLSearchParams({ key: keys[0] }),
    );
    expect(actualBytes.status()).toBe(200);
    expect(await actualBytes.body()).toEqual(payload);
    await save('ui_created_object_real_bytes_verified');
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据能力').selectOption('storage:write');
    await page.getByLabel('凭据名称').fill('Presigned ' + bucket);
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
    await save('writer_saved_before_presigned_sdk_mutation');
    const config = path.join(privatePath, 'presign-sdk.private.json');
    await writeFile(
      config,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: key.token_id,
        secret_key: key.s3_secret_access_key,
        bucket,
        mode: 'presigned-put',
      }),
      { mode: 0o600, flag: 'wx' },
    );
    let output;
    try {
      output = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
    } catch (error) {
      const stdout = (error as { stdout?: string }).stdout;
      if (stdout) await writeFile(info.outputPath('sdk-result.json'), stdout);
      throw new Error('Original SDK presigned PUT failed; inspect saved checkpoint');
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
    await expect(page.getByRole('cell', { name: keys[0], exact: true })).toBeVisible();
    await page.screenshot({
      path: info.outputPath('storage-presigned-upload.png'),
      fullPage: true,
    });
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
