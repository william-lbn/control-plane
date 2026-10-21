import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('Automatic manager leaf renewal preserves the original Function boot and SQL identity', async ({
  page,
  context,
}, info) => {
  test.setTimeout(900000);
  page.setDefaultTimeout(30000);
  const input = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !input || !admin || !zip || !privateDir)
    throw Error('Protected Linux owner and retained native SQL fixture required');
  const owner = JSON.parse(await readFile(input, 'utf8'));
  for (const [key, prefix] of [
    ['project_id', 'prj'],
    ['branch_id', 'br'],
    ['writer_id', 'ep'],
  ])
    if (!new RegExp('^' + prefix + '_[a-f0-9]{16}$').test(owner[key]))
      throw Error('Exact retained test scope required');
  const project = owner.project_id,
    branch = owner.branch_id,
    writer = owner.writer_id,
    base = `/api/v1/projects/${project}`,
    path = `${base}/branches/${branch}/functions/certprobe57`;
  const state = owner.manager_identity_resume || {
    result: 'running',
    checks: [],
    operations: [],
    function_id: '',
    deployment_id: '',
    before: null,
    after: null,
  };
  state.result = 'running';
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    const r = {
      ...state,
      project_id: project,
      branch_id: branch,
      writer_id: writer,
      credentials_in_evidence: false,
      data_retained: true,
      performance_reliability_campaign: false,
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(r) + '\n');
    await writeFile(privateDir + '/manager-identity-checkpoint.json', JSON.stringify(r) + '\n', {
      mode: 0o600,
    });
  }
  async function get(p: string) {
    const r = await context.request.get(p);
    expect(r.status(), p).toBe(200);
    return r.json();
  }
  async function operation(id: string) {
    if (!state.operations.includes(id)) {
      state.operations.push(id);
      await save();
    }
    await expect
      .poll(
        async () => {
          const o = await get(base + '/operations/' + id);
          if (['failed', 'cancelled'].includes(o.state))
            throw Error('Original operation ' + id + ' ' + o.error_code);
          return o.state;
        },
        { timeout: 480000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function action(p: string, button: string, method = 'POST') {
    const response = page.waitForResponse(
      (r) => new URL(r.url()).pathname === p && r.request().method() === method,
      { timeout: 45000 },
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return response;
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const v = await r.json();
    await operation(v.operation.id);
    return v;
  }
  async function select() {
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByLabel('Functions 分支', { exact: true }).selectOption(branch);
    await action(`${base}/branches/${branch}/functions`, '刷新函数状态', 'GET');
    if (state.function_id)
      await page.getByRole('button', { name: '查看函数 certprobe57', exact: true }).click();
  }
  async function observe() {
    await select();
    const r = await action(path + '/runtime', '观察运行状态', 'GET');
    expect(r.status()).toBe(200);
    const v = await r.json();
    expect(v.observed).toBe(true);
    expect(v.manager_certificate).toBeTruthy();
    await expect(page.getByTestId('function-manager-certificate')).toContainText(
      v.manager_certificate.fingerprint,
    );
    return v;
  }
  async function invoke() {
    await select();
    await page.getByLabel('调用方法', { exact: true }).selectOption('GET');
    await page.getByLabel('调用路径', { exact: true }).fill('/');
    const r = await action(path + '/console-invoke', '调用函数');
    expect(r.status()).toBe(200);
    const v = await r.json();
    expect(v.status_code).toBe(200);
    const body = JSON.parse(Buffer.from(v.body, 'base64').toString('utf8'));
    expect(body).toMatchObject({
      code: 'branch-copy-v1',
      value: 'parent-after-fork',
      environment: 'renewal-environment-v1',
      role: 'fn_' + state.function_id.slice(4),
    });
    return body;
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!state.checks.includes('original_definition_deployment_and_actual_sql_before_renewal')) {
      if (!state.function_id) {
        await select();
        await page.getByLabel('函数名称', { exact: true }).fill('certprobe57');
        await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
        await page.getByLabel('授权 Schema', { exact: true }).fill('app');
        const r = await action(`${base}/branches/${branch}/functions`, '创建函数定义');
        expect(r.status()).toBe(202);
        const v = await r.json();
        state.function_id = v.function_id;
        await save();
        await operation(v.operation.id);
      }
      if (!state.deployment_id) {
        await select();
        await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
        await page
          .getByLabel('环境变量 JSON', { exact: true })
          .fill(JSON.stringify({ BRANCH_MARKER: 'renewal-environment-v1' }));
        await wait(await action(path + '/deployments', '部署函数版本'));
        state.deployment_id = (await get(path)).active_deployment_id;
        await save();
      }
      state.before = await observe();
      expect(state.before.deployment_id).toBe(state.deployment_id);
      await invoke();
      await save('original_definition_deployment_and_actual_sql_before_renewal');
    }
    if (
      !state.checks.includes('automatic_fresh_leaf_same_instance_deployment_version_and_sql_role')
    ) {
      await expect
        .poll(
          async () => {
            const v = await get(path + '/runtime');
            expect(v.observed).toBe(true);
            return v.manager_certificate?.renewals || 0;
          },
          { timeout: 300000, intervals: [5000, 10000] },
        )
        .toBeGreaterThan(state.before.manager_certificate.renewals);
      state.after = await observe();
      for (const k of [
        'function_id',
        'instance_id',
        'deployment_id',
        'definition_version_token',
        'generation_token',
      ])
        expect(state.after[k]).toBe(state.before[k]);
      expect(state.after.manager_certificate.fingerprint).not.toBe(
        state.before.manager_certificate.fingerprint,
      );
      expect(Date.parse(state.after.manager_certificate.expires_at)).toBeGreaterThan(
        Date.parse(state.before.manager_certificate.expires_at),
      );
      await invoke();
      await save('automatic_fresh_leaf_same_instance_deployment_version_and_sql_role');
    }
    if (
      !state.checks.includes('normal_original_function_retirement_and_writer_zero_data_retained')
    ) {
      const definition = await get(path);
      expect(definition.id).toBe(state.function_id);
      await select();
      if (definition.state !== 'suspended')
        await wait(await action(path + '/suspend', '停止函数实例'));
      await select();
      await wait(await action(path, '删除函数', 'DELETE'));
      await page.goto(`/#/projects/${project}/compute`);
      await page.locator('select').first().selectOption(writer);
      if ((await get(base + '/endpoints/' + writer)).observed_state !== 'suspended')
        await wait(await action(base + '/endpoints/' + writer + '/suspend', '现在缩到 0'));
      await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
        timeout: 150000,
      });
      await save('normal_original_function_retirement_and_writer_zero_data_retained');
    }
    state.result = 'pass';
    await save();
  } catch (error) {
    state.result = 'fail';
    await save();
    throw error;
  }
});
