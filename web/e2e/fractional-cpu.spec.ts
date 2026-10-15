import { test, expect, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
test('UI fractional Reader uses 250m to 500m to 250m and preserves SQL and cold wake', async ({
  page,
  context,
}, info) => {
  test.setTimeout(1200000);
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !privateDir)
    throw new Error('Protected Linux owner/operator fixture required');
  const owner = JSON.parse(await readFile(fixtureFile, 'utf8')),
    project = owner.project_id,
    branch = owner.branch_id,
    password = (await readFile(owner.password_file, 'utf8')).trim();
  if (owner.database !== 'owner_transfer_v1') throw new Error('Retained owner required');
  const marker = path.join(privateDir, 'fractional-marker.json'),
    proof = path.join(privateDir, 'fractional-proof.json');
  const resumeFile = process.env.NEON_E2E_FRACTIONAL_RESUME_FILE;
  const resume = resumeFile ? JSON.parse(await readFile(resumeFile, 'utf8')) : null;
  let result = 'running',
    endpoint = resume?.endpoint_id ?? '';
  const checks: string[] = resume?.checks ?? [],
    operations: string[] = resume?.operations ?? [];
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        project_id: project,
        branch_id: branch,
        endpoint_id: endpoint,
        mode: 'fractional logical CPU and QEMU overcommit validation',
        strict_guest_cpu_quota_verified: false,
        production_qualified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
  }
  async function operation(response: Response) {
    expect(response.status()).toBe(202);
    const value = await response.json();
    operations.push(value.operation.id);
    if (!endpoint) endpoint = value.resource.id;
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${value.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Original fractional operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function observe(stage: string, cpu: number, memory: number) {
    await writeFile(
      marker,
      JSON.stringify({ endpoint_id: endpoint, stage, cpu_milli: cpu, memory_mib: memory }),
      { mode: 0o600 },
    );
    await expect
      .poll(
        async () => {
          try {
            const report = JSON.parse(await readFile(proof, 'utf8'));
            return report.samples?.some(
              (s: {
                stage: string;
                logical_cpu_milli: number;
                runner_cpu_milli: number;
                observed_memory_mib: number;
                qemu_cpu_quota: number;
                qemu_cpu_period: number;
              }) =>
                s.stage === stage &&
                s.logical_cpu_milli === cpu &&
                s.runner_cpu_milli === cpu &&
                s.observed_memory_mib === memory &&
                s.qemu_cpu_quota / s.qemu_cpu_period === (cpu / 1000) * 4,
            );
          } catch {
            return false;
          }
        },
        { timeout: 360000, intervals: [3000, 5000] },
      )
      .toBe(true);
  }
  async function bounds(low: number, high: number, minMem: number, maxMem: number) {
    await page.getByLabel('最小 CPU', { exact: true }).selectOption(String(low));
    await page.getByLabel('最大 CPU', { exact: true }).selectOption(String(high));
    await page.getByLabel('最小内存', { exact: true }).selectOption(String(minMem));
    await page.getByLabel('最大内存', { exact: true }).selectOption(String(maxMem));
    const wait = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}` &&
        r.request().method() === 'PATCH',
    );
    await page.getByRole('button', { name: '保存伸缩范围', exact: true }).click();
    await operation(await wait);
  }
  async function sql() {
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(endpoint);
    await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
    await page.getByLabel('执行模式', { exact: true }).selectOption('single');
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill('SELECT pg_is_in_recovery(), current_database()');
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    const wait = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query`,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const r = await wait;
    expect(r.status()).toBe(200);
    expect((await r.json()).rows).toEqual([[true, owner.database]]);
  }
  async function compute() {
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByLabel('选择 Endpoint').selectOption(endpoint);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (resume) {
      expect(resume.original_job).toBe('publication-ui-20261015125835');
      expect(endpoint).toBe('ep_0c0e38aedc1d423c');
      expect(checks).toHaveLength(1);
      const r = await context.request.get(`/api/v1/projects/${project}/endpoints/${endpoint}`);
      expect(r.status()).toBe(200);
      const current = await r.json();
      expect(current.endpoint_type).toBe('read_only');
      expect(current.branch_id).toBe(branch);
      expect(current.min_cpu_milli).toBe(250);
      expect(current.max_cpu_milli).toBe(500);
      await sql();
    } else {
      await page.goto(`/#/projects/${project}/branches/${branch}`);
      await page.getByRole('button', { name: /^＋ (添加 Compute|创建 Endpoint)$/ }).click();
      await page.getByLabel('Compute 类型', { exact: true }).selectOption('read_only');
      await page.getByLabel('计算资源预设', { exact: true }).selectOption('fractional');
      const creating = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建并验证', exact: true }).click();
      await operation(await creating);
      await save('ui_native_reader_created_with_fractional_250m_500m_and_1_2Gi_profile');
    }
    await observe('initial', 250, 1024);
    if (!resume) await sql();
    await save('actual_runner_status_and_qemu_quota_at_250m_with_real_read_replica_sql');
    await compute();
    await bounds(500, 500, 2048, 2048);
    await observe('raised', 500, 2048);
    await save('ui_bound_change_agent_controller_and_qemu_quota_reach_500m_2Gi');
    await bounds(250, 500, 1024, 2048);
    await observe('returned', 250, 1024);
    await sql();
    await save('idle_agent_returns_to_250m_1Gi_and_reader_sql_remains_valid');
    await compute();
    await page.screenshot({ path: info.outputPath('fractional-cpu.png'), fullPage: true });
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await sql();
    await save('fractional_reader_cold_wakes_with_retained_bounds_and_same_database_identity');
    await compute();
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await writeFile(marker, JSON.stringify({ done: true, endpoint_id: endpoint }));
    await save('owned_fractional_reader_suspended_metadata_sql_and_observer_proof_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
