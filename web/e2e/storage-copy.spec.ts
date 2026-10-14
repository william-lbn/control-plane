import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('UI guarded object copy and actual signed SDK COPY preserve original bytes', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    sourceFile = process.env.NEON_E2E_COPY_SOURCE_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !sourceFile || !adminFile || !privateDir || !probe)
    throw new Error('Protected exact original source, owner and Linux SDK inputs required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    database: string;
  };
  const source = JSON.parse(await readFile(sourceFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    bucket: string;
    source_key: string;
  };
  if (
    f.project_id !== source.project_id ||
    f.branch_id !== source.branch_id ||
    source.source_key !== 'directory/root.txt'
  )
    throw new Error('Original accepted directory fixture required');
  const resumeFile = process.env.NEON_E2E_COPY_RESUME;
  const resume = resumeFile
    ? (JSON.parse(await readFile(resumeFile, 'utf8')) as {
        targetBucket: string;
        checks: string[];
        operations: string[];
        credential: { token_id: string; s3_secret_access_key: string };
      })
    : null;
  const project = f.project_id,
    branch = f.branch_id,
    base = `/api/v1/projects/${project}/branches/${branch}`,
    targetBucket = resume?.targetBucket ?? 'ci-s3-copy-' + Date.now(),
    targetKey = 'copies/ui.txt';
  const checks: string[] = resume ? [...resume.checks] : [],
    operations: string[] = resume ? [...resume.operations] : [],
    credentials: string[] = [];
  let result = 'running';
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        credential_ids: credentials,
        source_bucket: source.bucket,
        target_bucket: targetBucket,
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
            throw new Error('Original copy enable/disable failed ' + op.id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function choose(bucket: string) {
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
  }
  try {
    await save();
    await writeFile(
      path.join(privateDir, 'copy-original-case.json'),
      JSON.stringify({ source, targetBucket, targetKey }),
      { mode: 0o600, flag: 'wx' },
    );
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/storage`);
    const state = await context.request.get(base + '/storage');
    expect(state.status()).toBe(200);
    const prior = await state.json();
    if (resume) {
      expect(prior.state).toBe('active');
      expect(prior.spec.database).toBe(f.database);
      for (const id of resume.operations) {
        const original = await context.request.get(`/api/v1/projects/${project}/operations/${id}`);
        expect(original.status()).toBe(200);
        expect((await original.json()).state).toBe('succeeded');
      }
    } else {
      expect(prior.state).toBe('disabled');
      expect(prior.spec.database).toBe(f.database);
      const enable = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '启用对象存储', exact: true }).click();
      await operation(await enable);
    }
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    const original = await context.request.get(
      base +
        `/storage/buckets/${source.bucket}/objects?` +
        new URLSearchParams({ key: source.source_key }),
    );
    expect(original.status()).toBe(200);
    const originalBytes = await original.body(),
      digest = original.headers()['etag'].replaceAll('"', '');
    if (!resume) {
      await page.getByLabel('存储桶名称', { exact: true }).fill(targetBucket);
      const created = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/storage/buckets' &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
      expect((await created).status()).toBe(201);
      await choose(source.bucket);
      const row = page
        .getByRole('row')
        .filter({ has: page.getByRole('cell', { name: source.source_key, exact: true }) });
      await row.getByRole('button', { name: '复制', exact: true }).click();
      await page.getByLabel('复制目标存储桶', { exact: true }).selectOption(targetBucket);
      await page.getByLabel('复制目标对象键', { exact: true }).fill(targetKey);
      const copied = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + `/storage/buckets/${targetBucket}/copy` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '确认复制对象', exact: true }).click();
      const reply = await copied;
      expect(reply.status()).toBe(201);
      expect((await reply.json()).sha256).toBe(digest);
      const target = await context.request.get(
        base +
          `/storage/buckets/${targetBucket}/objects?` +
          new URLSearchParams({ key: targetKey }),
      );
      expect(target.status()).toBe(200);
      expect(await target.body()).toEqual(originalBytes);
      await expect(page.getByRole('cell', { name: targetKey, exact: true })).toBeVisible();
      await save('ui_cross_bucket_copy_has_original_bytes_and_current_source_etag');
    } else {
      const target = await context.request.get(
        base +
          `/storage/buckets/${targetBucket}/objects?` +
          new URLSearchParams({ key: targetKey }),
      );
      expect(target.status()).toBe(200);
      expect(await target.body()).toEqual(originalBytes);
      await choose(targetBucket);
      await expect(page.getByRole('cell', { name: targetKey, exact: true })).toBeVisible();
    }
    // Negative requests retain the Console CSRF boundary and original source.
    async function negative(sourceSHA: string, key: string, conditional = true) {
      return page.evaluate(
        async ({ url, body, conditional }) => {
          const cookies = document.cookie.split(';').map((x) => x.trim());
          const pair = cookies.find((x) => x.startsWith('neon_v2_csrf='));
          const token = pair?.slice(pair.indexOf('=') + 1) ?? '';
          const response = await fetch(url, {
            method: 'POST',
            credentials: 'same-origin',
            headers: {
              'Content-Type': 'application/json',
              'X-CSRF-Token': decodeURIComponent(token),
              ...(conditional ? { 'If-None-Match': '*' } : {}),
            },
            body: JSON.stringify(body),
          });
          return response.status;
        },
        {
          url: base + `/storage/buckets/${targetBucket}/copy`,
          body: {
            source_bucket: source.bucket,
            source_key: source.source_key,
            source_sha256: sourceSHA,
            destination_key: key,
          },
          conditional,
        },
      );
    }
    let credential: { token_id: string; s3_secret_access_key: string };
    if (resume) {
      credential = resume.credential;
      const keys = await context.request.get(base + '/credentials');
      expect(keys.status()).toBe(200);
      const original = (await keys.json()).items.find(
        (item: { id: string }) => item.id === credential.token_id,
      );
      expect(original.state).toBe('active');
      expect(original.scopes).toContain('storage:write');
      // storage:write grants read transitively in the existing capability model.

      credentials.push(credential.token_id);
    } else {
      expect(await negative(digest, targetKey)).toBe(412);
      expect(await negative('0'.repeat(64), 'copies/stale.txt')).toBe(412);
      expect(await negative(digest, 'copies/unconditional.txt', false)).toBe(428);
      await save('duplicate_target_stale_source_and_missing_target_condition_denied');
      await page.goto(`/#/projects/${project}/credentials`);
      await page.getByLabel('凭据能力').selectOption('storage:write');
      await page.getByLabel('凭据名称').fill('COPY ' + targetBucket);
      const issued = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
      const credentialResponse = await issued;
      expect(credentialResponse.status()).toBe(201);
      credential = await credentialResponse.json();
      credentials.push(credential.token_id);
    }
    const config = path.join(privateDir, 'copy-sdk.private.json');
    await writeFile(
      config,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: credential.token_id,
        secret_key: credential.s3_secret_access_key,
        bucket: source.bucket,
        mode: 'copy',
      }),
      { mode: 0o600, flag: 'wx' },
    );
    if (!resume) {
      await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
      await save('ui_writer_key_saved_privately_before_signed_sdk_copy');
    }
    let output;
    try {
      output = await promisify(execFile)(probe, [config], { timeout: 190000, maxBuffer: 65536 });
    } catch (error) {
      const stdout = (error as { stdout?: string }).stdout;
      if (stdout) await writeFile(info.outputPath('sdk-result.json'), stdout);
      throw new Error('Original SDK COPY failed; sanitized SDK result retained');
    }

    const sdk = JSON.parse(output.stdout);
    expect(sdk.result).toBe('pass');
    for (const check of sdk.checks) await save(check);
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await choose(targetBucket);
    await expect(page.getByRole('cell', { name: targetKey, exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath('storage-copy.png'), fullPage: true });
    const retained = await context.request.get(
      base +
        `/storage/buckets/${source.bucket}/objects?` +
        new URLSearchParams({ key: source.source_key }),
    );
    expect(retained.status()).toBe(200);
    expect(await retained.body()).toEqual(originalBytes);
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
    await save('original_and_copied_data_retained_owned_runtime_disabled_compute_zero');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
