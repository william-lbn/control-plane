import { expect, test, type Response } from '@playwright/test';
import { generateKeyPairSync, randomBytes, sign } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Real Console -> native Driver -> branch gateway -> PostgREST -> Neon Proxy.
// The provider private key is ephemeral and never written to evidence or Git.
test('Data API: native UI, RLS, identity isolation, disable and cold wake', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!adminFile || !privateDir || !baseURL)
    throw new Error('Linux live E2E configuration required');
  const privateRoot = privateDir;
  const attempt = `${process.env.NEON_E2E_ATTEMPT || Date.now()}-dataapi`;
  const password = randomBytes(30).toString('base64url');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    mode: 0o600,
    flag: 'wx',
  });
  const keys = generateKeyPairSync('ed25519');
  const provider = {
    ...keys.publicKey.export({ format: 'jwk' }),
    alg: 'EdDSA',
    kid: 'live-provider',
    use: 'sig',
  };
  const issuer = 'https://identity.example.test';
  const checks: { name: string; [key: string]: unknown }[] = [];
  let project = '';
  let writer = '';
  let branch = '';
  let result = 'running';
  const pollFault = process.env.NEON_E2E_POLL_FAULT === 'true';
  let faultArmed = false;
  let injectedReads = 0;
  let enableMutations = 0;
  if (pollFault) {
    page.on('request', (request) => {
      if (
        faultArmed &&
        request.method() === 'POST' &&
        new URL(request.url()).pathname.endsWith('/data-api')
      )
        enableMutations++;
    });
    await page.route('**/api/v1/projects/*/operations/*', async (route) => {
      if (
        faultArmed &&
        route.request().method() === 'GET' &&
        /^\/api\/v1\/projects\/[^/]+\/operations\/[^/]+$/.test(
          new URL(route.request().url()).pathname,
        ) &&
        injectedReads < 2
      ) {
        injectedReads++;
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({
            code: 'metadata_unavailable',
            message: 'Controlled observation outage',
          }),
        });
      } else await route.continue();
    });
  }
  async function save() {
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          writer_id: writer,
          branch_id: branch,
          checks,
          tokens_or_credentials_in_evidence: false,
        },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(privateRoot, attempt + '-fixture.json'),
      JSON.stringify({ project_id: project, writer_id: writer, branch_id: branch }, null, 2) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await save();
  }
  function token(subject: string, changes: Record<string, unknown> = {}) {
    const header = Buffer.from(
      JSON.stringify({ alg: 'EdDSA', kid: 'live-provider', typ: 'JWT' }),
    ).toString('base64url');
    const claims = Buffer.from(
      JSON.stringify({
        iss: issuer,
        aud: branch,
        sub: subject,
        exp: Math.floor(Date.now() / 1000) + 3600,
        ...changes,
      }),
    ).toString('base64url');
    const message = `${header}.${claims}`;
    return message + '.' + sign(null, Buffer.from(message), keys.privateKey).toString('base64url');
  }
  async function sql(statement: string) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(writer);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const response = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${writer}/query`) && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    expect((await response).status()).toBe(200);
  }
  async function service(disable: boolean) {
    const pending = page.waitForResponse(
      (r) =>
        r.url().endsWith(`/branches/${branch}/data-api`) &&
        r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '停用 Data API' : '启用 Data API', exact: true })
      .click();
    const accepted: Response = await pending;
    expect(accepted.status()).toBe(202);
    const body = await accepted.json();
    await expect(page.getByTestId('data-api-operation')).toContainText(
      /succeeded|failed|cancelled/,
      {
        timeout: 490000,
      },
    );
    await record('ui_observed_data_api_operation', { operation_id: body.operation.id });
    await expect(page.getByTestId('data-api-operation')).toContainText('succeeded');
    await record(disable ? 'ui_disabled_preserving_data' : 'ui_enabled_native_service', {
      operation_id: body.operation.id,
    });
    return accepted;
  }
  async function request(applicationToken: string, expectedStatus: number, expectedText?: string) {
    await page.getByLabel('应用 JWT', { exact: true }).fill(applicationToken);
    await page.getByLabel('Data API 方法').selectOption('GET');
    await page.getByRole('button', { name: '发送 Data API 请求', exact: true }).click();
    await expect(page.getByTestId('data-api-response')).toContainText(`HTTP ${expectedStatus}`, {
      timeout: 190000,
    });
    if (expectedText)
      await expect(page.getByTestId('data-api-response')).toContainText(expectedText);
  }
  await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
    await page.getByLabel('项目名称').fill('ci-' + attempt);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const creation = page.waitForResponse(
      (r) => r.url().endsWith('/organizations/local/projects') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const created = await creation;
    expect(created.status()).toBe(202);
    project = (await created.json()).resource.id;
    await expect(page.locator('.create-modal')).toHaveCount(0, { timeout: 490000 });
    const listed = await context.request.get(`/api/v1/projects/${project}/endpoints`);
    expect(listed.status()).toBe(200);
    const endpoint = (await listed.json()).items[0];
    writer = endpoint.id;
    branch = endpoint.branch_id;
    await record('ui_created_retained_neon_project');
    for (const statement of [
      'GRANT CREATE, CONNECT ON DATABASE postgres TO control_probe WITH GRANT OPTION',
      'CREATE SCHEMA app_data',
      'CREATE TABLE app_data.notes(id text PRIMARY KEY, owner_id text NOT NULL, body text NOT NULL)',
      'ALTER TABLE app_data.notes ENABLE ROW LEVEL SECURITY',
      'ALTER TABLE app_data.notes FORCE ROW LEVEL SECURITY',
      'GRANT USAGE ON SCHEMA app_data TO control_probe WITH GRANT OPTION',
      'GRANT SELECT, INSERT, UPDATE, DELETE ON app_data.notes TO control_probe WITH GRANT OPTION',
      "CREATE POLICY owner_rows ON app_data.notes USING (owner_id = current_setting('request.jwt.claims',true)::jsonb->>'sub') WITH CHECK (owner_id = current_setting('request.jwt.claims',true)::jsonb->>'sub')",
      "INSERT INTO app_data.notes VALUES ('alice-1','alice','alice private'),('bob-1','bob','bob private')",
    ])
      await sql(statement);
    await record('ui_prepared_application_rls_schema');
    await page.goto(`/#/projects/${project}/data-api`);
    await page.getByLabel('JWT Issuer', { exact: true }).fill(issuer);
    await page.getByLabel('JWT Audience', { exact: true }).fill(branch);
    await page
      .getByLabel('Provider JWKS', { exact: true })
      .fill(JSON.stringify({ keys: [provider] }));
    await page.getByLabel('Data API 允许来源', { exact: true }).fill('https://app.example.test');
    faultArmed = pollFault;
    const accepted = await service(false);
    if (pollFault) {
      expect(injectedReads).toBe(2);
      expect(enableMutations).toBe(1);
      await record('ui_data_api_recovers_transient_observation_without_repeating_enable', {
        injected_reads: injectedReads,
        enable_mutations: enableMutations,
      });
    }
    faultArmed = false;
    // Replay the exact UI request: neither another service nor credentials
    // may be created; reusing its key with changed input must fail.
    const headers = accepted.request().headers();
    const replay = await context.request.post(
      `/api/v1/projects/${project}/branches/${branch}/data-api`,
      {
        headers: {
          'X-CSRF-Token': headers['x-csrf-token'],
          'Idempotency-Key': headers['idempotency-key'],
          'If-Match': headers['if-match'],
        },
        data: accepted.request().postDataJSON(),
      },
    );
    expect(replay.status()).toBe(202);
    expect((await replay.json()).operation.id).toBe((await accepted.json()).operation.id);
    await record('ui_request_idempotency_replayed_same_operation');
    await request(token('alice'), 200, 'alice private');
    await expect(page.getByTestId('data-api-response')).not.toContainText('bob private');
    await request(token('bob'), 200, 'bob private');
    await expect(page.getByTestId('data-api-response')).not.toContainText('alice private');
    await record('ui_rls_isolates_provider_subjects');
    await request(token('alice', { aud: 'other-branch' }), 401);
    await request(token('alice', { exp: Math.floor(Date.now() / 1000) - 60 }), 401);
    await request(token('alice', { role: 'cloud_admin' }), 401);
    await record('ui_rejects_wrong_audience_expiry_and_privileged_role');
    await page.getByLabel('应用 JWT', { exact: true }).fill(token('alice'));
    await page.getByLabel('Data API 方法').selectOption('POST');
    await page
      .getByLabel('Data API Body')
      .fill(JSON.stringify({ id: 'forged', owner_id: 'bob', body: 'forged' }));
    await page.getByRole('button', { name: '发送 Data API 请求', exact: true }).click();
    await expect(page.getByTestId('data-api-response')).toContainText('HTTP 403');
    await expect(page.getByTestId('data-api-response')).toContainText('42501');
    await page
      .getByLabel('Data API Body')
      .fill(JSON.stringify({ id: 'alice-2', owner_id: 'alice', body: 'accepted' }));
    await page.getByRole('button', { name: '发送 Data API 请求', exact: true }).click();
    await expect(page.getByTestId('data-api-response')).toContainText('HTTP 201');
    await record('ui_enforces_rls_for_writes');
    await service(true);
    const stopped = await context.request.get(`/data/v1/${branch}/notes`, {
      headers: { Authorization: `Bearer ${token('alice')}` },
    });
    expect(stopped.status()).toBe(404);
    await service(false);
    await expect(page.getByLabel('Data API 允许来源', { exact: true })).toHaveValue(
      'https://app.example.test',
    );
    await request(token('alice'), 200, 'accepted');
    await record('ui_reenable_rotates_runtime_credentials_and_preserves_rows');
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
    await record('ui_suspend_with_idle_data_api_pool');
    await page.goto(`/#/projects/${project}/data-api`);
    await request(token('alice'), 200, 'accepted');
    await record('ui_data_api_cold_wakes_real_neon_compute');
    // Configure the real controller in the UI, then poll runtime only. No
    // SQL/Data API traffic is sent until an automatic 1 -> 0 is observed.
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    await page.getByRole('checkbox').check();
    await page.getByRole('spinbutton').fill('60');
    const lifecycle = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${writer}/lifecycle`) && r.request().method() === 'PATCH',
    );
    await page.getByRole('button', { name: '保存生命周期策略', exact: true }).click();
    expect((await lifecycle).status()).toBe(200);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 240000,
    });
    await record('ui_auto_suspend_with_data_api_enabled_and_no_traffic');
    await page.goto(`/#/projects/${project}/data-api`);
    await request(token('alice'), 200, 'accepted');
    await record('ui_data_api_cold_wake_after_automatic_idle');
    await page.screenshot({
      path: testInfo.outputPath('data-api.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
    await service(true);
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
    await record('ui_test_runtime_stopped_database_and_evidence_retained');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    // Evidence contains only identifiers and typed assertions, never provider
    // keys, cookies, authorization values, database passwords or request bodies.
    await save();
    throw error;
  } finally {
    await save();
  }
});
