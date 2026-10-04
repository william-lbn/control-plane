import { expect, test, type APIResponse, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

type Accepted = {
  resource: { id: string; project_id?: string };
  operation: { id: string };
};
type Endpoint = { id: string; branch_id: string; endpoint_type: string };
type Check = { name: string; [key: string]: unknown };

// This is an opt-in live test: it provisions real, retained resources.
// Keep credentials outside the checkout; trace/video/network bodies are disabled.
test('native UI: project, timeline branch, writer, Proxy SQL and cold resume', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!baseURL || !adminFile || !privateDir) {
    throw new Error('Live E2E requires BASE_URL, ADMIN_PASSWORD_FILE and PRIVATE_DIR');
  }
  const privateRoot = privateDir;
  const attempt = process.env.NEON_E2E_ATTEMPT || new Date().toISOString().replace(/[^0-9]/g, '');
  if (!/^[a-z0-9_-]+$/.test(attempt)) throw new Error('Invalid evidence attempt');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  const password = randomBytes(30).toString('base64url');
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    mode: 0o600,
    flag: 'wx',
  });
  const adminPassword = (await readFile(adminFile, 'utf8')).trim();
  const checks: Check[] = [];
  const operations: { id: string; resource: string; idempotency_key?: string }[] = [];
  const endpoints = new Set<string>();
  let project = '';
  const startedAt = new Date().toISOString();
  let result = 'running';
  const expectSplit = process.env.NEON_E2E_EXPECT_SPLIT === 'true';
  const workerFault = process.env.NEON_E2E_WORKER_FAULT === 'true';
  let workerQueueRecorded = false;

  async function save() {
    const evidence = {
      result,
      started_at: startedAt,
      updated_at: new Date().toISOString(),
      project_id: project,
      checks,
      operations,
      credentials_in_report: false,
    };
    await writeFile(testInfo.outputPath('result.json'), JSON.stringify(evidence, null, 2) + '\n');
    await writeFile(
      path.join(privateRoot, attempt + '-fixture.json'),
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
  async function accepted(response: Response): Promise<Accepted> {
    expect(response.status(), 'UI mutation accepted').toBe(202);
    const item = (await response.json()) as Accepted;
    operations.push({
      id: item.operation.id,
      resource: item.resource.id,
      idempotency_key: response.request().headers()['idempotency-key'],
    });
    await save();
    if (workerFault && !workerQueueRecorded) {
      workerQueueRecorded = true;
      project = item.resource.project_id || item.resource.id;
      await expect(page.locator('.create-modal .create-progress')).toContainText('queued');
      const pending = await context.request.get(
        baseURL! + '/api/v1/projects/' + project + '/operations/' + item.operation.id,
      );
      expect(pending.status()).toBe(200);
      expect((await pending.json()).state).toBe('queued');
      await record('ui_worker_offline_preserves_operation_queue', {
        operation_id: item.operation.id,
      });
      await shot('worker-offline-queue');
      // The external Linux orchestrator restores the Worker only after this
      // UI/DB acceptance marker. No Kubernetes credentials enter the browser.
      await writeFile(
        path.join(privateRoot, 'worker-queue-ready.json'),
        JSON.stringify({ operation_id: item.operation.id }) + '\n',
        { mode: 0o600, flag: 'wx' },
      );
    }
    await expect(page.locator('.create-modal')).toBeHidden({ timeout: 490_000 });
    return item;
  }
  async function submit(route: string) {
    const response = page.waitForResponse(
      (r) => new URL(r.url()).pathname === route && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    return accepted(await response);
  }
  async function data(route: string): Promise<{ items: Endpoint[] }> {
    const response: APIResponse = await context.request.get(baseURL! + route);
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function sql(endpoint: string, statement: string): Promise<{ rows: unknown[][] }> {
    await page.goto('/#/projects/' + project + '/query');
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const response = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + endpoint + '/query') && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const completed = await response;
    expect(completed.status(), 'SQL through UI and Proxy').toBe(200);
    await expect(page.locator('.result-panel')).toBeVisible();
    return completed.json();
  }
  async function suspend(endpoint: string) {
    await page.goto('/#/projects/' + project + '/compute');
    await page.locator('select').first().selectOption(endpoint);
    const response = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + endpoint + '/suspend') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    expect((await response).status()).toBe(202);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145_000,
    });
    await record('ui_endpoint_suspended', { endpoint_id: endpoint });
  }

  await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill(adminPassword);
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (expectSplit) {
      const runtime = await context.request.get(baseURL + '/api/v1/capabilities');
      expect(runtime.status()).toBe(200);
      const status = (await runtime.json()).runtime;
      expect(status.process_role).toBe('api');
      expect(status.separated).toBe(true);
      await record('ui_uses_separate_api_worker', { process_role: status.process_role });
    }
    await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
    await page.getByLabel('项目名称').fill('ci-ui-' + attempt);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const created = await submit('/api/v1/organizations/local/projects');
    project = created.resource.id;
    const listed = (await data('/api/v1/projects/' + project + '/endpoints')).items;
    expect(listed).toHaveLength(1);
    expect(listed[0].endpoint_type).toBe('read_write');
    const writer = listed[0].id;
    const parent = listed[0].branch_id;
    endpoints.add(writer);
    await record('ui_native_project_ready', { writer, parent });
    await shot('project-created');
    await sql(
      writer,
      'CREATE TABLE public.ui_release_probe(id integer PRIMARY KEY, marker text NOT NULL)',
    );
    await sql(writer, "INSERT INTO public.ui_release_probe VALUES(1, 'parent')");
    expect((await sql(writer, 'SELECT id, marker FROM public.ui_release_probe')).rows).toEqual([
      [1, 'parent'],
    ]);
    await record('ui_proxy_postgres_write_read');
    await shot('writer-query');

    await page.goto('/#/projects/' + project + '/branches');
    await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
    await page.getByLabel('分支名称').fill('child-' + attempt);
    await page.getByLabel('父分支').selectOption(parent);
    await page.getByLabel('同时创建读写 Compute Endpoint').uncheck();
    const branch = (await submit('/api/v1/projects/' + project + '/branches')).resource.id;
    expect(
      (await data('/api/v1/projects/' + project + '/endpoints')).items.filter(
        (e) => e.branch_id === branch,
      ),
    ).toEqual([]);
    await record('ui_data_only_branch', { branch });
    await shot('data-only-branch');
    await suspend(writer);

    await page.goto('/#/projects/' + project + '/branches/' + branch);
    await page.getByRole('button', { name: '＋ 创建 Endpoint', exact: true }).click();
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const child = (await submit('/api/v1/projects/' + project + '/endpoints')).resource.id;
    endpoints.add(child);
    expect((await sql(child, 'SELECT id, marker FROM public.ui_release_probe')).rows).toEqual([
      [1, 'parent'],
    ]);
    await record('ui_branch_endpoint_inherits_data', { child });
    await sql(child, "INSERT INTO public.ui_release_probe VALUES(2, 'child')");
    await shot('child-query');
    await suspend(child);
    expect(
      (await sql(writer, 'SELECT id, marker FROM public.ui_release_probe ORDER BY id')).rows,
    ).toEqual([[1, 'parent']]);
    await record('ui_cold_resume_and_branch_isolation');
    await suspend(writer);

    await page.goto('/#/projects/' + project + '/monitoring');
    await expect(page.getByRole('heading', { name: '监控与运行洞察', exact: true })).toBeVisible();
    await expect(page.getByTestId('monitor-runtime-state')).toHaveText('已休眠');
    await expect(page.locator('.stats-grid .stat-card strong').nth(0)).toHaveText('—');
    await expect(page.locator('.stats-grid .stat-card strong').nth(1)).toHaveText('—');
    await record('ui_monitoring_does_not_present_old_active_samples_after_suspend');
    if (expectSplit) {
      await expect(page.getByLabel('控制面运行状态')).toContainText('独立 API / Worker');
      await expect(page.getByLabel('控制面运行状态')).toContainText('Worker 心跳正常');
      const runtime = await context.request.get(baseURL + '/api/v1/capabilities');
      expect((await runtime.json()).runtime.controller_status).toBe('active');
      await record('ui_worker_heartbeat_recovers');
    }
    await shot('monitoring');
    await page.goto('/#/projects/' + project + '/operations');
    await expect(page.getByRole('heading', { name: '操作记录', exact: true })).toBeVisible();
    await shot('operations');
    await record('ui_monitoring_and_operation_history');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    await shot('failure');
    throw error;
  } finally {
    await save();
    // Recovery is explicit: retained fixture identifies endpoints if the UI fails.
    // Successful tests already suspend every Compute; never erase data/evidence.
  }
});
