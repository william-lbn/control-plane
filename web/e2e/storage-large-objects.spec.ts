import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createHash } from 'node:crypto';
import path from 'node:path';

test('large storage UI and actual SDK preserve full bytes, ranges and checksums', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !adminFile || !privateDir || !probe)
    throw new Error('Protected retained data branch and Linux SDK inputs required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    role: string;
    password_file: string;
    resume_file?: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(f.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(f.branch_id) ||
    f.database !== 'owner_transfer_v1' ||
    f.role !== 'cloud_admin'
  )
    throw new Error('Exact retained data branch scope required');
  const base = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}`;
  const state = f.resume_file
    ? JSON.parse(await readFile(f.resume_file, 'utf8'))
    : {
        project_id: f.project_id,
        branch_id: f.branch_id,
        bucket: 'ci-s3-large-' + Date.now(),
        checks: [],
        operations: [],
        upload_id: '',
        credential: null,
        stage: '',
      };
  if (
    state.project_id !== f.project_id ||
    state.branch_id !== f.branch_id ||
    !/^ci-s3-large-\d+$/.test(state.bucket)
  )
    throw new Error('Original large storage checkpoint differs');
  const checks: string[] = state.checks,
    operations: string[] = state.operations,
    bucketPath = base + '/storage/buckets/' + state.bucket;
  const payload = Buffer.alloc(17 * 1024 * 1024 + 19);
  for (let i = 0; i < payload.length; i++) payload[i] = i % 251;
  const digest = createHash('sha256').update(payload).digest('hex');
  let result = 'running';
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    await writeFile(
      path.join(privateDir!, 'storage-large-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        project_id: f.project_id,
        branch_id: f.branch_id,
        bucket: state.bucket,
        upload_id: state.upload_id,
        credential_id: state.credential?.token_id,
        stage: state.stage,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function follow(id: string) {
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${f.project_id}/operations/${id}`);
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
  async function operation(r: Response) {
    expect(r.status()).toBe(202);
    const body = await r.json();
    operations.push(body.operation.id);
    await save();
    await follow(body.operation.id);
  }
  async function storage() {
    await page.goto(`/#/projects/${f.project_id}/storage`);
    await page.getByLabel('存储分支', { exact: true }).selectOption(f.branch_id);
    await expect(page.getByLabel('存储数据库', { exact: true })).toHaveValue(f.database);
  }
  async function selectBucket() {
    await storage();
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: state.bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
  }
  async function verify(key: string) {
    const r = await context.request.get(bucketPath + '/objects?key=' + encodeURIComponent(key), {
      timeout: 120000,
    });
    expect(r.status()).toBe(200);
    const actual = await r.body();
    expect(actual.length).toBe(payload.length);
    expect(createHash('sha256').update(actual).digest('hex')).toBe(digest);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!checks.includes('retained_owner_grants_storage_bootstrap')) {
      await page.goto(`/#/projects/${f.project_id}/query`);
      await page.getByLabel('目标 Endpoint').selectOption(f.writer_id);
      await page.getByLabel('数据库角色', { exact: true }).fill(f.role);
      await page.getByLabel('目标数据库', { exact: true }).fill(f.database);
      await page.getByLabel('执行模式').selectOption('single');
      await page.getByLabel('查询操作').selectOption('query');
      await page
        .getByLabel('SQL 查询')
        .fill(
          'GRANT CREATE, CONNECT ON DATABASE owner_transfer_v1 TO control_probe WITH GRANT OPTION',
        );
      await page
        .getByLabel('数据库角色密码', { exact: true })
        .fill((await readFile(f.password_file, 'utf8')).trim());
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname ===
          `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}/query`,
        { timeout: 190000 },
      );
      await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
      expect((await pending).status()).toBe(200);
      await save('retained_owner_grants_storage_bootstrap');
    }
    for (const id of operations) await follow(id);
    await storage();
    let r = await context.request.get(base + '/storage');
    expect(r.status()).toBe(200);
    let service = await r.json();
    if (service.state !== 'active') {
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '启用对象存储', exact: true }).click();
      await operation(await pending);
    }
    r = await context.request.get(base + '/storage');
    service = await r.json();
    expect(service.limits).toMatchObject({
      object_bytes: 64 * 1024 * 1024,
      part_bytes: 8 * 1024 * 1024,
      branch_bytes: 256 * 1024 * 1024,
    });
    await save('native_storage_enabled_with_64MiB_object_8MiB_part_256MiB_branch_limits');
    await storage();
    await expect(page.getByTestId('storage-s3-panel')).toContainText('64 MiB');
    r = await context.request.get(base + '/storage/buckets');
    expect(r.status()).toBe(200);
    const buckets = await r.json();
    if (!buckets.items.some((b: { name: string }) => b.name === state.bucket)) {
      await page.getByLabel('存储桶名称', { exact: true }).fill(state.bucket);
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/storage/buckets' &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
      expect((await pending).status()).toBe(201);
    } else await selectBucket();
    await save('UI_owned_bucket_created_or_original_reused');
    r = await context.request.head(bucketPath + '/objects?key=large%2Fui-single.bin');
    if (r.status() === 404) {
      await page.getByLabel('对象键', { exact: true }).fill('large/ui-single.bin');
      await page.getByLabel('上传文件', { exact: true }).setInputFiles({
        name: 'ui-single.bin',
        mimeType: 'application/octet-stream',
        buffer: payload,
      });
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === bucketPath + '/objects' && r.request().method() === 'PUT',
        { timeout: 150000 },
      );
      await page.getByRole('button', { name: '上传对象', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(200);
      expect(await response.json()).toMatchObject({ size: payload.length, sha256: digest });
    } else expect(r.status()).toBe(200);
    await verify('large/ui-single.bin');
    await save('UI_17MiB_direct_upload_checksum_and_download_exact_bytes');
    const panel = page.getByTestId('storage-multipart-panel');
    await expect(panel).toContainText('64');
    r = await context.request.head(bucketPath + '/objects?key=large%2Fui-multipart.bin');
    if (r.status() === 404) {
      await panel.getByLabel('分段对象名称', { exact: true }).fill('large/ui-multipart.bin');
      await panel.getByLabel('分段上传文件', { exact: true }).setInputFiles({
        name: 'ui-multipart.bin',
        mimeType: 'application/octet-stream',
        buffer: payload,
      });
      if (!state.upload_id) {
        const pending = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname === bucketPath + '/multipart-uploads' &&
            r.request().method() === 'POST',
        );
        await panel.getByRole('button', { name: '开始分段上传', exact: true }).click();
        const response = await pending;
        expect(response.status()).toBe(201);
        state.upload_id = (await response.json()).upload_id;
        await save();
      } else {
        await panel.getByRole('button', { name: '加载我的分段上传', exact: true }).click();
        await panel.getByLabel('未完成的上传', { exact: true }).selectOption(state.upload_id);
        await panel.getByRole('button', { name: '继续所选上传', exact: true }).click();
      }
      await expect(panel).toContainText('完整 SHA-256 校验通过', { timeout: 240000 });
    } else expect(r.status()).toBe(200);
    await verify('large/ui-multipart.bin');
    await save('UI_17MiB_four_part_full_digest_published_and_downloaded');
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('CSRF cookie required');
    const rejected = await context.request.post(bucketPath + '/multipart-uploads', {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': 'large-oversize-' + state.bucket },
      data: {
        key: 'large/oversize.bin',
        size: 64 * 1024 * 1024 + 1,
        sha256: digest,
        content_type: 'application/octet-stream',
        metadata: {},
      },
    });
    expect(rejected.status()).toBe(400);
    await save('oversized_multipart_manifest_rejected_without_object_publication');
    if (!state.credential) {
      if (state.stage === 'before_credential_issue')
        throw new Error('Inspect original credential issue before repeating');
      await page.goto(`/#/projects/${f.project_id}/credentials`);
      await page.getByLabel('凭据分支', { exact: true }).selectOption(f.branch_id);
      await page.getByLabel('凭据能力', { exact: true }).selectOption('storage:write');
      await page.getByLabel('凭据名称', { exact: true }).fill('Large ' + state.bucket);
      state.stage = 'before_credential_issue';
      await save();
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(201);
      const body = await response.json();
      state.credential = {
        token_id: body.token_id,
        s3_secret_access_key: body.s3_secret_access_key,
      };
      state.stage = 'credential_saved';
      await save('UI_scoped_storage_writer_saved_privately');
      await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    }
    if (!checks.includes('actual_SDK_large_objects_complete')) {
      const cfg = path.join(privateDir, 'large-sdk.private.json');
      await writeFile(
        cfg,
        JSON.stringify({
          endpoint: 'http://192.168.146.100:30893',
          region: 'us-east-2',
          access_key: state.credential.token_id,
          secret_key: state.credential.s3_secret_access_key,
          bucket: state.bucket,
          branch_id: f.branch_id,
          mode: 'large-objects',
          output: path.join(privateDir, 'large-sdk-checkpoint.private.json'),
        }),
        { mode: 0o600 },
      );
      let stdout = '';
      try {
        stdout = (await promisify(execFile)(probe, [cfg], { timeout: 610000, maxBuffer: 65536 }))
          .stdout;
      } catch (error) {
        const output = (error as { stdout?: string }).stdout;
        if (output) {
          const report = JSON.parse(output);
          for (const item of report.checks) await save(item);
          state.stage = 'SDK ' + report.stage;
          await save();
          throw new Error(
            `SDK large objects failed at ${report.stage}; code ${report.error_code}; HTTP ${report.http_statuses?.join(',')}`,
          );
        }
        throw new Error('Inspect protected original SDK process outcome');
      }
      const report = JSON.parse(stdout);
      expect(report.result).toBe('pass');
      for (const item of report.checks) await save(item);
      await save('actual_SDK_large_objects_complete');
    }
    await verify('large/sdk-single.bin');
    await verify('large/sdk-multipart.bin');
    await save('SDK_objects_visible_and_full_digest_verified_through_Console_API');
    await selectBucket();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    for (const key of [
      'large/ui-single.bin',
      'large/ui-multipart.bin',
      'large/sdk-single.bin',
      'large/sdk-multipart.bin',
    ])
      await expect(page.getByRole('cell', { name: key, exact: true })).toBeVisible();
    if (!checks.includes('new_scoped_SDK_credential_revoked')) {
      await page.goto(`/#/projects/${f.project_id}/credentials`);
      await page.getByLabel('凭据分支', { exact: true }).selectOption(f.branch_id);
      const revoking = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials/' + state.credential.token_id &&
          r.request().method() === 'DELETE',
      );
      await page
        .getByTestId('backend-row-' + state.credential.token_id)
        .getByRole('button', { name: '撤销凭据', exact: true })
        .click();
      expect((await revoking).status()).toBe(200);
      await save('new_scoped_SDK_credential_revoked');
    }
    for (const endpoint of [
      'ep_6f3c51bd15b772e3',
      'ep_7d4c985f85c13039',
      'ep_702009efb2684c08',
      'ep_bc9db077429e7b13',
      'ep_1f6d7e3446f01bba',
      f.writer_id,
    ]) {
      const target = `/api/v1/projects/${f.project_id}/endpoints/${endpoint}`;
      const current = await context.request.get(target);
      expect(current.status()).toBe(200);
      if ((await current.json()).observed_state === 'suspended') continue;
      const r = await context.request.post(target + '/suspend', {
        headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': state.bucket + '-zero-' + endpoint },
        data: {},
      });
      expect(r.status()).toBe(202);
      const body = await r.json();
      operations.push(body.operation.id);
      await save();
      await follow(body.operation.id);
      await expect
        .poll(async () => (await (await context.request.get(target)).json()).observed_state, {
          timeout: 150000,
          intervals: [2000, 5000],
        })
        .toBe('suspended');
    }
    await save('all_six_original_test_computes_zero_new_bytes_retained');
    state.stage = 'complete';
    result = 'pass';
    await save('UI_and_actual_SDK_large_objects_all_new_bytes_retained');
  } finally {
    if (result !== 'pass') result = 'fail';
    await save();
  }
});
