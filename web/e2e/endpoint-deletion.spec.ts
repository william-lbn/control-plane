import { expect, test } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Serial Linux UI acceptance. Only this uniquely-created fixture is retired;
// branch SQL, object bytes, credential files and all evidence remain retained.
test('UI independent Compute deletion, lost reply, replica continuity and retained data', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!adminFile || !privateDir || !baseURL || process.platform !== 'linux')
    throw new Error('Protected Linux live UI inputs required');
  const attempt = `${process.env.NEON_E2E_ATTEMPT || Date.now()}-epdelete`;
  const password = randomBytes(30).toString('base64url');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    mode: 0o600,
    flag: 'wx',
  });
  let project = '';
  let branch = '';
  let result = 'running';
  const endpoints = new Set<string>();
  const checks: { name: string; [key: string]: unknown }[] = [];
  const operations: { id: string; action: string; resource_id: string }[] = [];
  const cleanup: { endpoint_id?: string; status: number; operation_id?: string }[] = [];
  const projectName = 'ci-' + attempt;
  const base = () => `/api/v1/projects/${project}`;
  const storagePath = () => `${base()}/branches/${branch}/storage`;
  async function save() {
    await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          branch_id: branch,
          endpoints: [...endpoints],
          checks,
          operations,
          cleanup,
          data_retained: true,
          credentials_in_evidence: false,
          production_qualified: false,
        },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(privateDir!, attempt + '-fixture.json'),
      JSON.stringify({ project_id: project, branch_id: branch, endpoints: [...endpoints] }) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await save();
  }
  async function headers() {
    const csrf = (await context.cookies(baseURL!)).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('CSRF cookie missing');
    return { 'X-CSRF-Token': csrf, Origin: new URL(baseURL!).origin };
  }
  async function get(url: string) {
    const response = await context.request.get(url);
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function created(url: string) {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === url && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const value = await response.json();
    operations.push(value.operation);
    await expect(page.locator('.create-modal')).not.toBeVisible({ timeout: 480000 });
    return value.resource.id as string;
  }
  async function sql(endpoint: string, statement: string, expectedStatus = 200) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${endpoint}/query`) && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const response = await pending;
    await expect(page.locator('input[type="password"]')).toHaveValue('');
    expect(response.status()).toBe(expectedStatus);
    return response.json();
  }
  async function createEndpoint(type: 'read_only' | 'read_write') {
    await page.goto(`/#/projects/${project}/branches/${branch}`);
    await page.getByRole('button', { name: /^＋ (添加 Compute|创建 Endpoint)$/ }).click();
    await page.getByLabel('Compute 类型').selectOption(type);
    if (type === 'read_write')
      await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const id = await created(base() + '/endpoints');
    endpoints.add(id);
    await save();
    return id;
  }
  async function openDelete(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    const current = await get(`${base()}/endpoints/${endpoint}`);
    await page.getByRole('button', { name: '删除此 Compute', exact: true }).click();
    await expect(page.getByRole('dialog', { name: '删除 Compute Endpoint' })).toBeVisible();
    await page.getByLabel('Endpoint Selector 确认').fill(current.selector);
    return current;
  }
  async function deleteEndpoint(endpoint: string) {
    await openDelete(endpoint);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `${base()}/endpoints/${endpoint}` &&
        r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '确认删除 Compute', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const value = await response.json();
    operations.push(value.operation);
    await expect(page.locator('.endpoint-delete-modal')).not.toBeVisible({ timeout: 480000 });
    expect((await context.request.get(`${base()}/endpoints/${endpoint}`)).status()).toBe(404);
    return value.operation.id as string;
  }
  async function storage(disable: boolean) {
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByLabel('存储分支').selectOption(branch);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === storagePath() &&
        r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '禁用对象存储' : '启用对象存储', exact: true })
      .click();
    const reply = await pending;
    expect(reply.status()).toBe(202);
    operations.push((await reply.json()).operation);
    await expect(page.getByTestId('storage-operation')).toContainText('succeeded', {
      timeout: 490000,
    });
    await expect
      .poll(async () => (await get(storagePath())).state, { timeout: 490000 })
      .toBe(disable ? 'disabled' : 'active');
  }
  async function lifecycle(button: string, branchId = '') {
    await page.goto(`/#/projects/${project}/lifecycle`);
    const target = page.getByTestId(branchId ? 'lifecycle-' + branchId : 'project-lifecycle');
    await target.getByRole('button', { name: button, exact: true }).click();
    await page.getByLabel('资源名称确认').fill(branchId ? 'main' : projectName);
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    await expect(page.locator('.lifecycle-modal')).not.toBeVisible({ timeout: 480000 });
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
    await page.getByLabel('项目名称').fill(projectName);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    project = await created('/api/v1/organizations/local/projects');
    const original = (await get(base() + '/endpoints')).items[0];
    const writer = original.id as string;
    branch = original.branch_id;
    endpoints.add(writer);
    // The SQL workbench uses a single prepared statement per request.
    await sql(
      writer,
      'CREATE TABLE public.endpoint_retention_probe(id int PRIMARY KEY, value text)',
    );
    await sql(writer, "INSERT INTO public.endpoint_retention_probe VALUES(1,'retained')");
    await sql(writer, 'GRANT CREATE ON DATABASE postgres TO control_probe');
    await record('ui_created_native_project_and_persisted_branch_sql');
    expect((await sql(writer, 'SELECT 1; SELECT 2', 422)).code).toBe('sql_failed');
    await record('ui_rejected_multi_statement_query_clears_single_request_password');
    const reader1 = await createEndpoint('read_only');
    const reader2 = await createEndpoint('read_only');
    expect(
      (await sql(reader2, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id')).rows,
    ).toEqual([[1, 'retained']]);
    await record('ui_two_independent_readers_observe_real_writer_wal');

    const reviewed = await openDelete(reader1);
    await page.getByLabel('Endpoint Selector 确认').fill('wrong-selector');
    await expect(
      page.getByRole('button', { name: '确认删除 Compute', exact: true }),
    ).toBeDisabled();
    await page.getByLabel('Endpoint Selector 确认').fill(reviewed.selector);
    await record('ui_requires_exact_selector_confirmation_before_delete');
    const captured: { key: string; version: string; operation_id: string }[] = [];
    const deleteURL = `${base()}/endpoints/${reader1}`;
    await page.route('**' + deleteURL, async (route) => {
      if (route.request().method() !== 'DELETE') return route.continue();
      const response = await route.fetch();
      expect(response.status()).toBe(202);
      const value = await response.json();
      captured.push({
        key: route.request().headers()['idempotency-key'],
        version: route.request().headers()['if-match'],
        operation_id: value.operation.id,
      });
      if (captured.length === 1) return route.abort('failed'); // server accepted; browser lost the reply
      await route.fulfill({ response });
    });
    await page.getByRole('button', { name: '确认删除 Compute', exact: true }).click();
    await expect(page.locator('.endpoint-delete-modal [role="alert"]')).toBeVisible();
    await expect(page.getByRole('button', { name: '确认删除 Compute', exact: true })).toBeEnabled();
    await page.getByRole('button', { name: '确认删除 Compute', exact: true }).click();
    await expect(page.locator('.endpoint-delete-modal')).not.toBeVisible({ timeout: 480000 });
    await page.unroute('**' + deleteURL);
    expect(captured).toHaveLength(2);
    expect(captured[1]).toEqual(captured[0]);
    const oldDelete = await get(`${base()}/operations/${captured[0].operation_id}`);
    expect(oldDelete.state).toBe('succeeded');
    expect(oldDelete.steps).toHaveLength(4);
    expect(oldDelete.steps.every((s: { state: string }) => s.state === 'succeeded')).toBe(true);
    operations.push(oldDelete);
    await record('ui_lost_accepted_reply_reuses_key_version_and_same_operation', {
      operation_id: oldDelete.id,
    });
    const live = (await get(base() + '/endpoints')).items;
    expect(live.map((e: { id: string }) => e.id).sort()).toEqual([writer, reader2].sort());
    expect(
      (await sql(reader2, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id')).rows,
    ).toEqual([[1, 'retained']]);
    await record('ui_reader_delete_preserves_writer_other_reader_and_branch_data');
    const replay = await context.request.delete(deleteURL, {
      headers: {
        ...(await headers()),
        'Idempotency-Key': captured[0].key,
        'If-Match': captured[0].version,
      },
      data: { confirm_selector: reviewed.selector },
    });
    expect(replay.status()).toBe(202);
    expect((await replay.json()).operation.id).toBe(oldDelete.id);
    expect((await context.request.get(deleteURL + '/connection-info')).status()).toBe(404);
    await record('ui_deleted_selector_is_closed_and_completed_request_replay_is_stable');

    await storage(false);
    await openDelete(writer);
    const blocked = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `${base()}/endpoints/${writer}` &&
        r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '确认删除 Compute', exact: true }).click();
    const denied = await blocked;
    expect(denied.status()).toBe(409);
    expect((await denied.json()).code).toBe('endpoint_has_services');
    await expect(page.locator('.endpoint-delete-modal [role="alert"]')).toContainText(
      'Disable dependent',
    );
    await page.getByRole('button', { name: '取消', exact: true }).click();
    await record('ui_active_object_storage_blocks_writer_deletion_without_side_effects');
    await storage(true);
    const writerDeletion = await deleteEndpoint(writer);
    await record('ui_explicit_service_disable_allows_writer_retirement', {
      operation_id: writerDeletion,
    });
    expect(
      (await sql(reader2, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id')).rows,
    ).toEqual([[1, 'retained']]);
    await record('ui_remaining_reader_stays_queryable_after_writer_deletion');
    const wrongPassword = await context.request.post(base() + '/endpoints', {
      headers: { ...(await headers()), 'Idempotency-Key': 'wrong-replacement-' + attempt },
      data: {
        branch_id: branch,
        type: 'read_write',
        password: randomBytes(30).toString('base64url'),
      },
    });
    expect(wrongPassword.status()).toBe(422);
    expect((await wrongPassword.json()).code).toBe('branch_password_mismatch');
    await record('replacement_writer_does_not_implicitly_rotate_branch_credentials');
    const replacement = await createEndpoint('read_write');
    expect(replacement).not.toBe(writer);
    const newEndpoint = await get(`${base()}/endpoints/${replacement}`);
    expect(newEndpoint.selector).not.toBe(original.selector);
    expect(
      (await sql(replacement, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id')).rows,
    ).toEqual([[1, 'retained']]);
    await record('ui_new_writer_uses_new_selector_with_original_data_and_sql_password');
    await sql(
      replacement,
      "INSERT INTO public.endpoint_retention_probe VALUES(2,'after-replacement')",
    );
    await expect
      .poll(
        async () =>
          Number(
            (await sql(reader2, 'SELECT count(*) FROM public.endpoint_retention_probe')).rows[0][0],
          ),
        { timeout: 90000, intervals: [2000] },
      )
      .toBe(2);
    await record('ui_existing_reader_follows_replacement_writer_wal');
    await storage(false);
    const rebound = await get(storagePath());
    expect(rebound.state).toBe('active');
    expect(rebound.endpoint_id).toBe(replacement);
    await storage(true);
    await record('ui_disabled_backend_rebinds_to_replacement_writer_without_data_reset');
    const readerDeletion = await deleteEndpoint(reader2);
    await record('ui_final_reader_retirement_is_independent', { operation_id: readerDeletion });
    await deleteEndpoint(replacement);
    await expect(
      page.getByRole('heading', { name: '分支数据已保留，当前没有 Compute', exact: true }),
    ).toBeVisible();
    expect(
      (await get(base() + '/branches')).items.find((b: { id: string }) => b.id === branch).state,
    ).toBe('ready');
    await record('ui_no_compute_empty_state_keeps_branch_available');
    const finalWriter = await createEndpoint('read_write');
    expect(
      (await sql(finalWriter, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id')).rows,
    ).toEqual([
      [1, 'retained'],
      [2, 'after-replacement'],
    ]);
    await record('ui_recreate_after_all_compute_retired_restores_exact_rows');
    await lifecycle('解除分支保护', branch);
    await lifecycle('解除项目保护');
    await lifecycle('删除项目');
    await lifecycle('恢复项目');
    const recovered = (await get(base() + '/endpoints')).items;
    expect(recovered.map((e: { id: string }) => e.id)).toEqual([finalWriter]);
    expect(
      (await sql(finalWriter, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id')).rows,
    ).toEqual([
      [1, 'retained'],
      [2, 'after-replacement'],
    ]);
    await record('ui_project_recovery_excludes_independently_deleted_endpoints_and_keeps_rows');
    await page.screenshot({ path: testInfo.outputPath('endpoint-recovery.png'), fullPage: true });
    await lifecycle('删除项目');
    const receipt = await get(base() + '/lifecycle');
    expect(receipt.project.state).toBe('deleted');
    expect(
      receipt.tombstones.filter((t: { resource_type: string }) => t.resource_type === 'endpoint'),
    ).toHaveLength(4);
    expect(
      receipt.tombstones.every(
        (t: { physical_gc_state: string }) => t.physical_gc_state === 'held',
      ),
    ).toBe(true);
    await record('ui_finished_fixture_frees_runtime_quota_and_keeps_endpoint_tombstones');
    result = 'pass';
  } catch (e) {
    result = 'fail';
    throw e;
  } finally {
    await save();
    if (result !== 'pass' && project) {
      // Bounded normal retirement of this fixture only. Preserve original
      // Operations and data if a dependency or hardware failure blocks cleanup.
      const state = await context.request.get(storagePath(), { timeout: 10000 }).catch(() => null);
      if (state?.ok()) {
        const v = await state.json();
        if (v.state === 'active') {
          const r = await context.request
            .delete(storagePath(), {
              headers: {
                ...(await headers()),
                'If-Match': `"${v.generation}"`,
                'Idempotency-Key': 'failure-disable-' + attempt,
              },
              timeout: 10000,
            })
            .catch(() => null);
          if (r)
            cleanup.push({
              status: r.status(),
              ...(r.status() === 202 ? { operation_id: (await r.json()).operation.id } : {}),
            });
        }
      }
      for (const endpoint of endpoints) {
        const r = await context.request
          .post(`${base()}/endpoints/${endpoint}/suspend`, {
            headers: await headers(),
            timeout: 10000,
          })
          .catch(() => null);
        if (r)
          cleanup.push({
            endpoint_id: endpoint,
            status: r.status(),
            ...(r.status() === 202 ? { operation_id: (await r.json()).operation.id } : {}),
          });
      }
      await save();
    }
  }
});
