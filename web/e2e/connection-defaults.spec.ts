import { expect, test } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ actionTimeout: 30_000 });

test('Customer defaults: original reply recovery, actual SQL, Reader and branch inheritance', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!file || !adminFile || !privateDir)
    throw Error('Protected Linux connection defaults fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8')) as {
    project_id: string;
    parent_branch_id: string;
    resume_checkpoint_file?: string;
    recovery_operation_id?: string;
    recovery_branch_id?: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.parent_branch_id)
  )
    throw Error('Exact retained parent scope required');
  const project = fixture.project_id,
    root = `/api/v1/projects/${project}`;
  const state: any = fixture.resume_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        checks: [],
        operations: [],
        password_file: path.join(privateDir, 'customer-default-password'),
      };
  if (!fixture.resume_checkpoint_file)
    await writeFile(state.password_file, randomBytes(30).toString('base64url'), {
      mode: 0o600,
      flag: 'wx',
    });
  const password = (await readFile(state.password_file, 'utf8')).trim();
  const role = 'c02_customer_' + state.nonce,
    database = 'c02_default_' + state.nonce;
  let result = 'running';
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'connection-defaults-checkpoint.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        parent_branch_id: fixture.parent_branch_id,
        branch_id: state.branch_id,
        writer_id: state.writer_id,
        reader_id: state.reader_id,
        child_id: state.child_id,
        role,
        database,
        checks: state.checks,
        operations: state.operations,
        native_identity: state.native_identity,
        platform_identity: { role: state.platform_role, database: state.platform_database },
        data_deleted: false,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  const done = (check: string) => state.checks.includes(check);
  async function headers() {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF cookie required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(url: string) {
    const r = await context.request.get(url);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function observe(id: string) {
    if (!state.operations.includes(id)) {
      state.operations.push(id);
      await save();
    }
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Original operation failed: ' + id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function create(url: string, label: string, body: unknown) {
    const r = await context.request.post(url, {
      headers: { ...(await headers()), 'Idempotency-Key': 'c02-' + state.nonce + '-' + label },
      data: body,
    });
    expect(r.status()).toBe(202);
    const value = await r.json();
    await save();
    await observe(value.operation.id);
    return value;
  }
  async function sql(endpoint: string, statement: string, explicit = false) {
    const r = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { password, sql: statement, ...(explicit ? { role, database } : {}) },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function zero(endpoint: string, phase: string) {
    const e = await get(root + '/endpoints/' + endpoint);
    if (e.observed_state !== 'suspended')
      await create(
        root + '/endpoints/' + endpoint + '/suspend',
        'zero-' + endpoint + '-' + phase,
        {},
      );
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  async function catalog() {
    // Restore only the previous tab's non-secret branch preference on resume.
    // Every catalog/defaults response still comes from the actual owned branch.
    await page.evaluate(
      ([key, value]) => sessionStorage.setItem(key, value),
      ['neon-control:catalog-branch:' + project, state.branch_id],
    );
    await page.goto(`/#/projects/${project}/databases`);
    await page.getByLabel('管理分支', { exact: true }).selectOption(state.branch_id);
    await expect(page.getByRole('button', { name: '刷新目录', exact: true })).toBeEnabled({
      timeout: 190000,
    });
  }
  const identitySQL = `SELECT current_user,current_database(),(SELECT oid::text FROM pg_roles WHERE rolname=current_user),(SELECT oid::text FROM pg_database WHERE datname=current_database()),(SELECT marker FROM public.customer_default_probe WHERE id=1)`;
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (state.parent_catalog_cleanup === undefined) {
      const parentWriter = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === fixture.parent_branch_id && e.endpoint_type === 'read_write',
      );
      if (!parentWriter) throw Error('Original parent Writer required');
      state.parent_writer_id = parentWriter.id;
      state.parent_catalog_cleanup = parentWriter.observed_state === 'suspended';
      await save();
    }
    if (!state.branch_id) {
      const value = await create(root + '/branches', 'branch', {
        name: 'ci-c02-defaults-' + state.nonce,
        parent_branch_id: fixture.parent_branch_id,
        password,
        function_copy_policy: 'omit',
        autoscaling: {
          min_cpu_milli: 1000,
          max_cpu_milli: 1000,
          min_memory_mib: 1024,
          max_memory_mib: 1024,
        },
      });
      state.branch_id = value.resource.id;
      await save();
    }
    const branch = state.branch_id,
      base = root + '/branches/' + branch;
    state.writer_id = (await get(root + '/endpoints')).items.find(
      (e: any) => e.branch_id === branch && e.endpoint_type === 'read_write',
    ).id;
    if (!state.platform_role || !state.platform_database) {
      const original = await get(root + '/endpoints/' + state.writer_id);
      state.platform_role = original.role_name;
      state.platform_database = original.database_name;
    }
    await save();
    if (!done('owned_native_catalog_ready')) {
      await create(base + '/roles', 'role', { name: role, password });
      await create(base + '/databases', 'database', { name: database, owner: role });
      await sql(
        state.writer_id,
        'CREATE TABLE IF NOT EXISTS public.customer_default_probe(id integer PRIMARY KEY, marker text NOT NULL)',
        true,
      );
      await sql(
        state.writer_id,
        "INSERT INTO public.customer_default_probe VALUES(1,'retained-c02-default-data') ON CONFLICT(id) DO NOTHING",
        true,
      );
      state.native_identity = (await sql(state.writer_id, identitySQL, true)).rows;
      expect(state.native_identity[0][0]).toBe(role);
      expect(state.native_identity[0][1]).toBe(database);
      await save('owned_native_catalog_ready');
    }
    if (!done('lost_ack_replayed_original_customer_defaults')) {
      await catalog();
      const panel = page.getByRole('form', { name: '客户连接默认值', exact: true });
      await panel.getByLabel('默认连接角色', { exact: true }).selectOption(role);
      await panel.getByLabel('默认连接数据库', { exact: true }).selectOption(database);
      await panel.getByLabel('确认后续连接使用所选默认值和对应角色密码').check();
      const url = base + '/connection-defaults';
      let originalKey = '',
        originalBody = '',
        acceptedVersion = '';
      await page.route(
        '**' + url,
        async (route) => {
          if (route.request().method() !== 'PATCH') return route.continue();
          originalKey = route.request().headers()['idempotency-key'];
          originalBody = route.request().postData() || '';
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          acceptedVersion = (await response.json()).version_token;
          await route.abort('failed');
        },
        { times: 1 },
      );
      await panel.getByRole('button', { name: '保存连接默认值', exact: true }).click();
      await expect(
        panel.getByRole('button', { name: '重试原连接默认值提交', exact: true }),
      ).toBeVisible();
      const response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'PATCH',
      );
      await panel.getByRole('button', { name: '重试原连接默认值提交', exact: true }).click();
      const replay = await response;
      expect(replay.status()).toBe(200);
      expect(
        replay.request().headers()['idempotency-key'] === originalKey &&
          replay.request().postData() === originalBody,
      ).toBe(true);
      expect((await replay.json()).version_token).toBe(acceptedVersion);
      await expect(panel.getByTestId('connection-default-current')).toHaveText(
        role + ' / ' + database,
      );
      await save('lost_ack_replayed_original_customer_defaults');
    }
    if (!done('connection_info_sql_and_script_use_customer_identity')) {
      const connection = await get(root + '/endpoints/' + state.writer_id + '/connection-info');
      expect(connection.role).toBe(role);
      expect(connection.database).toBe(database);
      expect(connection.password_included).toBe(false);
      expect((await sql(state.writer_id, identitySQL)).rows).toEqual(state.native_identity);
      const script = await context.request.post(
        root + '/endpoints/' + state.writer_id + '/query-script',
        {
          headers: await headers(),
          data: { password, sql: 'SELECT current_user,current_database();' },
          timeout: 190000,
        },
      );
      expect(script.status()).toBe(200);
      expect((await script.json()).outcome).toBe('completed');
      await page.goto(`/#/projects/${project}/query`);
      await page.getByLabel('目标 Endpoint').selectOption(state.writer_id);
      await expect(page.getByLabel('数据库角色', { exact: true })).toHaveValue(role);
      await expect(page.getByLabel('目标数据库', { exact: true })).toHaveValue(database);
      await save('connection_info_sql_and_script_use_customer_identity');
    }
    if (!done('default_dependencies_reject_delete_and_rename')) {
      for (const kind of ['roles', 'databases']) {
        const name = kind === 'roles' ? role : database;
        const deleted = await context.request.delete(base + '/' + kind + '/' + name, {
          headers: {
            ...(await headers()),
            'Idempotency-Key': 'c02-' + state.nonce + '-delete-' + kind,
          },
        });
        expect(deleted.status()).toBe(409);
        expect((await deleted.json()).code).toBe('connection_default_dependency');
        const items = await get(base + '/' + kind),
          selected = items.items.find((v: any) => v.name === name);
        const renamed = await context.request.post(base + '/' + kind + '/' + name + '/rename', {
          headers: {
            ...(await headers()),
            'Idempotency-Key': 'c02-' + state.nonce + '-rename-' + kind,
            'If-Match': `"${selected.version_token}"`,
          },
          data: { new_name: name + '_other' },
        });
        expect(renamed.status()).toBe(409);
        expect((await renamed.json()).code).toBe('connection_default_dependency');
      }
      await save('default_dependencies_reject_delete_and_rename');
    }
    if (!done('retained_reader_uses_same_oid_password_and_defaults')) {
      if (!state.reader_id) {
        const v = await create(root + '/endpoints', 'reader', {
          branch_id: branch,
          type: 'read_only',
          autoscaling: {
            min_cpu_milli: 1000,
            max_cpu_milli: 1000,
            min_memory_mib: 1024,
            max_memory_mib: 1024,
          },
        });
        state.reader_id = v.resource.id;
        await save();
      }
      const connection = await get(root + '/endpoints/' + state.reader_id + '/connection-info');
      expect(connection.role).toBe(role);
      expect(connection.database).toBe(database);
      expect((await sql(state.reader_id, identitySQL)).rows).toEqual(state.native_identity);
      await zero(state.reader_id, 'reader');
      await zero(state.writer_id, 'before-child');
      await save('retained_reader_uses_same_oid_password_and_defaults');
    }
    if (!done('live_fork_inherits_customer_defaults_and_native_oids')) {
      if (state.parent_catalog_cleanup && !state.pre_child_parent_zero) {
        await zero(state.parent_writer_id, 'before-child-parent');
        state.pre_child_parent_zero = true;
        await save();
      }
      if (fixture.recovery_operation_id) {
        const id = fixture.recovery_operation_id;
        if (!/^op_[a-f0-9]{24}$/.test(id) || !state.operations.includes(id))
          throw Error('Original checkpoint operation required for explicit recovery');
        const op = await get(root + '/operations/' + id);
        expect(op.action).toBe('create_branch');
        expect(op.resource_id).toBe(fixture.recovery_branch_id);
        if (op.state === 'failed') {
          expect(op.retryable).toBe(true);
          await page.goto(`/#/projects/${project}/operations`);
          await page
            .getByRole('button', { name: '查看操作 ' + id + ' 的步骤', exact: true })
            .click();
          const response = page.waitForResponse(
            (r) =>
              new URL(r.url()).pathname === root + '/operations/' + id + '/retry' &&
              r.request().method() === 'POST',
          );
          await page.getByRole('button', { name: '重试原操作', exact: true }).click();
          const retried = await response;
          expect(retried.status()).toBe(202);
          expect((await retried.json()).id).toBe(id);
        }
        await observe(id);
      }
      if (!state.child_id) {
        const v = await create(root + '/branches', 'child', {
          name: 'ci-c02-child-' + state.nonce,
          parent_branch_id: branch,
          password,
          function_copy_policy: 'omit',
          autoscaling: {
            min_cpu_milli: 1000,
            max_cpu_milli: 1000,
            min_memory_mib: 1024,
            max_memory_mib: 1024,
          },
        });
        state.child_id = v.resource.id;
        await save();
      }
      const childWriter = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.child_id && e.endpoint_type === 'read_write',
      );
      state.child_writer_id = childWriter.id;
      await save();
      const defaults = await get(root + '/branches/' + state.child_id + '/connection-defaults');
      expect(defaults.role).toBe(role);
      expect(defaults.database).toBe(database);
      expect((await sql(childWriter.id, identitySQL)).rows).toEqual(state.native_identity);
      await zero(childWriter.id, 'child');
      await save('live_fork_inherits_customer_defaults_and_native_oids');
    }
    if (!done('managed_role_can_keep_original_platform_database')) {
      await catalog();
      const panel = page.getByRole('form', { name: '客户连接默认值', exact: true });
      await panel.getByLabel('连接默认值模式', { exact: true }).selectOption('managed');
      await panel.getByLabel('默认连接角色', { exact: true }).selectOption(role);
      await panel
        .getByLabel('默认连接数据库', { exact: true })
        .selectOption(state.platform_database);
      await panel.getByLabel('确认后续连接使用所选默认值和对应角色密码').check();
      const response = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/connection-defaults' &&
          r.request().method() === 'PATCH',
      );
      await panel.getByRole('button', { name: '保存连接默认值', exact: true }).click();
      expect((await response).status()).toBe(200);
      await expect(panel.getByTestId('connection-default-current')).toHaveText(
        role + ' / ' + state.platform_database,
      );
      await expect(
        page.getByRole('button', { name: '删除数据库 ' + state.platform_database, exact: true }),
      ).toBeDisabled();
      const connection = await get(root + '/endpoints/' + state.writer_id + '/connection-info');
      expect(connection.role).toBe(role);
      expect(connection.database).toBe(state.platform_database);
      expect((await sql(state.writer_id, 'SELECT current_user,current_database()')).rows).toEqual([
        [role, state.platform_database],
      ]);
      await save('managed_role_can_keep_original_platform_database');
    }
    if (!done('platform_restore_keeps_native_data_and_bootstrap_identity')) {
      await catalog();
      const panel = page.getByRole('form', { name: '客户连接默认值', exact: true });
      await panel.getByLabel('连接默认值模式', { exact: true }).selectOption('platform');
      await panel.getByLabel('确认后续连接使用所选默认值和对应角色密码').check();
      const response = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/connection-defaults' &&
          r.request().method() === 'PATCH',
      );
      await panel.getByRole('button', { name: '保存连接默认值', exact: true }).click();
      expect((await response).status()).toBe(200);
      await expect(panel.getByTestId('connection-default-current')).toHaveText(
        state.platform_role + ' / ' + state.platform_database,
      );
      const connection = await get(root + '/endpoints/' + state.writer_id + '/connection-info');
      expect(connection.role).toBe(state.platform_role);
      expect(connection.database).toBe(state.platform_database);
      expect((await sql(state.writer_id, identitySQL, true)).rows).toEqual(state.native_identity);
      await zero(state.writer_id, 'final');
      await save('platform_restore_keeps_native_data_and_bootstrap_identity');
    }
    // The catalog page initially reads the default branch before switching to
    // this owned branch. Return only that originally suspended Writer to zero.
    if (state.parent_catalog_cleanup && !done('original_parent_catalog_wake_returned_to_zero')) {
      await zero(state.parent_writer_id, 'parent-catalog');
      await save('original_parent_catalog_wake_returned_to_zero');
    }
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
