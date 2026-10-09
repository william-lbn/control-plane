import { expect, test } from '@playwright/test';
import { generateKeyPairSync, sign } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

// Continue a specifically identified failed fixture. The original ephemeral
// provider key was deliberately not persisted; rotate the test issuer via UI.
// Never create another project, rewrite the failed receipt or purge user data.
test('Data API retained failure: original data, issuer rotation and cold wake', async ({
  page,
  context,
  baseURL,
}, info) => {
  const input = process.env.NEON_E2E_DATA_API_RECOVERY_FIXTURE;
  const admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!input || !admin || !baseURL) throw new Error('Protected Linux recovery inputs required');
  const f = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    project_name: string;
    branch_id: string;
    writer_id: string;
    original_failure_job: string;
    database_password_file: string;
  };
  if (
    !/^ci-publication-ui-\d+-dataapi$/.test(f.project_name) ||
    !/^publication-ui-\d+$/.test(f.original_failure_job) ||
    f.project_name !== `ci-${f.original_failure_job}-dataapi` ||
    !/^prj_[a-f0-9]{16}$/.test(f.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(f.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(f.writer_id)
  )
    throw new Error('Original failed fixture identity is invalid');
  const prefix = `/api/v1/projects/${f.project_id}`;
  const servicePath = `${prefix}/branches/${f.branch_id}/data-api`;
  const checks: string[] = [];
  let result = 'running';
  const save = () =>
    writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: f.project_id,
          branch_id: f.branch_id,
          writer_id: f.writer_id,
          original_failure_job: f.original_failure_job,
          original_attempt_unchanged: true,
          created_new_project: false,
          credentials_in_evidence: false,
          physical_gc: false,
          checks,
        },
        null,
        2,
      ) + '\n',
    );
  const record = async (name: string) => {
    checks.push(name);
    await save();
  };
  async function metadata(path: string) {
    const r = await context.request.get(path);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function service(disable: boolean) {
    await page.goto(`/#/projects/${f.project_id}/data-api`);
    await page.getByLabel('Data API 分支', { exact: true }).selectOption(f.branch_id);
    if (!disable) {
      await page.getByLabel('JWT Issuer', { exact: true }).fill('https://identity.example.test');
      await page.getByLabel('JWT Audience', { exact: true }).fill(f.branch_id);
      await page.getByLabel('Provider JWKS', { exact: true }).fill(JSON.stringify({ keys: [jwk] }));
    }
    const pending = page.waitForResponse(
      (r) =>
        r.url().endsWith(servicePath) && r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '停用 Data API' : '启用 Data API', exact: true })
      .click();
    const accepted = await pending;
    expect(accepted.status()).toBe(202);
    const op = (await accepted.json()).operation.id;
    await expect(page.getByTestId('data-api-operation')).toContainText(op);
    await expect(page.getByTestId('data-api-operation')).toContainText('succeeded', {
      timeout: 490000,
    });
  }
  async function suspend() {
    await page.goto(`/#/projects/${f.project_id}/compute`);
    await page.locator('select').first().selectOption(f.writer_id);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
  }
  const keys = generateKeyPairSync('ed25519');
  const jwk = {
    ...keys.publicKey.export({ format: 'jwk' }),
    alg: 'EdDSA',
    kid: 'explicit-failed-fixture-recovery',
    use: 'sig',
  };
  function token() {
    const header = Buffer.from(JSON.stringify({ alg: 'EdDSA', kid: jwk.kid })).toString(
      'base64url',
    );
    const claims = Buffer.from(
      JSON.stringify({
        iss: 'https://identity.example.test',
        aud: f.branch_id,
        sub: 'alice',
        exp: Math.floor(Date.now() / 1000) + 3600,
      }),
    ).toString('base64url');
    const message = `${header}.${claims}`;
    return message + '.' + sign(null, Buffer.from(message), keys.privateKey).toString('base64url');
  }
  async function applicationRead() {
    await page.goto(`/#/projects/${f.project_id}/data-api`);
    await page.getByLabel('应用 JWT', { exact: true }).fill(token());
    await page.getByLabel('Data API 方法').selectOption('GET');
    await page.getByRole('button', { name: '发送 Data API 请求', exact: true }).click();
    await expect(page.getByTestId('data-api-response')).toContainText('HTTP 200', {
      timeout: 190000,
    });
    await expect(page.getByTestId('data-api-response')).toContainText('accepted');
    await expect(page.getByTestId('data-api-response')).not.toContainText('bob private');
    await page.getByLabel('应用 JWT', { exact: true }).fill('');
  }
  async function lifecycle(button: string, name: string, branch = '') {
    await page
      .getByTestId(branch ? 'lifecycle-' + branch : 'project-lifecycle')
      .getByRole('button', { name: button, exact: true })
      .click();
    await page.getByLabel('资源名称确认').fill(name);
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    await expect(page.getByRole('dialog', { name: '确认生命周期操作' })).toHaveCount(0, {
      timeout: 490000,
    });
  }
  await save();
  try {
    await page.goto('/');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const before = await metadata(prefix + '/lifecycle');
    expect(before.project.name).toBe(f.project_name);
    expect(before.project.source).toBe('managed');
    expect(before.project.state).toBe('ready');
    expect(before.branches.map((b: { id: string }) => b.id)).toEqual([f.branch_id]);
    const endpoints = (await metadata(prefix + '/endpoints')).items;
    expect(endpoints.map((e: { id: string }) => e.id)).toEqual([f.writer_id]);
    expect(endpoints[0].branch_id).toBe(f.branch_id);
    await page.goto(`/#/projects/${f.project_id}/query`);
    await page.locator('select').first().selectOption(f.writer_id);
    await page
      .getByLabel('SQL 查询')
      .fill('SELECT id,owner_id,body FROM app_data.notes ORDER BY id');
    await page
      .locator('input[type="password"]')
      .fill((await readFile(f.database_password_file, 'utf8')).trim());
    const queried = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${f.writer_id}/query`) && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const rows = await queried;
    expect(rows.status()).toBe(200);
    const retained = JSON.stringify(await rows.json());
    expect(retained).toContain('alice private');
    expect(retained).toContain('bob private');
    expect(retained).toContain('accepted');
    await record('ui_original_failed_fixture_sql_rows_retained_without_new_project');
    const old = await metadata(servicePath);
    expect(old.state).toBe('active');
    await service(true);
    await service(false);
    const fresh = await metadata(servicePath);
    expect(fresh.generation).toBeGreaterThan(old.generation);
    await applicationRead();
    await record('ui_explicit_test_issuer_rotation_preserves_rls_rows_and_isolation');
    await suspend();
    await applicationRead();
    await record('ui_original_data_api_cold_wakes_compute_after_manual_zero');
    await page.goto(`/#/projects/${f.project_id}/compute`);
    await page.locator('select').first().selectOption(f.writer_id);
    await page.getByRole('checkbox').check();
    await page.getByRole('spinbutton').fill('60');
    const policy = page.waitForResponse(
      (r) =>
        r.url().endsWith(`/endpoints/${f.writer_id}/lifecycle`) && r.request().method() === 'PATCH',
    );
    await page.getByRole('button', { name: '保存生命周期策略', exact: true }).click();
    expect((await policy).status()).toBe(200);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 240000,
    });
    await applicationRead();
    await record('ui_original_data_api_automatic_zero_and_first_read_cold_wake');
    await service(true);
    await suspend();
    await page.goto(`/#/projects/${f.project_id}/lifecycle`);
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
    if (before.project.protected) await lifecycle('解除项目保护', f.project_name);
    if (before.branches[0].protected) await lifecycle('解除分支保护', 'main', f.branch_id);
    await lifecycle('删除项目', f.project_name);
    const after = await metadata(prefix + '/lifecycle');
    expect(after.project.state).toBe('deleted');
    expect(after.physical_gc_enabled).toBe(false);
    expect(
      after.tombstones.some((v: { physical_gc_state: string }) => v.physical_gc_state === 'held'),
    ).toBe(true);
    await record('ui_original_fixture_services_stopped_data_retained_and_quota_released');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    throw error;
  } finally {
    await save();
  }
});
