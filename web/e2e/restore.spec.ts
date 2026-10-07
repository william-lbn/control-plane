import { expect, test, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Opt-in real storage/SQL acceptance. No secret enters screenshots or reports.
test('UI historical restore: timestamp, LSN, catalog isolation and cold wake', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  // Bound selector failures independently of the long storage acceptance run.
  page.setDefaultTimeout(40_000);
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!baseURL || !privateDir || !adminFile)
    throw new Error('Protected Linux E2E configuration required');
  const protectedRoot = privateDir;
  const attempt = process.env.NEON_E2E_ATTEMPT || new Date().toISOString().replace(/[^0-9]/g, '');
  if (!/^[a-z0-9_-]+$/.test(attempt)) throw new Error('Invalid attempt');
  const password = randomBytes(30).toString('base64url');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    mode: 0o600,
    flag: 'wx',
  });
  const checks: Record<string, unknown>[] = [];
  const endpoints = new Set<string>();
  const operations: Record<string, unknown>[] = [];
  let project = '';
  let result = 'running';
  let faultActive = false;
  let failedReads = 0;
  let restorePosts = 0;
  async function save() {
    await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        { result, project_id: project, checks, operations, credentials_in_report: false },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(protectedRoot, attempt + '-fixture.json'),
      JSON.stringify({ project_id: project, endpoints: [...endpoints] }, null, 2) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await save();
  }
  async function shot(name: string) {
    await page.screenshot({
      path: testInfo.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  page.on('request', (r) => {
    if (
      faultActive &&
      r.method() === 'POST' &&
      new URL(r.url()).pathname === `/api/v1/projects/${project}/branches`
    )
      restorePosts++;
  });
  if (process.env.NEON_E2E_POLL_FAULT === 'true')
    await page.route('**/api/v1/projects/*/operations/*', async (route) => {
      if (faultActive && route.request().method() === 'GET' && failedReads < 2) {
        failedReads++;
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({
            code: 'metadata_unavailable',
            message: 'Controlled restore observation outage',
          }),
        });
      } else await route.continue();
    });
  async function get(route: string) {
    const r = await context.request.get(baseURL! + route);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function accepted(response: Response) {
    expect(response.status()).toBe(202);
    const accepted = await response.json();
    if (String(accepted.resource.id).startsWith('prj_')) project = accepted.resource.id;
    operations.push({ id: accepted.operation.id, resource: accepted.resource.id });
    await save();
    await expect(page.locator('.create-modal')).toBeHidden({ timeout: 490_000 });
    return {
      ...accepted,
      replayBody: response.request().postDataJSON(),
      replayKey: response.request().headers()['idempotency-key'],
    };
  }
  async function submit(route: string) {
    const r = page.waitForResponse(
      (r) => new URL(r.url()).pathname === route && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    return accepted(await r);
  }
  async function sql(endpoint: string, query: string) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(query);
    await page.locator('input[type="password"]').fill(password);
    const r = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${endpoint}/query`) && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const done = await r;
    expect(done.status(), 'UI SQL via Proxy').toBe(200);
    return done.json();
  }
  async function suspend(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    const r = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${endpoint}/suspend`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    expect((await r).status()).toBe(202);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145_000,
    });
  }
  async function restore(parent: string, kind: 'timestamp' | 'lsn', point: string, name: string) {
    await page.goto(`/#/projects/${project}/restore`);
    await page.getByLabel('恢复源分支').selectOption(parent);
    await page.getByRole('button', { name: '恢复到新分支', exact: true }).click();
    await page.getByLabel('分支名称').fill(name);
    await page.getByLabel('分支起点').selectOption(kind);
    await page
      .getByLabel(kind === 'timestamp' ? '恢复时间' : '恢复 LSN', { exact: true })
      .fill(point);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    return submit(`/api/v1/projects/${project}/branches`);
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
    await page.getByLabel('项目名称').fill('restore-' + attempt);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    project = (await submit('/api/v1/organizations/local/projects')).resource.id;
    const root = (await get(`/api/v1/projects/${project}/endpoints`)).items[0];
    endpoints.add(root.id);
    expect((await get('/api/v1/capabilities')).features.pitr_new_branch.enabled).toBe(true);
    await sql(
      root.id,
      'CREATE TABLE public.restore_receipt(id integer PRIMARY KEY,marker text NOT NULL)',
    );
    await sql(root.id, "INSERT INTO public.restore_receipt VALUES(1,'before')");
    const point = (
      await sql(
        root.id,
        `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),pg_current_wal_flush_lsn()::text`,
      )
    ).rows[0];
    const timestamp = String(point[0]);
    const lsn = String(point[1]);
    await sql(root.id, "UPDATE public.restore_receipt SET marker='after'");
    await sql(root.id, 'CREATE TABLE public.created_after_restore(id integer)');
    // Create managed catalog intent after the point: history must not inherit it.
    await page.goto(`/#/projects/${project}/databases`);
    await page.getByLabel('管理分支').selectOption(root.branch_id);
    await page.getByLabel('新角色名称').fill('role_after_restore');
    await page.getByLabel('新角色密码').fill(randomBytes(24).toString('base64url'));
    await page.getByRole('button', { name: '创建角色', exact: true }).click();
    await expect(page.locator('.catalog-operation strong')).toHaveText('操作状态：succeeded', {
      timeout: 190_000,
    });
    await page.getByLabel('新数据库名称').fill('database_after_restore');
    await page.getByLabel('数据库 Owner').selectOption('role_after_restore');
    await page.getByRole('button', { name: '创建数据库', exact: true }).click();
    await expect(page.locator('.catalog-operation strong')).toHaveText('操作状态：succeeded', {
      timeout: 190_000,
    });
    await suspend(root.id);
    await record('source_writes_and_catalog_after_target_retained', {
      source_branch: root.branch_id,
      source_endpoint: root.id,
      target_timestamp: timestamp,
      target_lsn: lsn,
    });
    const history = await get(
      `/api/v1/projects/${project}/branches/${root.branch_id}/restore-window`,
    );
    expect(history.min_readable_lsn).toBeTruthy();
    const idle = (await get(`/api/v1/projects/${project}/endpoints`)).items.find(
      (e: { id: string }) => e.id === root.id,
    );
    expect(idle.observed_state).toBe('suspended');
    await record('restore_window_does_not_wake_compute');
    faultActive = true;
    const restored = await restore(root.branch_id, 'timestamp', timestamp, 'timestamp-' + attempt);
    faultActive = false;
    const branch = restored.resource.id;
    const writer = (await get(`/api/v1/projects/${project}/endpoints`)).items.find(
      (e: { branch_id: string }) => e.branch_id === branch,
    );
    endpoints.add(writer.id);
    expect(restored.resource.restore_source).toBe('timestamp');
    expect(restored.resource.parent_timestamp).toBeTruthy();
    const op = await get(`/api/v1/projects/${project}/operations/${restored.operation.id}`);
    expect(op.state).toBe('succeeded');
    expect(op.steps[0].name).toBe('pin_restore_point');
    await record('timestamp_restore_native_timeline_ready', {
      branch,
      endpoint: writer.id,
      parent_lsn: restored.resource.parent_lsn,
    });
    if (process.env.NEON_E2E_POLL_FAULT === 'true') {
      expect(failedReads).toBe(2);
      expect(restorePosts).toBe(1);
      await record('restore_observation_outage_does_not_repeat_mutation', {
        failed_reads: failedReads,
        mutation_count: restorePosts,
      });
    }
    expect(
      (await sql(writer.id, 'SELECT marker FROM public.restore_receipt WHERE id=1')).rows,
    ).toEqual([['before']]);
    expect(
      (
        await sql(
          writer.id,
          "SELECT to_regclass('public.created_after_restore') IS NULL, NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='role_after_restore'), NOT EXISTS(SELECT 1 FROM pg_database WHERE datname='database_after_restore')",
        )
      ).rows,
    ).toEqual([[true, true, true]]);
    await record('historical_data_schema_roles_and_databases_not_overwritten_by_current_catalog');
    await shot('restored-historical-query');
    await sql(writer.id, "INSERT INTO public.restore_receipt VALUES(2,'restored-only')");
    await suspend(writer.id);
    expect((await sql(writer.id, 'SELECT count(*) FROM public.restore_receipt')).rows).toEqual([
      [2],
    ]);
    await record('restored_endpoint_suspend_and_proxy_cold_wake_preserves_data');
    await suspend(writer.id);
    expect(
      (await sql(root.id, 'SELECT marker,count(*) OVER() FROM public.restore_receipt')).rows,
    ).toEqual([['after', 1]]);
    await record('restored_branch_changes_do_not_modify_source');
    await suspend(root.id);
    const restoredLSN = await restore(root.branch_id, 'lsn', lsn, 'lsn-' + attempt);
    const writerLSN = (await get(`/api/v1/projects/${project}/endpoints`)).items.find(
      (e: { branch_id: string }) => e.branch_id === restoredLSN.resource.id,
    );
    endpoints.add(writerLSN.id);
    expect(restoredLSN.resource.restore_source).toBe('lsn');
    expect((await sql(writerLSN.id, 'SELECT marker FROM public.restore_receipt')).rows).toEqual([
      ['before'],
    ]);
    await record('explicit_lsn_restores_same_historical_data', {
      branch: restoredLSN.resource.id,
      endpoint: writerLSN.id,
    });
    await suspend(writerLSN.id);
    await page.goto(`/#/projects/${project}/restore`);
    await expect(page.locator('.table-card')).toContainText('timestamp-' + attempt);
    await expect(page.locator('.table-card')).toContainText('lsn-' + attempt);
    await shot('restore-history');
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('CSRF cookie required');
    const replay = await context.request.post(baseURL + `/api/v1/projects/${project}/branches`, {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': restored.replayKey },
      data: restored.replayBody,
    });
    expect(replay.status()).toBe(202);
    const replayed = await replay.json();
    expect(replayed.resource.id).toBe(branch);
    expect(replayed.operation.id).toBe(restored.operation.id);
    expect(replayed.resource.parent_lsn).toBe(restored.resource.parent_lsn);
    await record('restore_replay_preserves_operation_branch_and_fixed_point');
    const before = (await get(`/api/v1/projects/${project}/branches`)).items.length;
    const invalid = await context.request.post(baseURL + `/api/v1/projects/${project}/branches`, {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': 'outside-' + attempt },
      data: {
        name: 'outside-' + attempt,
        parent_branch_id: root.branch_id,
        parent_timestamp: '2000-01-01T00:00:00Z',
        create_endpoint: false,
      },
    });
    expect(invalid.status()).toBe(422);
    expect((await invalid.json()).code).toBe('restore_point_outside_history');
    expect((await get(`/api/v1/projects/${project}/branches`)).items.length).toBe(before);
    await record('outside_history_rejected_without_accepting_branch');
    const all = (await get(`/api/v1/projects/${project}/endpoints`)).items;
    expect(all.every((e: { observed_state: string }) => e.observed_state === 'suspended')).toBe(
      true,
    );
    await record('all_test_compute_suspended_data_and_evidence_retained');
    result = 'pass';
  } finally {
    if (result === 'running') result = 'fail';
    await save();
  }
});
