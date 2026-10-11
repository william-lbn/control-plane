import { expect, test } from '@playwright/test';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import path from 'node:path';

// Explicit continuation of an original accepted timestamp restore. Never
// replace the original project/branch/Operation or rewrite a failed receipt.
test('UI retained restore: original timestamp, LSN, isolation and cold wake', async ({
  page,
  context,
  baseURL,
}, info) => {
  page.setDefaultTimeout(40_000);
  const fixtureFile =
    process.env.NEON_E2E_RESTORE_RESUME_FIXTURE || process.env.NEON_E2E_RECOVERY_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const attempt = process.env.NEON_E2E_ATTEMPT;
  if (
    !fixtureFile ||
    !adminFile ||
    !privateDir ||
    !baseURL ||
    !/^publication-ui-\d+$/.test(attempt || '')
  )
    throw new Error('Protected original restore fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    project_name: string;
    operation_id: string;
    writer_id: string;
    child_branch_id: string;
    child_endpoint_id: string;
    original_failure_job: string;
    names: { receipt: string; laterTable: string; laterRole: string; laterDatabase: string };
    sourcePoint: { timestamp: string; lsn: string };
    resolved_parent_lsn: string;
    database_password_file: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^op_[a-f0-9]+$/.test(fixture.operation_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.child_branch_id) ||
    ![fixture.writer_id, fixture.child_endpoint_id].every((id) => /^ep_[a-f0-9]{16}$/.test(id)) ||
    !/^publication-ui-\d+$/.test(fixture.original_failure_job) ||
    fixture.project_name !== 'restore-' + fixture.original_failure_job ||
    !Object.values(fixture.names).every((name) => /^[a-z_0-9]{1,63}$/.test(name)) ||
    !/^[a-fA-F0-9]+\/[a-fA-F0-9]+$/.test(fixture.resolved_parent_lsn) ||
    !/^[a-fA-F0-9]+\/[a-fA-F0-9]+$/.test(fixture.sourcePoint.lsn) ||
    !Number.isFinite(Date.parse(fixture.sourcePoint.timestamp))
  )
    throw new Error('Invalid retained restore identity');
  const password = (await readFile(fixture.database_password_file, 'utf8')).trim();
  const protectedRoot = privateDir;
  const project = fixture.project_id;
  const checks: Record<string, unknown>[] = [];
  const endpoints = new Set([fixture.writer_id, fixture.child_endpoint_id]);
  let result = 'running';
  let acceptedLSN: Record<string, unknown> | null = null;
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  async function save() {
    await mkdir(info.outputDir, { recursive: true });
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          checks,
          project_id: project,
          original_failure_job: fixture.original_failure_job,
          original_operation_id: fixture.operation_id,
          creates_new_project: false,
          creates_new_timestamp_branch: false,
          credentials_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
    // The accepted request includes a SQL password; this file is private only.
    await writeFile(
      path.join(protectedRoot, attempt + '-restore-resume-fixture.json'),
      JSON.stringify(
        {
          project_id: project,
          endpoints: [...endpoints],
          original_operation_id: fixture.operation_id,
          accepted_lsn: acceptedLSN,
        },
        null,
        2,
      ) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await save();
  }
  async function get(route: string) {
    const r = await context.request.get(baseURL! + route);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function sql(endpoint: string, statement: string) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${endpoint}/query`) && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function suspend(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${endpoint}/suspend`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(202);
    const op = (await r.json()).operation.id;
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145_000,
    });
    await expect
      .poll(async () => (await get(`/api/v1/projects/${project}/operations/${op}`)).state, {
        timeout: 60_000,
      })
      .toBe('succeeded');
    return op;
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const current = await get(`/api/v1/projects/${project}`);
    expect(current.name).toBe(fixture.project_name);
    expect(current.source).toBe('managed');
    expect(current.state).toBe('ready');
    const op = await get(`/api/v1/projects/${project}/operations/${fixture.operation_id}`);
    expect(op.state).toBe('succeeded');
    expect(op.resource_id).toBe(fixture.child_branch_id);
    expect(op.action).toBe('create_branch');
    const branch = await get(`/api/v1/projects/${project}/branches/${fixture.child_branch_id}`);
    expect(branch.restore_source).toBe('timestamp');
    expect(branch.parent_lsn).toBe(fixture.resolved_parent_lsn);
    expect(Date.parse(branch.parent_timestamp)).toBe(Date.parse(fixture.sourcePoint.timestamp));
    await page.goto(`/#/projects/${project}/operations`);
    await page
      .getByRole('button', { name: '查看操作 ' + fixture.operation_id + ' 的步骤', exact: true })
      .click();
    const dialog = page.getByRole('dialog', { name: '操作详情' });
    await expect(dialog).toContainText(fixture.operation_id);
    await expect(dialog.locator('.detail-row').filter({ hasText: '状态' })).toContainText('已完成');
    await record('original_timestamp_operation_and_fixed_point_reopened_without_recreation');
    expect(
      (
        await sql(
          fixture.child_endpoint_id,
          `SELECT marker FROM public.${fixture.names.receipt} WHERE id=1`,
        )
      ).rows,
    ).toEqual([['before']]);
    expect(
      (
        await sql(
          fixture.child_endpoint_id,
          `SELECT to_regclass('public.${fixture.names.laterTable}') IS NULL, NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='${fixture.names.laterRole}'), NOT EXISTS(SELECT 1 FROM pg_database WHERE datname='${fixture.names.laterDatabase}')`,
        )
      ).rows,
    ).toEqual([[true, true, true]]);
    await record('original_timestamp_data_schema_roles_and_databases_preserved');
    await sql(
      fixture.child_endpoint_id,
      `INSERT INTO public.${fixture.names.receipt} VALUES(2,'restored-only')`,
    );
    const suspension = await suspend(fixture.child_endpoint_id);
    expect(
      (
        await sql(
          fixture.child_endpoint_id,
          `SELECT id,marker FROM public.${fixture.names.receipt} ORDER BY id`,
        )
      ).rows,
    ).toEqual([
      [1, 'before'],
      [2, 'restored-only'],
    ]);
    await record('original_restored_writer_zero_to_one_preserves_actual_rows', {
      suspend_operation: suspension,
    });
    await suspend(fixture.child_endpoint_id);
    expect(
      (
        await sql(
          fixture.writer_id,
          `SELECT marker,count(*) OVER() FROM public.${fixture.names.receipt}`,
        )
      ).rows,
    ).toEqual([['after', 1]]);
    await suspend(fixture.writer_id);
    await record('restored_writes_do_not_modify_source_current_data');
    await page.goto(`/#/projects/${project}/restore`);
    await page.getByLabel('恢复源分支').selectOption(branch.parent_branch_id);
    await page.getByRole('button', { name: '恢复到新分支', exact: true }).click();
    await page.getByLabel('分支名称').fill('lsn-' + attempt);
    await page.getByLabel('分支起点').selectOption('lsn');
    await page.getByLabel('恢复 LSN', { exact: true }).fill(fixture.sourcePoint.lsn);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/branches` &&
        r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const accepted = await response.json();
    acceptedLSN = {
      operation_id: accepted.operation.id,
      branch_id: accepted.resource.id,
      replay_body: response.request().postDataJSON(),
      replay_key: response.request().headers()['idempotency-key'],
    };
    await save();
    const candidate = (await get(`/api/v1/projects/${project}/endpoints`)).items.find(
      (e: { branch_id: string }) => e.branch_id === accepted.resource.id,
    );
    expect(candidate).toBeTruthy();
    endpoints.add(candidate.id);
    await save();
    await expect(page.locator('.create-modal')).toBeHidden({ timeout: 490_000 });
    expect(
      (await get(`/api/v1/projects/${project}/operations/${accepted.operation.id}`)).state,
    ).toBe('succeeded');
    const lsnBranch = await get(`/api/v1/projects/${project}/branches/${accepted.resource.id}`);
    expect(lsnBranch.restore_source).toBe('lsn');
    expect(lsnBranch.parent_lsn).toBe(fixture.sourcePoint.lsn);
    expect(
      (await sql(candidate.id, `SELECT id,marker FROM public.${fixture.names.receipt} ORDER BY id`))
        .rows,
    ).toEqual([[1, 'before']]);
    await record('explicit_lsn_ui_restore_preserves_historical_rows_and_fixed_point', {
      branch_id: lsnBranch.id,
      endpoint_id: candidate.id,
    });
    await suspend(candidate.id);
    expect(
      (await sql(candidate.id, `SELECT count(*) FROM public.${fixture.names.receipt}`)).rows,
    ).toEqual([[1]]);
    await suspend(candidate.id);
    await record('lsn_restored_writer_zero_to_one_preserves_actual_rows');
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('CSRF required');
    const replay = await context.request.post(baseURL + `/api/v1/projects/${project}/branches`, {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': String(acceptedLSN.replay_key) },
      data: acceptedLSN.replay_body,
    });
    expect(replay.status()).toBe(202);
    const replayed = await replay.json();
    expect(replayed.operation.id).toBe(accepted.operation.id);
    expect(replayed.resource.id).toBe(lsnBranch.id);
    expect(replayed.resource.parent_lsn).toBe(fixture.sourcePoint.lsn);
    await record('lsn_replay_preserves_original_operation_branch_and_fixed_point');
    const before = (await get(`/api/v1/projects/${project}/branches`)).items.length;
    const outside = await context.request.post(baseURL + `/api/v1/projects/${project}/branches`, {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': 'outside-' + attempt },
      data: {
        name: 'outside-' + attempt,
        parent_branch_id: branch.parent_branch_id,
        parent_timestamp: '2000-01-01T00:00:00Z',
        create_endpoint: false,
      },
    });
    expect(outside.status()).toBe(422);
    expect((await outside.json()).code).toBe('restore_point_outside_history');
    expect((await get(`/api/v1/projects/${project}/branches`)).items.length).toBe(before);
    await record('outside_history_rejected_without_accepting_new_branch');
    await page.goto(`/#/projects/${project}/restore`);
    await expect(page.locator('.table-card')).toContainText(
      'timestamp-' + fixture.original_failure_job,
    );
    await expect(page.locator('.table-card')).toContainText('lsn-' + attempt);
    await page.goto(`/#/projects/${project}/monitoring`);
    await expect(page.getByRole('heading', { name: '监控与运行洞察', exact: true })).toBeVisible();
    await expect(page.getByTestId('monitor-runtime-state')).toHaveText('已休眠');
    await page.screenshot({
      path: info.outputPath('original-restoration-and-lsn-monitoring.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
    await record('ui_restore_history_and_monitoring_after_retained_recovery');
    const all = (await get(`/api/v1/projects/${project}/endpoints`)).items;
    expect(all.map((e: { id: string }) => e.id).sort()).toEqual([...endpoints].sort());
    expect(all.every((e: { observed_state: string }) => e.observed_state === 'suspended')).toBe(
      true,
    );
    await record('all_original_and_new_lsn_computes_suspended_data_and_evidence_retained');
    result = 'pass';
  } finally {
    if (result === 'running') result = 'fail';
    await save();
  }
});
