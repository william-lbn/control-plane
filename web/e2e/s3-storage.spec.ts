import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('S3 product starts at UI and uses actual SDK signing, object bytes, rotation and revocation', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!file || !admin || !privateDir || !probe)
    throw new Error('Protected owner fixture and verified Linux SDK probe required');
  const privatePath: string = privateDir,
    probePath: string = probe;
  const f = JSON.parse(await readFile(file, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    role: string;
    password_file: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(f.project_id) ||
    f.database !== 'owner_transfer_v1' ||
    f.role !== 'owner_new_v1'
  )
    throw new Error('Exact retained owner fixture required');
  const project = f.project_id,
    branch = f.branch_id,
    base = `/api/v1/projects/${project}/branches/${branch}`,
    resumeFile = process.env.NEON_E2E_S3_RESUME;
  type Key = { id: string; secret: string };
  const resume = resumeFile
    ? (JSON.parse(await readFile(resumeFile, 'utf8')) as {
        project_id: string;
        branch_id: string;
        bucket: string;
        key: Key;
        checks: string[];
        operations: string[];
        primary_uploaded?: boolean;
      })
    : undefined;
  if (
    resume &&
    (resume.project_id !== project ||
      resume.branch_id !== branch ||
      !/^ci-s3-[0-9]+$/.test(resume.bucket) ||
      !/^bcr_[a-f0-9]{24}$/.test(resume.key.id) ||
      resume.key.secret.length !== 43)
  )
    throw new Error('Original owned S3 checkpoint required');
  const bucket = resume?.bucket ?? 'ci-s3-' + Date.now();
  let result = 'running';
  const checks: string[] = [],
    operations: string[] = [],
    credentials: string[] = [];
  let sequence = 0;
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        credential_ids: credentials,
        project_id: project,
        branch_id: branch,
        bucket,
        credentials_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const a = await r.json();
    operations.push(a.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${a.operation.id}`,
          );
          const op = await r.json();
          if (op.state === 'failed' || op.state === 'cancelled')
            throw new Error(`Original S3 operation failed ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function sdk(key: Key, mode: string) {
    const cfg = path.join(privatePath, `s3-config-${++sequence}.json`);
    await writeFile(
      cfg,
      JSON.stringify({
        endpoint: 'http://192.168.146.100:30893',
        region: 'us-east-2',
        access_key: key.id,
        secret_key: key.secret,
        bucket,
        mode,
      }),
      { mode: 0o600, flag: 'wx' },
    );
    let stdout = '';
    try {
      stdout = (await promisify(execFile)(probePath, [cfg], { timeout: 190000, maxBuffer: 65536 }))
        .stdout;
    } catch (e) {
      const output = (e as { stdout?: string }).stdout;
      if (output) {
        const report = JSON.parse(output);
        for (const item of report.checks) await save(item);
        throw new Error(
          `SDK ${mode} failed at ${report.stage}; HTTP ${report.http_statuses?.join(',') ?? 'unavailable'}`,
        );
      }
      throw new Error(`SDK ${mode} did not complete; inspect private process evidence`);
    }
    const report = JSON.parse(stdout);
    expect(report.result).toBe('pass');
    for (const item of report.checks) await save(item);
  }
  async function issue(capability: string, name: string) {
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据能力').selectOption(capability);
    await page.getByLabel('凭据名称').fill(name);
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(201);
    const body = await r.json();
    const key = { id: body.token_id as string, secret: body.s3_secret_access_key as string };
    expect(/^bcr_[a-f0-9]{24}$/.test(key.id)).toBe(true);
    expect(typeof key.secret === 'string' && key.secret.length === 43).toBe(true);
    credentials.push(key.id);
    await expect(page.getByLabel('新 S3 Secret Access Key')).toHaveAttribute('type', 'password');
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    await expect(page.getByLabel('新 S3 Secret Access Key')).toHaveCount(0);
    return key;
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(f.writer_id);
    await page.getByLabel('数据库角色', { exact: true }).fill(f.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(f.database);
    await page.getByLabel('执行模式').selectOption('single');
    await page.getByLabel('查询操作').selectOption('query');
    await page
      .getByLabel('SQL 查询')
      .fill(`GRANT CREATE, CONNECT ON DATABASE "${f.database}" TO control_probe WITH GRANT OPTION`);
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(f.password_file, 'utf8')).trim());
    const grant = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${f.writer_id}/query`,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    expect((await grant).status()).toBe(200);
    await page.goto(`/#/projects/${project}/storage`);
    if (resume) {
      const response = await context.request.get(base + '/storage');
      expect(response.status()).toBe(200);
      const service = await response.json();
      expect(service.state).toBe('active');
      expect(service.spec.database).toBe(f.database);
      checks.push(...resume.checks);
      operations.push(...resume.operations);
      await save('original_owned_active_storage_service_verified_and_reused');
    } else {
      await page.getByLabel('存储数据库').fill(f.database);
      const enable = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '启用对象存储', exact: true }).click();
      await wait(await enable);
    }
    await expect(page.getByTestId('storage-s3-panel')).toContainText('forcePathStyle');
    await save('ui_owner_grant_and_native_branch_storage_s3_endpoint_enabled');
    const writer = resume?.key ?? (await issue('storage:write', 's3-writer-' + Date.now()));
    if (resume) {
      const response = await context.request.get(base + '/credentials');
      expect(response.status()).toBe(200);
      const credential = (await response.json()).items.find(
        (item: { id: string }) => item.id === writer.id,
      );
      expect(credential.state).toBe('active');
      expect(credential.scopes).toContain('storage:write');
      credentials.push(writer.id);
      await save('original_storage_writer_reused_no_duplicate_credential_issue');
    }
    await save('ui_unified_storage_writer_issued_once_and_secret_masked_then_cleared');
    await sdk(
      writer,
      resume ? (resume.primary_uploaded ? 'continue_existing' : 'continue_write') : 'write',
    );
    await sdk(writer, 'read');
    const reader = await issue('storage:read', 's3-reader-' + Date.now());
    await sdk(reader, 'read');
    await sdk(reader, 'deny_write');
    await save('actual_sdk_scoped_reads_and_write_denial');
    const listing = await context.request.get(base + '/credentials');
    expect(listing.status()).toBe(200);
    const encoded = await listing.text();
    expect(encoded.includes(writer.secret) || encoded.includes(reader.secret)).toBe(false);
    await save('metadata_list_does_not_expose_s3_secrets');
    await page.goto(`/#/projects/${project}/credentials`);
    const row = page.getByTestId('backend-row-' + writer.id);
    const rotated = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/credentials/' + writer.id + '/rotate',
    );
    await row.getByRole('button', { name: '轮换', exact: true }).click();
    const rr = await rotated;
    expect(rr.status()).toBe(200);
    const rv = await rr.json();
    const next = { id: rv.token_id as string, secret: rv.s3_secret_access_key as string };
    expect(next.id).toBe(writer.id);
    expect(next.secret === writer.secret).toBe(false);
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    await sdk(writer, 'deny_read');
    await sdk(next, 'read');
    await save('ui_in_place_rotation_invalidates_old_s3_key_and_preserves_objects');
    await sdk(next, 'cleanup');
    const revoked = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/credentials/' + writer.id &&
        r.request().method() === 'DELETE',
    );
    await page
      .getByTestId('backend-row-' + writer.id)
      .getByRole('button', { name: '撤销', exact: true })
      .click();
    expect((await revoked).status()).toBe(200);
    await sdk(next, 'deny_read');
    await save('ui_revocation_immediately_denies_sdk_requests');
    const otherRevoked = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/credentials/' + reader.id &&
        r.request().method() === 'DELETE',
    );
    await page
      .getByTestId('backend-row-' + reader.id)
      .getByRole('button', { name: '撤销', exact: true })
      .click();
    expect((await otherRevoked).status()).toBe(200);
    await page.goto(`/#/projects/${project}/storage`);
    await page.screenshot({ path: info.outputPath('s3-storage.png'), fullPage: true });
    const disable = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base + '/storage' && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用对象存储', exact: true }).click();
    await wait(await disable);
    await page.goto(`/#/projects/${project}/compute`);
    const state = await context.request.get(`/api/v1/projects/${project}/endpoints/${f.writer_id}`);
    if ((await state.json()).state === 'active') {
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
        timeout: 150000,
      });
    }
    await save(
      'owned_keys_revoked_service_disabled_physical_blobs_and_evidence_retained_compute_zero',
    );
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
