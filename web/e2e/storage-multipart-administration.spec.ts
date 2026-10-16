import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI recovers revoked-credential multipart uploads and lost administrator response', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !adminFile || !privateDir || !probe)
    throw new Error('Protected retained owner/admin and Linux SDK inputs required');
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
  const resume = process.env.NEON_E2E_MULTIPART_ADMIN_RESUME;
  const checkpoint = resume
    ? JSON.parse(await readFile(resume, 'utf8'))
    : {
        bucket: 'ci-s3-mpadmin-' + Date.now(),
        checks: [] as string[],
        operations: [] as string[],
        credential: null,
        original_state: '',
        sdk_started: false,
        upload_ids: [] as string[],
        revoked: false,
        receipts: [] as { upload_id: string; intent_id: string }[],
        lost_key: '',
        lost_receipt: null,
      };
  const bucket: string = checkpoint.bucket,
    checks: string[] = checkpoint.checks,
    operations: string[] = checkpoint.operations;
  if (!/^ci-s3-mpadmin-\d+$/.test(bucket))
    throw new Error('Original owned administration bucket required');
  const bucketPath = base + `/storage/buckets/${bucket}`,
    admin = bucketPath + '/multipart-administration';
  const reason = 'Recover owned uploads after credential revocation';
  let result = 'running';
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    await writeFile(
      path.join(protectedPath, 'multipart-admin-original.private.json'),
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
        upload_ids: checkpoint.upload_ids,
        receipts: checkpoint.receipts,
        production_qualified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
  }
  async function operation(response: Response) {
    expect(response.status()).toBe(202);
    const v = await response.json();
    operations.push(v.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${v.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(
              'Original administration operation failed ' + op.id + ' ' + op.error_code,
            );
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
    if (!checks.includes('UI_owned_admin_bucket_created')) {
      await page.getByLabel('存储桶名称', { exact: true }).fill(bucket);
      const created = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/storage/buckets' &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
      expect((await created).status()).toBe(201);
      await save('UI_owned_admin_bucket_created');
    }
    if (!checkpoint.credential) {
      await page.goto(`/#/projects/${project}/credentials`);
      await page.getByLabel('凭据能力', { exact: true }).selectOption('storage:write');
      await page.getByLabel('凭据名称', { exact: true }).fill('MP admin ' + bucket);
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
      await save('UI_original_scoped_credential_retained');
    }
    if (!checkpoint.upload_ids.length) {
      if (checkpoint.sdk_started)
        throw new Error(
          'Inspect original protected SDK checkpoint before continuing unknown mutations',
        );
      const config = path.join(protectedPath, 'multipart-admin-sdk.private.json'),
        output = path.join(protectedPath, 'multipart-admin-sdk-checkpoint.private.json');
      await writeFile(
        config,
        JSON.stringify({
          endpoint: 'http://192.168.146.100:30893',
          region: 'us-east-2',
          access_key: checkpoint.credential.token_id,
          secret_key: checkpoint.credential.s3_secret_access_key,
          bucket,
          mode: 'multipart-administration',
          output,
        }),
        { mode: 0o600, flag: 'wx' },
      );
      checkpoint.sdk_started = true;
      await save();
      let stdout: string;
      try {
        stdout = (await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 }))
          .stdout;
      } catch (error) {
        const value = (error as { stdout?: string }).stdout;
        if (value) await writeFile(info.outputPath('sdk-result.json'), value);
        throw new Error('Original multipart grouping SDK failed; retain original checkpoint');
      }
      const sdk = JSON.parse(stdout);
      expect(sdk.result).toBe('pass');
      const original = JSON.parse(await readFile(output, 'utf8'));
      expect(original.stage).toBe('ready_for_credential_revocation_and_admin_abort');
      expect(original.upload_ids).toHaveLength(3);
      checkpoint.upload_ids = original.upload_ids;
      for (const c of sdk.checks) await save(c);
      await save('SDK_original_three_uploads_saved_for_admin_recovery');
    }
    if (!checkpoint.revoked) {
      await page.goto(`/#/projects/${project}/credentials`);
      const revoking = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials/' + checkpoint.credential.token_id &&
          r.request().method() === 'DELETE',
      );
      await page
        .getByTestId('backend-row-' + checkpoint.credential.token_id)
        .getByRole('button', { name: '撤销凭据', exact: true })
        .click();
      expect((await revoking).status()).toBe(200);
      checkpoint.revoked = true;
      await save('UI_original_initiator_revoked_before_recovery');
    }
    await selectBucket();
    const panel = page.getByTestId('storage-multipart-administration');
    await expect(panel).toBeVisible();
    await panel.getByRole('button', { name: '加载全部活动上传', exact: true }).click();
    await panel.getByLabel('中止原因', { exact: true }).fill(reason);
    for (const id of checkpoint.upload_ids as string[]) {
      if (checkpoint.receipts.some((v: { upload_id: string }) => v.upload_id === id)) continue;
      const row = panel.getByTestId('multipart-admin-row-' + id);
      await expect(row).toContainText('credential:' + checkpoint.credential.token_id);
      if (!checks.includes('UI_lost_abort_response_replayed_original_receipt')) {
        if (checkpoint.lost_receipt)
          throw new Error('Inspect original lost-response checkpoint; do not invent a new intent');
        const target = admin + '/' + id + '/abort';
        await page.route('**' + target, async (route) => {
          checkpoint.lost_key = route.request().headers()['idempotency-key'];
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          checkpoint.lost_receipt = await response.json();
          await save('server_confirmed_original_abort_before_response_loss');
          await route.abort('failed');
        });
        await row.getByRole('button', { name: '中止遗留上传', exact: true }).click();
        await expect(panel).toContainText('请求结果尚未确认');
        await page.unroute('**' + target);
        await panel.getByLabel('查询原上传 ID', { exact: true }).fill(id);
        await panel.getByRole('button', { name: '查询原上传状态', exact: true }).click();
        await expect(panel.getByTestId('multipart-admin-row-' + id)).toContainText('aborted');
        const replay = page.waitForResponse(
          (r) => new URL(r.url()).pathname === target && r.request().method() === 'POST',
        );
        await panel
          .getByTestId('multipart-admin-row-' + id)
          .getByRole('button', { name: '确认原中止回执', exact: true })
          .click();
        const response = await replay;
        expect(response.status()).toBe(200);
        expect(response.headers()['idempotency-replayed']).toBe('true');
        expect(response.request().headers()['idempotency-key']).toBe(checkpoint.lost_key);
        const receipt = await response.json();
        expect(receipt).toEqual(checkpoint.lost_receipt);
        await expect(panel.getByTestId('multipart-admin-error')).toHaveCount(0);
        checkpoint.receipts.push({ upload_id: id, intent_id: receipt.intent_id });
        await save('UI_lost_abort_response_replayed_original_receipt');
      } else {
        const aborting = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname === admin + '/' + id + '/abort' &&
            r.request().method() === 'POST',
        );
        await row.getByRole('button', { name: '中止遗留上传', exact: true }).click();
        const response = await aborting;
        expect(response.status()).toBe(200);
        const receipt = await response.json();
        expect(receipt.state).toBe('aborted');
        checkpoint.receipts.push({ upload_id: id, intent_id: receipt.intent_id });
        await save();
      }
    }
    await panel.getByRole('button', { name: '加载全部活动上传', exact: true }).click();
    await expect(panel.locator('tbody tr')).toHaveCount(0);
    await save('UI_all_original_revoked_credential_uploads_logically_retired');
    if (!checks.includes('UI_local_read_failure_clears_after_original_metadata_reload')) {
      await page.route('**' + admin, (route) => route.abort('failed'));
      await panel.getByRole('button', { name: '加载全部活动上传', exact: true }).click();
      await expect(panel.getByTestId('multipart-admin-error')).toBeVisible();
      await page.unroute('**' + admin);
      await panel.getByRole('button', { name: '加载全部活动上传', exact: true }).click();
      await expect(panel.getByTestId('multipart-admin-error')).toHaveCount(0);
      await expect(page.getByText('TypeError: Failed to fetch', { exact: true })).toHaveCount(0);
      await save('UI_local_read_failure_clears_after_original_metadata_reload');
    }
    const objects = await context.request.get(bucketPath + '/objects');
    expect(objects.status()).toBe(200);
    expect((await objects.json()).items).toHaveLength(0);
    await save('no_partial_object_published_physical_part_and_original_business_objects_retained');
    await page.screenshot({
      path: info.outputPath('storage-multipart-administration.png'),
      fullPage: true,
    });
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
    await save('owned_runtime_zero_all_original_evidence_preserved');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
