import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ actionTimeout: 30000 });
test('Original default database Owner: durable authorization, cold replicas and fork identity', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!file || !adminFile || !privateDir) throw Error('Protected retained C02 fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8')),
    original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8')),
    catalog = JSON.parse(await readFile(fixture.resume_catalog_checkpoint_file, 'utf8'));
  const project = fixture.project_id,
    branch = catalog.branch_id,
    writer = catalog.writer_id,
    reader = original.reader_id;
  if (
    project !== catalog.project_id ||
    branch !== original.branch_id ||
    writer !== original.writer_id ||
    !/^ep_[a-f0-9]{16}$/.test(reader)
  )
    throw Error('Original C02 identity scope required');
  const root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch,
    url = base + '/default-database-owner',
    role = 'c02_identity_' + catalog.nonce;
  const password = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const state: any = fixture.resume_default_owner_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_default_owner_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        checks: [],
        operations: [],
        owner_operations: [],
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        reader_id: reader,
      };
  if (
    state.project_id !== project ||
    state.branch_id !== branch ||
    state.writer_id !== writer ||
    state.reader_id !== reader
  )
    throw Error('Original default Owner checkpoint scope changed');
  const schema = 'c02_default_owner_' + state.nonce,
    quote = (v: string) => '"' + v.replaceAll('"', '""') + '"';
  let result = 'running';
  const done = (v: string) => state.checks.includes(v);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'default-database-owner-checkpoint.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        reader_id: reader,
        role,
        role_oid: catalog.oids[role],
        database: original.platform_database,
        original_native: state.original_native,
        checks: state.checks,
        operations: state.operations,
        owner_operations: state.owner_operations,
        live_child_id: state.live_child_id,
        historical_child_id: state.historical_child_id,
        copied_excluded_role_oid: state.excluded_role_oid,
        original_replaced_role_oid: catalog.oids[role + '_replace'],
        recovered_fork_operations: state.recovered_fork_operations || [],
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function headers() {
    const csrf = (await context.cookies()).find((v) => v.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(target: string) {
    const r = await context.request.get(target, { timeout: 190000 });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function sql(
    statement: string,
    endpoint = writer,
    database = original.platform_database,
    loginRole = original.platform_role,
  ) {
    const r = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { role: loginRole, database, password, sql: statement },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function observe(id: string, owner = false) {
    if (!state.operations.includes(id)) state.operations.push(id);
    if (owner && !state.owner_operations.includes(id)) state.owner_operations.push(id);
    await save();
    await expect
      .poll(
        async () => {
          const v = await get(root + '/operations/' + id);
          if (v.state === 'failed') throw Error('Original default Owner Operation failed ' + id);
          return v.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function zero(endpoint: string, label: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state !== 'suspended') {
      const r = await context.request.post(root + '/endpoints/' + endpoint + '/suspend', {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-default-owner-' + state.nonce + '-zero-' + label,
        },
        data: {},
      });
      expect(r.status()).toBe(202);
      await observe((await r.json()).operation.id);
    }
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  function body(v: any, mode = 'managed') {
    return {
      mode,
      ...(mode === 'managed' ? { owner: role, owner_oid: catalog.oids[role] } : {}),
      expected_database_oid: v.database_oid,
      expected_previous_owner_oid: v.observed_owner_oid,
      acknowledge_owner_change: true,
      acknowledge_initial_observation: v.mode === 'uninitialized',
      password,
    };
  }
  async function patch(data: any, label: string, version: string, status: number) {
    const r = await context.request.patch(url, {
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-default-owner-' + state.nonce + '-' + label,
        'If-Match': '"' + version + '"',
      },
      data,
      timeout: 190000,
    });
    expect(r.status()).toBe(status);
    return r;
  }
  const nativeSQL = `SELECT d.oid::text,d.datdba::text,pg_get_userbyid(d.datdba),md5(COALESCE(shobj_description(d.oid,'pg_database'),'')),d.datallowconn,d.datistemplate,d.datconnlimit::text,n.oid::text,n.nspowner::text,c.oid::text,c.relowner::text,(SELECT marker FROM ${quote(schema)}.retained WHERE id=1) FROM pg_database d JOIN pg_namespace n ON n.nspname='${schema}' JOIN pg_class c ON c.relnamespace=n.oid AND c.relname='retained' WHERE d.datname=current_database()`;
  async function preserved(endpoint: string, ownerOID: string, ownerName: string) {
    const rows = (await sql(nativeSQL, endpoint)).rows;
    const expected = structuredClone(state.original_native);
    expected[0][1] = ownerOID;
    expected[0][2] = ownerName;
    expect(rows).toEqual(expected);
    return rows;
  }
  async function catalogPage() {
    await page.evaluate(
      ([key, value]) => sessionStorage.setItem(key, value),
      ['neon-control:catalog-branch:' + project, branch],
    );
    await page.goto('/#/projects/' + project + '/databases');
    await expect(page.getByRole('button', { name: '刷新目录', exact: true })).toBeEnabled({
      timeout: 190000,
    });
  }
  const form = page.getByRole('form', { name: '默认数据库 Owner', exact: true });
  async function readPanel() {
    await form.getByRole('button', { name: '读取默认数据库 Owner', exact: true }).click();
    await expect(
      form.getByRole('button', { name: '读取默认数据库 Owner', exact: true }),
    ).toBeEnabled({ timeout: 190000 });
    await expect(form.getByTestId('default-owner-current')).toContainText('OID');
  }
  async function uiTransfer() {
    await readPanel();
    await form.getByLabel('默认数据库目标 Owner', { exact: true }).selectOption(role);
    await form.getByLabel('当前平台角色密码', { exact: true }).fill(password);
    await form
      .getByLabel('我确认迁移原默认数据库 Owner，并保留原数据库和数据。', { exact: true })
      .check();
    await expect(
      form.getByRole('button', { name: '迁移默认数据库 Owner', exact: true }),
    ).toBeDisabled();
    await form
      .getByLabel('我确认把当前原 OID 观察记录为初始身份，早期历史尚未证明。', { exact: true })
      .check();
    await page.route(
      '**' + url,
      async (route) => {
        const data = route.request().postDataJSON();
        delete data.password;
        state.original_request = {
          key: route.request().headers()['idempotency-key'],
          version: route.request().headers()['if-match'],
          body_without_password: data,
        };
        await save();
        const response = await route.fetch({ timeout: 190000 });
        expect(response.status()).toBe(202);
        state.managed_operation = (await response.json()).operation.id;
        await save();
        await route.abort('failed');
      },
      { times: 1 },
    );
    await form.getByRole('button', { name: '迁移默认数据库 Owner', exact: true }).click();
    const retry = form.getByRole('button', { name: '重试原 Owner 提交', exact: true });
    await expect(retry).toBeEnabled({ timeout: 190000 });
    const response = page.waitForResponse(
      (r) => new URL(r.url()).pathname === url && r.request().method() === 'PATCH',
    );
    await retry.click();
    const value = await response;
    expect(value.status()).toBe(202);
    expect((await value.json()).operation.id).toBe(state.managed_operation);
    await observe(state.managed_operation, true);
    await expect(form.getByTestId('default-owner-operation')).toContainText('succeeded', {
      timeout: 190000,
    });
    await expect(form.getByLabel('当前平台角色密码', { exact: true })).toHaveValue('');
  }
  async function fork(kind: 'live' | 'historical') {
    if (state[kind + '_fork_verified']) return;
    const key = kind + '_child_id',
      endpointKey = kind + '_child_writer';
    if (!state[key]) {
      const response = await context.request.post(root + '/branches', {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-default-owner-' + state.nonce + '-' + kind,
        },
        data: {
          name: 'ci-default-owner-' + kind + '-' + state.nonce,
          parent_branch_id: branch,
          ...(kind === 'historical' ? { parent_lsn: state.before_owner_lsn } : {}),
          password,
          function_copy_policy: 'omit',
          autoscaling: {
            min_cpu_milli: 1000,
            max_cpu_milli: 1000,
            min_memory_mib: 1024,
            max_memory_mib: 1024,
          },
        },
        timeout: 190000,
      });
      expect(response.status()).toBe(202);
      const value = await response.json();
      expect(value.resource?.id).toMatch(/^br_[a-f0-9]{16}$/);
      state[key] = value.resource.id;
      state[kind + '_child_operation'] = value.operation.id;
      await save();
    }
    const originalFork = await get(root + '/operations/' + state[kind + '_child_operation']);
    if (originalFork.state === 'failed') {
      expect(originalFork.retryable).toBe(true);
      const retry = await context.request.post(root + '/operations/' + originalFork.id + '/retry', {
        headers: await headers(),
        data: {},
      });
      expect(retry.status()).toBe(202);
      expect((await retry.json()).id).toBe(originalFork.id);
      state.recovered_fork_operations ||= [];
      if (!state.recovered_fork_operations.includes(originalFork.id))
        state.recovered_fork_operations.push(originalFork.id);
      await save();
    }
    await observe(state[kind + '_child_operation']);
    if (!state[endpointKey]) {
      const endpoints = (await get(root + '/endpoints')).items;
      state[endpointKey] = endpoints.find(
        (v: any) => v.branch_id === state[key] && v.endpoint_type === 'read_write',
      )?.id;
      if (!state[endpointKey]) throw Error('Owned fork Writer missing');
      await save();
    }
    const expectedOID = kind === 'live' ? String(catalog.oids[role]) : state.original_native[0][1],
      expectedName = kind === 'live' ? role : original.platform_role;
    await preserved(state[endpointKey], expectedOID, expectedName);
    const preview = await get(root + '/branches/' + state[key] + '/default-database-owner');
    expect(preview.database_oid).toBe(Number(state.original_native[0][0]));
    expect(preview.observed_owner_oid).toBe(Number(expectedOID));
    expect(preview.mode).toBe(kind === 'live' ? 'managed' : 'uninitialized');
    if (kind === 'live') {
      expect(preview.owner).toBe(role);
      const childBase = root + '/branches/' + state[key],
        replacement = role + '_replace';
      const excluded = (await get(childBase + '/roles')).items.find(
        (v: any) => v.name === replacement,
      );
      expect(excluded.managed).toBe(false);
      expect(excluded.management_excluded).toBe(true);
      expect(excluded.state).toBe('external_identity');
      const actual = (
        await sql(
          "SELECT oid::text,COALESCE(shobj_description(oid,'pg_authid'),'')='neon-control/branch/" +
            branch +
            "' FROM pg_roles WHERE rolname='" +
            replacement +
            "'",
          state[endpointKey],
        )
      ).rows[0];
      expect(Number(actual[0])).not.toBe(catalog.oids[replacement]);
      expect(actual[1]).toBe(true);
      state.excluded_role_oid = Number(actual[0]);
      const actions: [string, string, unknown][] = [
        ['DELETE', childBase + '/roles/' + replacement, undefined],
        ['PATCH', childBase + '/roles/' + replacement, { password: password + '-must-not-apply' }],
        [
          'POST',
          childBase + '/roles/' + replacement + '/rename',
          { new_name: replacement + '_blocked' },
        ],
        [
          'POST',
          childBase + '/databases',
          { name: 'blocked_excluded_' + state.nonce, owner: replacement },
        ],
        [
          'POST',
          childBase + '/catalog-identities',
          {
            kind: 'role',
            name: replacement,
            oid: Number(actual[0]),
            acknowledge_initial_observation: true,
          },
        ],
      ];
      for (let index = 0; index < actions.length; index++) {
        const [method, target, data] = actions[index];
        const denied = await context.request.fetch(target, {
          method,
          headers: {
            ...(await headers()),
            'Idempotency-Key': 'c02-default-owner-' + state.nonce + '-excluded-' + index,
            'If-Match': '"' + excluded.version_token + '"',
          },
          ...(data ? { data } : {}),
        });
        expect(denied.status()).toBe(409);
        expect((await denied.json()).code).toBe('catalog_inheritance_identity_excluded');
      }
      const inventory = await get(childBase + '/catalog-identities'),
        unmanaged = inventory.native_objects.find(
          (v: any) => v.kind === 'role' && v.name === replacement,
        );
      expect(unmanaged.managed).toBe(false);
      expect(unmanaged.can_initialize).toBe(false);
      expect(unmanaged.management_excluded).toBe(true);
      await page.evaluate(
        ([key, value]) => sessionStorage.setItem(key, value),
        ['neon-control:catalog-branch:' + project, state[key]],
      );
      await page.reload();
      await expect(page.getByRole('button', { name: '刷新目录', exact: true })).toBeEnabled({
        timeout: 190000,
      });
      const row = page.getByRole('row').filter({ hasText: replacement });
      await expect(row).toContainText('平台管理已隔离');
      await expect(
        row.getByRole('button', { name: '删除角色 ' + replacement, exact: true }),
      ).toBeDisabled();
    }
    await zero(state[endpointKey], kind + '-child');
    state[kind + '_fork_verified'] = true;
    await save();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('original_default_identity_and_input_guards')) {
      const before = await get(url);
      expect(before.mode).toBe('uninitialized');
      expect(before.database).toBe(original.platform_database);
      expect(before.observed_owner).toBe(original.platform_role);
      await sql('CREATE SCHEMA IF NOT EXISTS ' + quote(schema));
      await sql(
        'CREATE TABLE IF NOT EXISTS ' +
          quote(schema) +
          '.retained(id integer PRIMARY KEY,marker text)',
      );
      await sql(
        'INSERT INTO ' +
          quote(schema) +
          ".retained VALUES(1,'retained-default-owner-data') ON CONFLICT(id) DO NOTHING",
      );
      state.original_native = (await sql(nativeSQL)).rows;
      state.before_owner_lsn = (await sql('SELECT pg_current_wal_flush_lsn()::text')).rows[0][0];
      await save();
      const input = body(before);
      await patch(
        { ...input, acknowledge_initial_observation: false },
        'missing-initial-ack',
        before.version_token,
        422,
      );
      await patch(
        { ...input, expected_database_oid: before.database_oid + 1 },
        'wrong-database-oid',
        before.version_token,
        409,
      );
      await patch(input, 'stale-version', '2', 412);
      const replaced = 'c02_identity_' + catalog.nonce + '_replace',
        replacementOID = Number(
          (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + replaced + "'")).rows[0][0],
        );
      await patch(
        { ...input, owner: replaced, owner_oid: replacementOID },
        'known-replacement',
        before.version_token,
        409,
      );
      const noCSRF = await context.request.patch(url, {
        headers: { 'Idempotency-Key': 'c02-owner-no-csrf-' + state.nonce, 'If-Match': '"1"' },
        data: input,
      });
      expect(noCSRF.status()).toBe(403);
      await save('original_default_identity_and_input_guards');
    }
    await catalogPage();
    if (!done('durable_ui_original_request_and_native_owner')) {
      if (state.managed_operation) {
        const prior = await get(root + '/operations/' + state.managed_operation);
        if (prior.state === 'failed') {
          expect(prior.retryable).toBe(true);
          await readPanel();
          await expect(form.getByTestId('default-owner-operation')).toContainText(
            state.managed_operation,
          );
          await expect(form.getByLabel('当前平台角色密码', { exact: true })).toHaveValue('');
          await form.getByRole('button', { name: '重试原 Owner 操作', exact: true }).click();
        }
        await observe(state.managed_operation, true);
      } else await uiTransfer();
      const value = await get(url);
      expect(value.mode).toBe('managed');
      expect(value.owner_oid).toBe(catalog.oids[role]);
      expect(value.version_token).toBe('2');
      await preserved(writer, String(catalog.oids[role]), role);
      await save('durable_ui_original_request_and_native_owner');
    }
    if (!done('default_owner_dependencies_and_independent_connection_defaults')) {
      const roles = (await get(base + '/roles')).items,
        r = roles.find((v: any) => v.name === role);
      const remove = await context.request.delete(base + '/roles/' + role, {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-owner-delete-selected-' + state.nonce,
          'If-Match': '"' + r.version_token + '"',
        },
      });
      expect(remove.status()).toBe(409);
      const rename = await context.request.post(base + '/roles/' + role + '/rename', {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-owner-rename-selected-' + state.nonce,
          'If-Match': '"' + r.version_token + '"',
        },
        data: { new_name: role + '_blocked' },
      });
      expect(rename.status()).toBe(409);
      const defaults = await get(base + '/connection-defaults');
      expect(defaults.mode).toBe('platform');
      expect(defaults.version_token).toBe('5');
      expect(defaults.database).toBe(original.platform_database);
      expect(defaults.role).toBe(original.platform_role);
      expect(
        (await sql('SELECT current_user', writer, original.platform_database, role)).rows[0][0],
      ).toBe(role);
      await save('default_owner_dependencies_and_independent_connection_defaults');
    }
    if (!done('cold_reader_preserves_original_oid_data_and_owners')) {
      await zero(reader, 'reader-before');
      await zero(writer, 'writer-before-reader');
      await preserved(reader, String(catalog.oids[role]), role);
      await zero(reader, 'reader-after');
      await save('cold_reader_preserves_original_oid_data_and_owners');
    }
    if (!done('cold_writer_preserves_original_oid_data_and_owners')) {
      await preserved(writer, String(catalog.oids[role]), role);
      await save('cold_writer_preserves_original_oid_data_and_owners');
    }
    if (!done('live_and_historical_fork_owner_intent')) {
      await fork('live');
      await fork('historical');
      await save('live_and_historical_fork_owner_intent');
    }
    if (!done('platform_restore_and_old_receipt_do_not_revert_new_owner')) {
      if (!state.platform_operation) {
        const current = await get(url),
          response = await patch(
            body(current, 'platform'),
            'restore-platform',
            current.version_token,
            202,
          );
        state.platform_operation = (await response.json()).operation.id;
        await save();
      }
      await observe(state.platform_operation, true);
      await preserved(writer, state.original_native[0][1], original.platform_role);
      const originalRequest = state.original_request;
      if (!originalRequest) throw Error('Original lost-ack request receipt required');
      const replay = await context.request.patch(url, {
        headers: {
          ...(await headers()),
          'Idempotency-Key': originalRequest.key,
          'If-Match': originalRequest.version,
        },
        data: { ...originalRequest.body_without_password, password },
        timeout: 190000,
      });
      expect(replay.status()).toBe(202);
      expect((await replay.json()).operation.id).toBe(state.managed_operation);
      const value = await get(url);
      expect(value.mode).toBe('platform');
      expect(value.version_token).toBe('3');
      expect(value.observed_owner_oid).toBe(Number(state.original_native[0][1]));
      await save('platform_restore_and_old_receipt_do_not_revert_new_owner');
    }
    if (!done('original_customer_data_retained_and_all_computes_zero')) {
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            writer,
            'c02_default_' + original.nonce,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      await zero(writer, 'final-writer');
      await zero(reader, 'final-reader');
      await save('original_customer_data_retained_and_all_computes_zero');
    }
    result = 'pass';
  } finally {
    await save();
  }
});
