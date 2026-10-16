import { expect, test, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ actionTimeout: 30_000 });

// Credentials and resumable inputs stay outside the checkout. A failed run is
// continued on these same objects; it must never manufacture a replacement DB.
test('Catalog rename: UI intent recovery, original OIDs, password, data and cold Reader', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_CATALOG_RENAME_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!input || !admin || !privateDir) throw new Error('Protected Linux rename fixture required');
  const privateRoot = privateDir;
  const fixture = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    resume?: {
      password_file: string;
      checks: string[];
      operations: string[];
      reader_id: string;
      original_rows?: unknown[][];
      acl_rows?: unknown[][];
      original_failure_job: string;
    };
  };
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw new Error('Exact retained rename scope required');
  const base = `/api/v1/projects/${project}/branches/${branch}`,
    roleOld = 'rename_role_v1',
    roleNew = 'renamed_role_v1',
    dbOld = 'rename_database_v1',
    dbNew = 'renamed_database_v1';
  const password = fixture.resume
    ? (await readFile(fixture.resume.password_file, 'utf8')).trim()
    : randomBytes(30).toString('base64url');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  const passwordFile = path.join(privateRoot, 'rename-role-password');
  if (!fixture.resume) await writeFile(passwordFile, password, { mode: 0o600, flag: 'wx' });
  let result = 'running',
    reader = fixture.resume?.reader_id ?? '',
    originalRows = fixture.resume?.original_rows,
    aclRows = fixture.resume?.acl_rows;
  const checks = [...(fixture.resume?.checks ?? [])],
    operations = [...(fixture.resume?.operations ?? [])];
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    const report = {
      result,
      project_id: project,
      branch_id: branch,
      writer_id: writer,
      reader_id: reader,
      old_role: roleOld,
      new_role: roleNew,
      old_database: dbOld,
      new_database: dbNew,
      original_rows: originalRows,
      acl_rows: aclRows,
      checks,
      operations,
      data_deleted: false,
      credentials_in_evidence: false,
      production_qualified: false,
      ...(fixture.resume ? { original_failure_job: fixture.resume.original_failure_job } : {}),
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(report) + '\n');
    await writeFile(
      path.join(privateRoot, 'catalog-rename-checkpoint.json'),
      JSON.stringify({ ...report, password_file: fixture.resume?.password_file ?? passwordFile }) +
        '\n',
      { mode: 0o600 },
    );
  }
  async function observe(id: string) {
    if (!operations.includes(id)) operations.push(id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${project}/operations/${id}`);
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Original Operation failed: ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const value = await r.json();
    await observe(value.operation.id);
    return value;
  }
  async function mutate(button: string, url: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === url && r.request().method() === method,
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function sql(endpoint: string, role: string, database: string, query: string) {
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(endpoint);
    await page.getByLabel('数据库角色', { exact: true }).fill(role);
    await page.getByLabel('目标数据库', { exact: true }).fill(database);
    await page.getByLabel('SQL 查询').fill(query);
    await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${endpoint}/query` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function zero(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    const r = await context.request.get(`/api/v1/projects/${project}/endpoints/${endpoint}`);
    expect(r.status()).toBe(200);
    if ((await r.json()).observed_state !== 'suspended')
      await wait(
        await mutate('现在缩到 0', `/api/v1/projects/${project}/endpoints/${endpoint}/suspend`),
      );
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
  }
  const identitySQL = `SELECT (SELECT oid::text FROM pg_roles WHERE rolname=current_user),oid::text,pg_get_userbyid(datdba),(SELECT marker FROM public.rename_probe WHERE id=1) FROM pg_database WHERE datname=current_database()`;
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!checks.includes('owned_role_created')) {
      await page.goto(`/#/projects/${project}/databases`);
      await page.getByLabel('新角色名称', { exact: true }).fill(roleOld);
      await page.getByLabel('新角色密码', { exact: true }).fill(password);
      await wait(await mutate('创建角色', base + '/roles'));
      await expect(page.getByLabel('新角色名称', { exact: true })).toHaveValue('');
      await save('owned_role_created');
    }
    if (!checks.includes('owned_database_created')) {
      await page.goto(`/#/projects/${project}/databases`);
      await page.getByLabel('新数据库名称', { exact: true }).fill(dbOld);
      await page.getByLabel('数据库 Owner', { exact: true }).selectOption(roleOld);
      await wait(await mutate('创建数据库', base + '/databases'));
      await expect(page.getByLabel('新数据库名称', { exact: true })).toHaveValue('');
      await save('owned_database_created');
    }
    if (!checks.includes('original_oid_acl_and_data_recorded')) {
      await sql(
        writer,
        roleOld,
        dbOld,
        'CREATE TABLE IF NOT EXISTS public.rename_probe(id integer PRIMARY KEY, marker text NOT NULL)',
      );
      await sql(
        writer,
        roleOld,
        dbOld,
        "INSERT INTO public.rename_probe VALUES(1,'retained-rename-business-data') ON CONFLICT(id) DO NOTHING",
      );
      originalRows = (await sql(writer, roleOld, dbOld, identitySQL)).rows;
      expect(originalRows?.[0]?.[2]).toBe(roleOld);
      expect(originalRows?.[0]?.[3]).toBe('retained-rename-business-data');
      aclRows = (
        await sql(
          writer,
          roleOld,
          dbOld,
          "SELECT relacl::text FROM pg_class WHERE oid='public.rename_probe'::regclass",
        )
      ).rows;
      await save('original_oid_acl_and_data_recorded');
    }
    if (!checks.includes('original_reader_created_and_suspended')) {
      if (!reader) {
        await page.goto(`/#/projects/${project}/branches/${branch}`);
        await page.getByRole('button', { name: '＋ 添加 Compute', exact: true }).click();
        await page.getByLabel('Compute 类型').selectOption('read_only');
        await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
        await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
        const accepted = await wait(
          await mutate('创建并验证', `/api/v1/projects/${project}/endpoints`),
        );
        reader = accepted.resource.id;
        await save();
        await expect(page.locator('.create-modal')).not.toBeVisible({ timeout: 30000 });
      }
      expect(reader).toMatch(/^ep_[a-f0-9]{16}$/);
      await zero(reader);
      await save('original_reader_created_and_suspended');
    }
    if (!checks.includes('role_ui_lost_reply_replayed_same_operation')) {
      await page.goto(`/#/projects/${project}/databases`);
      await page.getByLabel('重命名对象类型').selectOption('roles');
      await page.getByLabel('重命名原对象').selectOption(roleOld);
      await page.getByLabel('重命名新名称').fill(roleNew);
      await page.getByLabel('确认更新应用连接名称').check();
      const url = base + '/roles/' + roleOld + '/rename';
      let acceptedId = '',
        originalHeaders: Record<string, string> = {};
      await page.route(
        '**' + url,
        async (route) => {
          if (route.request().method() !== 'POST') return route.continue();
          originalHeaders = await route.request().allHeaders();
          const response = await route.fetch();
          expect(response.status()).toBe(202);
          acceptedId = (await response.json()).operation.id;
          await save();
          await route.abort('failed');
        },
        { times: 1 },
      );
      await page.getByRole('button', { name: '提交重命名', exact: true }).click();
      await expect(
        page.getByRole('button', { name: '重试原重命名请求', exact: true }),
      ).toBeVisible();
      const recovered = await wait(await mutate('重试原重命名请求', url));
      expect(recovered.operation.id).toBe(acceptedId);
      await expect(page.getByLabel('重命名原对象')).toHaveValue('');
      const headers = {
        'X-CSRF-Token': originalHeaders['x-csrf-token'],
        'If-Match': originalHeaders['if-match'],
        'Idempotency-Key': originalHeaders['idempotency-key'],
      };
      const conflict = await context.request.post(url, {
        headers,
        data: { new_name: roleNew + '_other' },
      });
      expect(conflict.status()).toBe(409);
      expect((await conflict.json()).code).toBe('idempotency_conflict');
      const stale = await context.request.post(base + '/roles/' + roleNew + '/rename', {
        headers: { ...headers, 'Idempotency-Key': 'rename-stale-' + project },
        data: { new_name: roleOld },
      });
      expect(stale.status()).toBe(412);
      await save('role_ui_lost_reply_replayed_same_operation');
    }
    if (!checks.includes('original_role_oid_password_owner_and_acl_preserved')) {
      const rows = (await sql(writer, roleNew, dbOld, identitySQL)).rows;
      expect(rows).toEqual([
        [originalRows![0][0], originalRows![0][1], roleNew, 'retained-rename-business-data'],
      ]);
      expect(
        (
          await sql(
            writer,
            roleNew,
            dbOld,
            "SELECT relacl::text FROM pg_class WHERE oid='public.rename_probe'::regclass",
          )
        ).rows,
      ).toEqual(aclRows);
      const inventory = await context.request.get(base + '/databases');
      expect(inventory.status()).toBe(200);
      expect(
        (await inventory.json()).items.find((d: { name: string }) => d.name === dbOld)?.owner,
      ).toBe(roleNew);
      await save('original_role_oid_password_owner_and_acl_preserved');
    }
    if (!checks.includes('database_ui_renamed')) {
      await page.goto(`/#/projects/${project}/databases`);
      await page.getByLabel('重命名对象类型').selectOption('databases');
      await page.getByLabel('重命名原对象').selectOption(dbOld);
      await page.getByLabel('重命名新名称').fill(dbNew);
      await page.getByLabel('确认更新应用连接名称').check();
      await wait(await mutate('提交重命名', base + '/databases/' + dbOld + '/rename'));
      await expect(page.getByLabel('重命名原对象')).toHaveValue('');
      await save('database_ui_renamed');
    }
    if (!checks.includes('writer_cold_wake_original_oids_password_and_data')) {
      await zero(writer);
      const rows = (await sql(writer, roleNew, dbNew, identitySQL)).rows;
      expect(rows).toEqual([
        [originalRows![0][0], originalRows![0][1], roleNew, 'retained-rename-business-data'],
      ]);
      await save('writer_cold_wake_original_oids_password_and_data');
    }
    if (!checks.includes('existing_reader_cold_wake_updated_catalog_and_proxy')) {
      await zero(writer);
      const rows = (await sql(reader, roleNew, dbNew, identitySQL)).rows;
      expect(rows).toEqual([
        [originalRows![0][0], originalRows![0][1], roleNew, 'retained-rename-business-data'],
      ]);
      expect((await sql(reader, roleNew, dbNew, 'SELECT pg_is_in_recovery()')).rows).toEqual([
        [true],
      ]);
      await save('existing_reader_cold_wake_updated_catalog_and_proxy');
    }
    if (!checks.includes('owned_reader_retired_and_writer_zero_data_retained')) {
      await page.goto(`/#/projects/${project}/compute`);
      await page.locator('select').first().selectOption(reader);
      const r = await context.request.get(`/api/v1/projects/${project}/endpoints/${reader}`);
      expect(r.status()).toBe(200);
      const selector = (await r.json()).selector;
      await page.getByRole('button', { name: '删除此 Compute', exact: true }).click();
      await page.getByLabel('Endpoint Selector 确认').fill(selector);
      await wait(
        await mutate(
          '确认删除 Compute',
          `/api/v1/projects/${project}/endpoints/${reader}`,
          'DELETE',
        ),
      );
      await zero(writer);
      await save('owned_reader_retired_and_writer_zero_data_retained');
    }
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
