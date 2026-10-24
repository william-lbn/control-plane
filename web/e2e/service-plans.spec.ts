import { expect, test } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Owned service plans: immutable entitlements and actual resource admission', async ({
  page,
  context,
}, info) => {
  test.setTimeout(1500000);
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (process.platform !== 'linux' || !fixtureFile || !privateDir || !adminFile)
    throw Error('Protected Linux service-plan fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    !fixture.new_owned_org ||
    !/^org_[a-f0-9]{24}$/.test(fixture.org_id) ||
    !fixture.org_name.startsWith('ci-service-plans-')
  )
    throw Error('Only the newly owned acceptance organization is allowed');
  const state: any = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : {
        nonce: String(Date.now()),
        checks: [],
        requests: {},
        operations: {},
        password: randomBytes(30).toString('base64url'),
      };
  if (state.running) throw Error('Inspect the original unfinished step before resuming');
  state.name ??= 'Functional plan ' + state.nonce;
  state.project_name ??= 'ci-plan-' + state.nonce;
  let result = 'running';
  const root = '/api/v1/platform/service-plans',
    entitlement = '/api/v1/platform/organizations/' + fixture.org_id + '/service-plan',
    project = () => '/api/v1/projects/' + state.project_id,
    panel = () => page.getByRole('region', { name: '组织服务计划', exact: true }),
    bounds = {
      min_cpu_milli: 1000,
      max_cpu_milli: 1000,
      min_memory_mib: 1024,
      max_memory_mib: 1024,
    };
  async function save() {
    await writeFile(
      path.join(privateDir!, 'service-plans-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks: state.checks,
        org_id: fixture.org_id,
        plan_id: state.plan_id,
        project_id: state.project_id,
        original_limits_restored: state.checks.includes(
          'owned_cleanup_and_original_limits_restored',
        ),
        payment_processing: false,
        commercial_calls: false,
        credentials_exported: false,
        performance_reliability_campaign: false,
        transient_read_failures: state.transient_read_failures || 0,
      }) + '\n',
    );
  }
  async function step(label: string, work: () => Promise<void>) {
    if (state.checks.includes(label)) return;
    state.running = label;
    await save();
    await work();
    state.checks.push(label);
    state.running = '';
    await save();
  }
  async function get(url: string) {
    for (let attempt = 0; attempt < 3; attempt++) {
      const response = await context.request.get(url, { timeout: 45000 });
      if (response.status() === 200) return response.json();
      if (![429, 502, 503, 504].includes(response.status()) || attempt === 2) {
        expect(response.status()).toBe(200);
        throw Error('Original read unavailable');
      }
      state.transient_read_failures = (state.transient_read_failures || 0) + 1;
      await save();
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
    throw Error('Original read unavailable');
  }
  async function headers(key: string) {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': decodeURIComponent(csrf), 'Idempotency-Key': key };
  }
  async function request(label: string, method: string, url: string, body: unknown, status = 202) {
    state.requests[label] ??= {
      method,
      url,
      body,
      key: 'service-plans-' + state.nonce + '-' + label,
    };
    await save();
    const pending = state.requests[label];
    const response = await context.request.fetch(pending.url, {
      method: pending.method,
      headers: await headers(pending.key),
      data: pending.body,
      timeout: 190000,
    });
    expect(response.status()).toBe(status);
    return response.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const value = await get(project() + '/operations/' + id);
          if (['failed', 'cancelled'].includes(value.state))
            throw Error('Inspect original operation ' + id + ': ' + value.error_code);
          return value.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function create(label: string, url: string, body: object) {
    if (!state.operations[label]) {
      const value = await request(label, 'POST', url, body);
      if (label === 'project') state.project_id = value.resource.id;
      state.operations[label] = value.operation.id;
      await save();
    }
    await observe(state.operations[label]);
  }
  async function visit() {
    await page.goto('/#/organization');
    await expect(panel().getByText('服务计划与资源配额', { exact: true })).toBeVisible();
    await expect(panel().getByLabel('编辑服务计划', { exact: true })).toBeVisible();
    if (state.plan_id)
      await panel().getByLabel('编辑服务计划', { exact: true }).selectOption(state.plan_id);
    await expect(
      panel().getByRole('button', {
        name: state.plan_id ? '发布新的计划修订' : '创建服务计划',
        exact: true,
      }),
    ).toBeVisible();
    await expect(panel().getByLabel('服务计划名称', { exact: true })).toBeEnabled();
  }
  async function ui(label: string, button: string, url: string, method: string, status = 200) {
    if (state.requests[label]) return request(label, method, url, {}, status);
    const pattern = '**' + url;
    await page.route(pattern, async (route) => {
      if (route.request().method() !== method) return route.continue();
      state.requests[label] = {
        method,
        url,
        body: route.request().postDataJSON(),
        key: route.request().headers()['idempotency-key'],
      };
      await save();
      await route.continue();
    });
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === url && r.request().method() === method,
    );
    await panel().getByRole('button', { name: button, exact: true }).click();
    const response = await waiting;
    await page.unroute(pattern);
    expect(response.status()).toBe(status);
    const value = await response.json();
    if (status < 400)
      await expect(panel().getByRole('status')).toHaveText(
        '原操作已确认，配额与不可变历史已保存。',
      );
    return value;
  }
  async function lifecycle(label: string, button: string, name: string, branch = '') {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return;
    }
    await page.goto('/#/projects/' + state.project_id + '/lifecycle');
    const target = branch
      ? page.getByTestId('lifecycle-' + branch)
      : page.getByTestId('project-lifecycle');
    await target.getByRole('button', { name: button, exact: true }).click();
    await page.getByLabel('资源名称确认').fill(name);
    const waiting = page.waitForResponse(
      (r) =>
        r.request().method() !== 'GET' &&
        new URL(r.url()).pathname.startsWith(project()) &&
        !r.url().includes('/operations/'),
    );
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    const response = await waiting;
    expect([200, 202]).toContain(response.status());
    if (response.status() === 202) {
      state.operations[label] = (await response.json()).operation.id;
      await save();
      await observe(state.operations[label]);
    }
    await expect(page.locator('.lifecycle-modal')).not.toBeVisible({ timeout: 490000 });
  }
  await save();
  try {
    await page.goto('/#/organization');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await page.getByLabel('切换组织', { exact: true }).click();
    await page.getByRole('option', { name: fixture.org_name + ' · admin', exact: true }).click();
    await step('new_owned_scope_and_unchanged_original_limits', async () => {
      await visit();
      const original = (await get(entitlement)).entitlement;
      expect([original.max_projects, original.max_endpoints, original.version]).toEqual([
        50, 200, 0,
      ]);
      expect([original.current_projects, original.current_endpoints]).toEqual([0, 0]);
      expect(
        (await context.request.get('/api/v1/platform/organizations/local/service-plan')).status(),
      ).toBe(403);
    });
    await step('GUI_lost_creation_reply_replays_exact_original_receipt', async () => {
      await visit();
      if (!state.requests['lost-create']) {
        await panel().getByLabel('服务计划名称', { exact: true }).fill(state.name);
        await panel().getByLabel('服务计划项目上限', { exact: true }).fill('1');
        await panel().getByLabel('服务计划 Endpoint 上限', { exact: true }).fill('1');
        await page.route('**' + root, async (route) => {
          if (route.request().method() !== 'POST') return route.continue();
          state.requests['lost-create'] = {
            method: 'POST',
            url: root,
            body: route.request().postDataJSON(),
            key: route.request().headers()['idempotency-key'],
          };
          await save();
          const original = await route.fetch({ maxRetries: 0, maxRedirects: 0 });
          expect(original.status()).toBe(201);
          const value = await original.json();
          state.plan_id = value.plan_id;
          state.revision1 = value.revision_id;
          await save();
          await route.abort('failed');
        });
        await panel().getByRole('button', { name: '创建服务计划', exact: true }).click();
        await expect(
          panel().getByRole('button', { name: '读取原计划操作回执', exact: true }),
        ).toBeVisible();
        await page.unroute('**' + root);
        const waiting = page.waitForResponse(
          (r) => new URL(r.url()).pathname === root && r.request().method() === 'POST',
        );
        await panel().getByRole('button', { name: '读取原计划操作回执', exact: true }).click();
        expect((await waiting).headers()['idempotency-replayed']).toBe('true');
      } else {
        const pending = state.requests['lost-create'];
        const response = await context.request.post(root, {
          headers: await headers(pending.key),
          data: pending.body,
        });
        expect(response.status()).toBe(201);
        expect(response.headers()['idempotency-replayed']).toBe('true');
        const value = await response.json();
        state.plan_id = value.plan_id;
        state.revision1 = value.revision_id;
      }
      expect((await get(root)).items.filter((v: any) => v.name === state.name)).toHaveLength(1);
    });
    await step('GUI_assignment_and_first_real_project_admitted', async () => {
      await visit();
      await panel().getByLabel('组织计划版本', { exact: true }).selectOption(state.revision1);
      await ui('assign1', '确认分配组织计划', entitlement, 'PUT');
      await create('project', '/api/v1/organizations/' + fixture.org_id + '/projects', {
        name: state.project_name,
        password: state.password,
        autoscaling: bounds,
      });
      const endpoints = (await get(project() + '/endpoints')).items;
      expect(endpoints).toHaveLength(1);
      expect(endpoints[0].state).toBe('active');
      state.writer_id = endpoints[0].id;
      state.branch_id = endpoints[0].branch_id;
      await save();
    });
    await step('project_and_Reader_quota_denied_without_new_operations', async () => {
      const before = await get(project() + '/operations');
      for (const [label, url, body] of [
        [
          'deny-project',
          '/api/v1/organizations/' + fixture.org_id + '/projects',
          { name: 'ci-denied-' + state.nonce, password: state.password, autoscaling: bounds },
        ],
        [
          'deny-reader',
          project() + '/endpoints',
          { branch_id: state.branch_id, type: 'read_only', password: '', autoscaling: bounds },
        ],
      ] as const) {
        const value = await request(label, 'POST', url, body, 429);
        expect(value.code).toBe('organization_quota_exceeded');
      }
      expect((await get(project() + '/operations')).items.length).toBe(before.items.length);
      const value = (await get(entitlement)).entitlement;
      expect([
        value.current_projects,
        value.current_endpoints,
        value.max_projects,
        value.max_endpoints,
      ]).toEqual([1, 1, 1, 1]);
    });
    await step('GUI_publish_new_revision_preserves_existing_entitlement', async () => {
      await visit();
      await panel().getByLabel('服务计划项目上限', { exact: true }).fill('2');
      await panel().getByLabel('服务计划 Endpoint 上限', { exact: true }).fill('2');
      const value = await ui(
        'publish2',
        '发布新的计划修订',
        root + '/' + state.plan_id + '/revisions',
        'POST',
      );
      state.revision2 = value.revision_id;
      await save();
      const current = (await get(entitlement)).entitlement;
      expect(current.revision_id).toBe(state.revision1);
      expect(current.max_endpoints).toBe(1);
      const revisions = (await get(root + '/' + state.plan_id)).revisions;
      expect(revisions).toHaveLength(2);
      expect(revisions.find((v: any) => v.id === state.revision1).max_endpoints).toBe(1);
    });
    await step('GUI_explicit_revision_switch_admits_Reader', async () => {
      await visit();
      await panel().getByLabel('组织计划版本', { exact: true }).selectOption(state.revision2);
      await ui('assign2', '确认分配组织计划', entitlement, 'PUT');
      await create('reader', project() + '/endpoints', {
        branch_id: state.branch_id,
        type: 'read_only',
        password: '',
        autoscaling: bounds,
      });
      expect((await get(project() + '/endpoints')).items).toHaveLength(2);
    });
    await step('GUI_downgrade_below_actual_usage_rejected_atomically', async () => {
      await visit();
      const before = await get(entitlement);
      await panel().getByLabel('组织计划版本', { exact: true }).selectOption(state.revision1);
      const value = await ui('deny-downgrade', '确认分配组织计划', entitlement, 'PUT', 409);
      expect(value.code).toBe('service_plan_below_current_usage');
      const after = await get(entitlement);
      expect(after.entitlement).toEqual(before.entitlement);
      expect(after.history).toEqual(before.history);
      await expect(panel().getByRole('alert')).toBeVisible();
    });
    await step('GUI_disable_preserves_assigned_revision_and_denies_new_assignment', async () => {
      await visit();
      await ui('disable', '禁用服务计划', root + '/' + state.plan_id + '/state', 'PUT');
      expect(
        (await get('/api/v1/service-plans')).items.some((v: any) => v.id === state.plan_id),
      ).toBe(false);
      const before = await get(entitlement);
      expect(before.entitlement.revision_id).toBe(state.revision2);
      const denied = await request(
        'disabled-assign',
        'PUT',
        entitlement,
        { version: before.entitlement.version, revision_id: state.revision2 },
        409,
      );
      expect(denied.code).toBe('service_plan_disabled');
      expect((await get(entitlement)).entitlement).toEqual(before.entitlement);
    });
    await step('owned_cleanup_and_original_limits_restored', async () => {
      const current = await get(project() + '/lifecycle');
      if (current.project.protected)
        await lifecycle('unprotect-project', '解除项目保护', state.project_name);
      for (const branch of current.branches)
        if (branch.protected)
          await lifecycle('unprotect-' + branch.id, '解除分支保护', branch.name, branch.id);
      await lifecycle('delete-project', '删除项目', state.project_name);
      await visit();
      await panel().getByLabel('组织计划版本', { exact: true }).selectOption('');
      await ui('clear', '确认分配组织计划', entitlement, 'PUT');
      const final = await get(entitlement);
      expect([final.entitlement.max_projects, final.entitlement.max_endpoints]).toEqual([50, 200]);
      expect([final.entitlement.current_projects, final.entitlement.current_endpoints]).toEqual([
        0, 0,
      ]);
      expect(final.entitlement.revision_id).toBeNull();
      expect(final.history).toHaveLength(3);
      expect((await get(root + '/' + state.plan_id)).plan.state).toBe('disabled');
    });
    result = 'pass';
  } catch (error) {
    result = 'fail';
    throw error;
  } finally {
    await save();
  }
});
