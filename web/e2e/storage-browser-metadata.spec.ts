import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';
import { createServer } from 'node:http';

test('UI CORS browser preflight and REST/S3 metadata preserved', async ({
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
  const resume = await readFile(path.join(privatePath, 'cors-metadata-resume.private.json'), 'utf8')
    .then((v) => JSON.parse(v))
    .catch((e: NodeJS.ErrnoException) => {
      if (e.code === 'ENOENT') return null;
      throw e;
    });
  const project = f.project_id,
    branch = f.branch_id,
    base = `/api/v1/projects/${project}/branches/${branch}`,
    bucket = resume?.bucket || 'ci-s3-browser-' + Date.now();
  const keys = ['metadata/ui.txt'],
    payload = Buffer.from('Original presigned upload bytes; retained and repeatable.');
  const checks: string[] = resume?.checks || [],
    operations: string[] = resume?.operations || [],
    credentials: string[] = resume ? [resume.credential.token_id] : [];
  let result = 'running';
  const checkpoint = {
    bucket,
    keys,
    uploaded: resume ? (resume.uploaded as string[]) : [],
    credential: resume?.credential as null | { token_id: string; s3_secret_access_key: string },
    checks,
    operations,
  };
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      path.join(privatePath, 'cors-metadata-original-case.private.json'),
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
    await writeFile(path.join(privatePath, 'cors-metadata-payload.private.bin'), payload, {
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
    if (!resume) {
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
          new URL(r.url()).pathname === base + '/storage/buckets' &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
      expect((await created).status()).toBe(201);
      await save('ui_presigned_case_created_original_data_untouched');
    }
    const policy = [
      {
        id: 'local-browser',
        allowed_origins: ['http://127.0.0.1:19001'],
        allowed_methods: ['PUT', 'HEAD', 'GET'],
        allowed_headers: ['content-type', 'if-none-match', 'x-amz-meta-*'],
        expose_headers: ['ETag', 'x-amz-meta-purpose'],
        max_age_seconds: 60,
      },
    ];
    let key = resume?.credential
      ? {
          token_id: resume.credential.token_id,
          s3_secret_access_key: resume.credential.s3_secret_access_key,
        }
      : null;
    if (!resume) {
      await page.getByRole('button', { name: '加载 CORS 规则', exact: true }).click();
      await page.getByLabel('CORS 规则 JSON', { exact: true }).fill(JSON.stringify(policy));
      const policyReply = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/cors` &&
          r.request().method() === 'PUT',
      );
      await page.getByRole('button', { name: '保存 CORS 规则', exact: true }).click();
      expect((await policyReply).status()).toBe(200);
      await save('ui_conditional_bucket_cors_policy_saved');
      await page.getByLabel('对象键', { exact: true }).fill(keys[0]);
      await page
        .getByLabel('上传文件', { exact: true })
        .setInputFiles({ name: 'ui.txt', mimeType: 'text/plain', buffer: payload });
      await page
        .getByLabel('对象元数据 JSON', { exact: true })
        .fill(JSON.stringify({ purpose: 'ui-native', 'owner-id': 'retained-fixture' }));
      const put = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + `/storage/buckets/${bucket}/objects` &&
          r.request().method() === 'PUT',
      );
      await page.getByRole('button', { name: '上传对象', exact: true }).click();
      expect((await put).status()).toBe(200);
      checkpoint.uploaded.push(keys[0]);
      await save('ui_file_and_user_metadata_committed');
      await page
        .getByRole('row')
        .filter({ has: page.getByRole('cell', { name: keys[0], exact: true }) })
        .getByRole('button', { name: '元数据', exact: true })
        .click();
      await expect(page.getByTestId('storage-object-metadata')).toContainText('ui-native');
      await save('ui_live_head_metadata_matches_uploaded_properties');
      await page.goto(`/#/projects/${project}/credentials`);
      await page.getByLabel('凭据能力').selectOption('storage:write');
      await page.getByLabel('凭据名称').fill('Browser ' + bucket);
      const issuing = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
      const keyResponse = await issuing;
      expect(keyResponse.status()).toBe(201);
      key = await keyResponse.json();
      checkpoint.credential = {
        token_id: key!.token_id,
        s3_secret_access_key: key!.s3_secret_access_key,
      };
      credentials.push(key!.token_id);
      await save('original_writer_saved_before_sdk_metadata_mutation');
    }
    if (!key) throw new Error('Original writer credential required');
    const capability = path.join(privatePath, 'cors-browser-capability.private.json'),
      config = path.join(privatePath, 'cors-sdk.private.json');
    await writeFile(
      config,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: key.token_id,
        secret_key: key.s3_secret_access_key,
        bucket,
        mode: 'cors-metadata',
        branch_id: branch,
        output: capability,
      }),
      { mode: 0o600, flag: 'wx' },
    );
    let output;
    try {
      output = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
    } catch (error) {
      const stdout = (error as { stdout?: string }).stdout;
      if (stdout) await writeFile(info.outputPath('sdk-result.json'), stdout);
      throw new Error('Original SDK metadata/CORS case failed; checkpoint retained');
    }
    const sdk = JSON.parse(output.stdout);
    expect(sdk.result).toBe('pass');
    for (const check of sdk.checks) await save(check);
    const cap = JSON.parse(await readFile(capability, 'utf8'));
    for (const port of [19002, 19001]) {
      const server = createServer((_, response) => {
        response.setHeader('Content-Type', 'text/html');
        response.end('<!doctype html><title>Real local browser CORS origin</title>');
      });
      await new Promise<void>((resolve, reject) => {
        server.once('error', reject);
        server.listen(port, '127.0.0.1', resolve);
      });
      const browserContext = await context.browser()!.newContext();
      const browserPage = await browserContext.newPage();
      try {
        await browserPage.goto(`http://127.0.0.1:${port}/`);
        const actual = await browserPage.evaluate(async (cap) => {
          try {
            const response = await fetch(cap.url, {
              method: 'PUT',
              headers: cap.headers,
              body: cap.body,
              credentials: 'omit',
              redirect: 'error',
            });
            return { status: response.status, etag: response.headers.get('etag'), blocked: false };
          } catch {
            return { status: 0, etag: null, blocked: true };
          }
        }, cap);
        if (port === 19002) {
          expect(actual.blocked).toBe(true);
          await save('real_browser_disallowed_origin_preflight_blocks_upload');
        } else {
          expect(actual.status).toBe(200);
          expect(actual.etag).toMatch(/^"[a-f0-9]{64}"$/);
          await save('real_browser_allowed_origin_preflight_put_and_exposed_etag');
        }
      } finally {
        await browserContext.close();
        await new Promise<void>((resolve) => server.close(() => resolve()));
      }
    }
    const head = await context.request.head(
      base + `/storage/buckets/${bucket}/objects?key=metadata/browser.txt`,
    );
    expect(head.status()).toBe(200);
    expect(head.headers()['x-amz-meta-purpose']).toBe('browser-native');
    await save('actual_browser_object_metadata_verified_after_upload');
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    await expect(
      page.getByRole('cell', { name: 'metadata/browser.txt', exact: true }),
    ).toBeVisible();
    await page.screenshot({
      path: info.outputPath('storage-browser-metadata.png'),
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
