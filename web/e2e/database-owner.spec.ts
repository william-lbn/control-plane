import { expect, test, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Database owner transfer: actual SQL, replay, retained data and Reader cold configuration', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_CATALOG_OWNER_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!input || !admin || !privateDir) throw new Error('Protected Linux owner fixture required');
  const privateRoot = privateDir;
  const fixture = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
  };
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw new Error('Exact retained owner scope required');
  const base = `/api/v1/projects/${project}/branches/${branch}`,
    db = 'owner_transfer_v1',
    old = 'owner_old_v1',
    next = 'owner_new_v1';
  const passwordOld = randomBytes(30).toString('base64url'),
    passwordNext = randomBytes(30).toString('base64url');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(path.join(privateRoot, 'owner-old-password'), passwordOld, {
    mode: 0o600,
    flag: 'wx',
  });
  await writeFile(path.join(privateRoot, 'owner-new-password'), passwordNext, {
    mode: 0o600,
    flag: 'wx',
  });
  let result = 'running',
    reader = '';
  const checks: string[] = [],
    operations: string[] = [];
  async function save(name?: string) {
    if (name) checks.push(name);
    const report = {
      result,
      project_id: project,
      branch_id: branch,
      writer_id: writer,
      reader_id: reader,
      database: db,
      old_owner: old,
      new_owner: next,
      checks,
      operations,
      data_deleted: false,
      production_qualified: false,
      credentials_in_evidence: false,
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(report) + '\n');
    await writeFile(
      path.join(privateRoot, 'catalog-owner-fixture.json'),
      JSON.stringify({
        ...report,
        password_old_file: path.join(privateRoot, 'owner-old-password'),
        password_new_file: path.join(privateRoot, 'owner-new-password'),
      }) + '\n',
      { mode: 0o600 },
    );
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const accepted = await r.json();
    operations.push(accepted.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const response = await context.request.get(
            `/api/v1/projects/${project}/operations/${accepted.operation.id}`,
          );
          expect(response.status()).toBe(200);
          const op = await response.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Original catalog Operation failed: ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
  }
  async function mutate(button: string, url: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === url && r.request().method() === method,
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function sql(endpoint: string, role: string, password: string, query: string, want = 200) {
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint', { exact: true }).selectOption(endpoint);
    await page.getByLabel('数据库角色', { exact: true }).fill(role);
    await page.getByLabel('目标数据库', { exact: true }).fill(db);
    await page.getByLabel('SQL 查询').fill(query);
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(want);
    return response.json();
  }
  async function zero(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    await wait(
      await mutate('现在缩到 0', `/api/v1/projects/${project}/endpoints/${endpoint}/suspend`),
    );
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.goto(`/#/projects/${project}/databases`);
    for (const [role, password] of [
      [old, passwordOld],
      [next, passwordNext],
    ] as const) {
      await page.getByLabel('新角色名称', { exact: true }).fill(role);
      await page.getByLabel('新角色密码', { exact: true }).fill(password);
      await wait(await mutate('创建角色', base + '/roles'));
      await expect(page.getByLabel('新角色名称', { exact: true })).toHaveValue('');
    }
    await page.getByLabel('新数据库名称', { exact: true }).fill(db);
    await page.getByLabel('数据库 Owner', { exact: true }).selectOption(old);
    await wait(await mutate('创建数据库', base + '/databases'));
    await expect(page.getByLabel('新数据库名称', { exact: true })).toHaveValue('');
    await sql(
      writer,
      old,
      passwordOld,
      "CREATE TABLE public.owner_before(id integer PRIMARY KEY, marker text); INSERT INTO public.owner_before VALUES(1,'retained-owner-data')",
    );
    await save('owned_roles_database_and_actual_original_owner_data_created');
    await page.goto(`/#/projects/${project}/databases`);
    await page.getByLabel('变更 Owner 的数据库', { exact: true }).selectOption(db);
    await page.getByLabel('新的数据库 Owner', { exact: true }).selectOption(next);
    const response = await mutate('保存数据库 Owner', base + '/databases/' + db, 'PATCH');
    const headers = await response.request().allHeaders();
    const accepted = await wait(response);
    await expect(page.getByLabel('新的数据库 Owner', { exact: true })).toHaveValue('');
    await expect(
      page
        .locator('tr')
        .filter({ has: page.getByText(db, { exact: true }) })
        .first(),
    ).toContainText(next);
    const replayHeaders = {
      'X-CSRF-Token': headers['x-csrf-token'],
      'If-Match': headers['if-match'],
      'Idempotency-Key': headers['idempotency-key'],
    };
    const replay = await context.request.patch(base + '/databases/' + db, {
      headers: replayHeaders,
      data: { owner: next },
    });
    expect(replay.status()).toBe(202);
    expect((await replay.json()).operation.id).toBe(accepted.operation.id);
    const conflict = await context.request.patch(base + '/databases/' + db, {
      headers: replayHeaders,
      data: { owner: old },
    });
    expect(conflict.status()).toBe(409);
    expect((await conflict.json()).code).toBe('idempotency_conflict');
    const stale = await context.request.patch(base + '/databases/' + db, {
      headers: { ...replayHeaders, 'Idempotency-Key': 'owner-stale-' + project },
      data: { owner: old },
    });
    expect(stale.status()).toBe(412);
    await save('ui_owner_change_same_operation_replay_conflict_and_stale_rejected');
    const observation = await sql(
      writer,
      next,
      passwordNext,
      'SELECT current_user,pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()',
    );
    expect(observation.rows).toEqual([[next, next]]);
    expect(
      (await sql(writer, old, passwordOld, 'SELECT id,marker FROM public.owner_before')).rows,
    ).toEqual([[1, 'retained-owner-data']]);
    await sql(
      writer,
      next,
      passwordNext,
      'CREATE TABLE public.owner_after(id integer PRIMARY KEY); INSERT INTO public.owner_after VALUES(2)',
    );
    await sql(writer, old, passwordOld, 'INSERT INTO public.owner_after VALUES(3)', 422);
    await save('real_new_owner_login_create_and_original_object_ownership_acl_preserved');
    await zero(writer);
    expect(
      (
        await sql(
          writer,
          next,
          passwordNext,
          'SELECT current_user,pg_get_userbyid(datdba),(SELECT id FROM public.owner_after) FROM pg_database WHERE datname=current_database()',
        )
      ).rows,
    ).toEqual([[next, next, 2]]);
    await save('writer_cold_wake_keeps_new_owner_and_credentials');
    await page.goto(`/#/projects/${project}/branches/${branch}`);
    await page.getByRole('button', { name: '＋ 添加 Compute', exact: true }).click();
    await page.getByLabel('Compute 类型').selectOption('read_only');
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const create = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const added = await wait(await create);
    reader = added.resource.id;
    expect(reader).toMatch(/^ep_[a-f0-9]{16}$/);
    await save('owned_reader_created_from_updated_catalog');
    await expect(page.locator('.create-modal')).not.toBeVisible({ timeout: 30000 });
    await zero(writer);
    expect(
      (
        await sql(
          reader,
          next,
          passwordNext,
          'SELECT pg_is_in_recovery(),pg_get_userbyid(datdba),(SELECT id FROM public.owner_after) FROM pg_database WHERE datname=current_database()',
        )
      ).rows,
    ).toEqual([[true, next, 2]]);
    await zero(reader);
    expect(
      (
        await sql(
          reader,
          next,
          passwordNext,
          'SELECT pg_is_in_recovery(),pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()',
        )
      ).rows,
    ).toEqual([[true, next]]);
    await save('reader_creation_and_cold_wake_keep_owner_data_and_password');
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(reader);
    const endpoint = await context.request.get(`/api/v1/projects/${project}/endpoints/${reader}`);
    expect(endpoint.status()).toBe(200);
    const selector = (await endpoint.json()).selector;
    await page.getByRole('button', { name: '删除此 Compute', exact: true }).click();
    await page.getByLabel('Endpoint Selector 确认').fill(selector);
    await wait(
      await mutate('确认删除 Compute', `/api/v1/projects/${project}/endpoints/${reader}`, 'DELETE'),
    );
    await zero(writer);
    await save('owned_reader_normally_retired_data_roles_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
