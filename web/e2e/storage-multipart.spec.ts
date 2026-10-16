import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI multipart resumes original upload and actual signed SDK publishes and aborts', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !adminFile || !privateDir || !probe)
    throw new Error('Protected retained owner/admin/Linux SDK inputs required');
  const protectedPath: string = privateDir;
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    database: string;
  };
  if (f.database !== 'owner_transfer_v1') throw new Error('Retained owner fixture required');
  const project = f.project_id,
    branch = f.branch_id,
    base = `/api/v1/projects/${project}/branches/${branch}`;
  const resume = process.env.NEON_E2E_MULTIPART_RESUME;
  const checkpoint = resume
    ? JSON.parse(await readFile(resume, 'utf8'))
    : {
        bucket: 'ci-s3-multipart-' + Date.now(),
        upload_id: '',
        aborted_upload_id: '',
        credential: null,
        checks: [] as string[],
        operations: [] as string[],
        published: false,
        original_state: '',
      };
  const bucket: string = checkpoint.bucket,
    checks: string[] = checkpoint.checks,
    operations: string[] = checkpoint.operations;
  if (!/^ci-s3-multipart-\d+$/.test(bucket))
    throw new Error('Original owned multipart bucket required');
  const bucketPath = base + `/storage/buckets/${bucket}`,
    mp = bucketPath + '/multipart-uploads',
    payload = Buffer.alloc(5 * 1024 * 1024 + 97, 'u');
  let result = 'running';
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    await writeFile(
      path.join(protectedPath, 'multipart-original-case.private.json'),
      JSON.stringify(checkpoint),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        bucket,
        project_id: project,
        branch_id: branch,
        upload_id: checkpoint.upload_id,
        aborted_upload_id: checkpoint.aborted_upload_id,
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
            throw new Error('Original multipart operation failed ' + op.id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function selectBucket() {
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    const projectRead = await context.request.get(`/api/v1/projects/${project}`);
    expect(projectRead.status()).toBe(200);
    const actualProject = await projectRead.json();
    expect(actualProject.org_id).toBeTruthy();
    await page.goto(`/#/projects/${project}/storage`);
    const state = await context.request.get(base + '/storage');
    expect(state.status()).toBe(200);
    const before = await state.json();
    expect(before.spec.database).toBe(f.database);
    if (!checkpoint.original_state) {
      checkpoint.original_state = before.state;
      await save();
    }
    if (before.state === 'disabled') {
      const enabling = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '启用对象存储', exact: true }).click();
      await operation(await enabling);
    } else expect(before.state).toBe('active');
    if (!checks.includes('UI_owned_bucket_created')) {
      await page.getByLabel('存储桶名称', { exact: true }).fill(bucket);
      const created = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/storage/buckets' &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
      expect((await created).status()).toBe(201);
      await save('UI_owned_bucket_created');
    } else await selectBucket();
    const panel = page.getByTestId('storage-multipart-panel');
    await expect(panel).toBeVisible();
    if (!checkpoint.upload_id) {
      // Fail the second part before dispatch. Resume must query the ORIGINAL
      // upload, verify its file digest and skip its confirmed first part.
      let interrupted = false;
      await page.route('**/multipart-uploads/*/parts/2', async (route) => {
        interrupted = true;
        await route.abort('failed');
      });
      await panel.getByLabel('分段对象名称', { exact: true }).fill('multipart/ui.bin');
      await panel
        .getByLabel('分段上传文件', { exact: true })
        .setInputFiles({ name: 'ui.bin', mimeType: 'application/octet-stream', buffer: payload });
      const creating = page.waitForResponse(
        (r) => new URL(r.url()).pathname === mp && r.request().method() === 'POST',
      );
      await panel.getByRole('button', { name: '开始分段上传', exact: true }).click();
      const response = await creating;
      expect(response.status()).toBe(201);
      checkpoint.upload_id = (await response.json()).upload_id;
      await save();
      await expect(panel.getByRole('button', { name: '开始分段上传', exact: true })).toBeEnabled({
        timeout: 120000,
      });
      expect(interrupted).toBe(true);
      await page.unroute('**/multipart-uploads/*/parts/2');
      const original = await context.request.get(mp + '/' + checkpoint.upload_id);
      expect(original.status()).toBe(200);
      const partial = await original.json();
      expect(partial.parts.map((p: { part_number: number }) => p.part_number)).toEqual([1]);
      const absent = await context.request.head(bucketPath + '/objects?key=multipart%2Fui.bin');
      expect(absent.status()).toBe(404);
      await save('UI_interrupted_original_upload_first_part_retained_object_invisible');
    }
    if (!checkpoint.published) {
      await selectBucket();
      await panel.getByRole('button', { name: '加载我的分段上传', exact: true }).click();
      await panel.getByLabel('未完成的上传', { exact: true }).selectOption(checkpoint.upload_id);
      await panel
        .getByLabel('分段上传文件', { exact: true })
        .setInputFiles({ name: 'ui.bin', mimeType: 'application/octet-stream', buffer: payload });
      const partRequests: number[] = [];
      const record = (r: { url: () => string; method: () => string }) => {
        const match = new URL(r.url()).pathname.match(/\/multipart-uploads\/[^/]+\/parts\/(\d+)$/);
        if (match && r.method() === 'PUT') partRequests.push(Number(match[1]));
      };
      page.on('request', record);
      const completing = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === mp + '/' + checkpoint.upload_id + '/complete' &&
          r.request().method() === 'POST',
      );
      await panel.getByRole('button', { name: '继续所选上传', exact: true }).click();
      expect((await completing).status()).toBe(200);
      await expect(panel).toContainText('完整 SHA-256 校验通过');
      page.off('request', record);
      expect(partRequests).toEqual([2]);
      checkpoint.published = true;
      await save('UI_reload_resume_original_upload_skips_confirmed_first_part_and_publishes');
    }
    const originalObject = await context.request.get(
      bucketPath + '/objects?key=multipart%2Fui.bin',
    );
    expect(originalObject.status()).toBe(200);
    expect(await originalObject.body()).toEqual(payload);
    await save('UI_download_matches_complete_original_file');
    if (!checkpoint.credential) {
      await page.goto(`/#/projects/${project}/credentials`);
      await page.getByLabel('凭据能力', { exact: true }).selectOption('storage:write');
      await page.getByLabel('凭据名称', { exact: true }).fill('Multipart ' + bucket);
      const issuing = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
      const response = await issuing;
      expect(response.status()).toBe(201);
      const c = await response.json();
      checkpoint.credential = {
        token_id: c.token_id,
        s3_secret_access_key: c.s3_secret_access_key,
      };
      await save('UI_scoped_S3_credential_saved_before_SDK');
    }
    if (!checks.includes('SDK_all_seven_multipart_operations_passed')) {
      if (
        resume &&
        checks.includes('SDK_dispatched') &&
        !checks.includes('SDK_prior_before_create_signature_failure_verified')
      )
        throw new Error(
          'Inspect original protected SDK checkpoint before retry; mutations must not be recreated',
        );
      const config = path.join(protectedPath, 'multipart-sdk.private.json');
      await writeFile(
        config,
        JSON.stringify({
          endpoint: 'http://192.168.146.100:30893',
          region: 'us-east-2',
          access_key: checkpoint.credential.token_id,
          secret_key: checkpoint.credential.s3_secret_access_key,
          bucket,
          mode: 'multipart',
          output: path.join(protectedPath, 'multipart-sdk-checkpoint.private.json'),
        }),
        { mode: 0o600, flag: 'wx' },
      );
      await save('SDK_dispatched');
      let output;
      try {
        output = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
      } catch (error) {
        const stdout = (error as { stdout?: string }).stdout;
        if (stdout) await writeFile(info.outputPath('sdk-result.json'), stdout);
        throw new Error('Original SDK multipart failed; inspect protected checkpoint');
      }
      const sdk = JSON.parse(output.stdout);
      expect(sdk.result).toBe('pass');
      for (const check of sdk.checks) await save(check);
      await save('SDK_all_seven_multipart_operations_passed');
    }
    await selectBucket();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    await expect(
      page.getByRole('cell', { name: 'multipart/sdk-中文.bin', exact: true }),
    ).toBeVisible();
    await save('UI_reads_SDK_published_object');
    if (!checkpoint.aborted_upload_id) {
      await page.route('**/multipart-uploads/*/parts/1', (route) => route.abort('failed'));
      await panel.getByLabel('分段对象名称', { exact: true }).fill('multipart/ui-aborted.bin');
      await panel.getByLabel('分段上传文件', { exact: true }).setInputFiles({
        name: 'abort.bin',
        mimeType: 'application/octet-stream',
        buffer: Buffer.from('abort-owned-fixture'),
      });
      const creating = page.waitForResponse(
        (r) => new URL(r.url()).pathname === mp && r.request().method() === 'POST',
      );
      await panel.getByRole('button', { name: '开始分段上传', exact: true }).click();
      const created = await creating;
      expect(created.status()).toBe(201);
      checkpoint.aborted_upload_id = (await created.json()).upload_id;
      await save();
      await expect(panel.getByRole('button', { name: '开始分段上传', exact: true })).toBeEnabled();
      await page.unroute('**/multipart-uploads/*/parts/1');
    }
    if (!checks.includes('UI_abort_closes_original_upload')) {
      await panel.getByRole('button', { name: '加载我的分段上传', exact: true }).click();
      await panel
        .getByLabel('未完成的上传', { exact: true })
        .selectOption(checkpoint.aborted_upload_id);
      const aborting = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === mp + '/' + checkpoint.aborted_upload_id &&
          r.request().method() === 'DELETE',
      );
      await panel.getByRole('button', { name: '中止所选上传', exact: true }).click();
      expect((await aborting).status()).toBe(204);
      await expect(panel).toContainText('物理分片仍保留');
      await save('UI_abort_closes_original_upload');
    }
    await page.screenshot({ path: info.outputPath('storage-multipart.png'), fullPage: true });
    if (!checks.includes('UI_owned_SDK_credential_revoked')) {
      await page.goto(`/#/projects/${project}/credentials`);
      const revoked = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials/' + checkpoint.credential.token_id &&
          r.request().method() === 'DELETE',
      );
      await page
        .getByTestId('backend-row-' + checkpoint.credential.token_id)
        .getByRole('button', { name: '撤销凭据', exact: true })
        .click();
      expect((await revoked).status()).toBe(200);
      await save('UI_owned_SDK_credential_revoked');
      await selectBucket();
    }
    if (checkpoint.original_state === 'disabled') {
      const disabling = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'DELETE',
      );
      await page.getByRole('button', { name: '禁用对象存储', exact: true }).click();
      await operation(await disabling);
    }
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save('Compute_zero_original_business_data_and_all_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
