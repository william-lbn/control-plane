import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Original rename receipts preserve later names, passwords and original data', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile)
    throw Error('Protected retained original catalog fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')),
    original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8')),
    prior = JSON.parse(await readFile(fixture.resume_catalog_mutation_checkpoint_file, 'utf8'));
  const project = prior.project_id,
    branch = prior.branch_id,
    writer = prior.writer_id,
    reader = prior.reader_id;
  if (
    project !== fixture.project_id ||
    branch !== original.branch_id ||
    writer !== original.writer_id ||
    reader !== original.reader_id ||
    prior.checks.length !== 5
  )
    throw Error('Original completed catalog fixture required');
  const platformPassword = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const state: any = fixture.resume_catalog_rename_result_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_rename_result_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        original_nonce: prior.nonce,
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        reader_id: reader,
        checks: [],
        operations: {},
        versions: {},
      };
  if (
    state.project_id !== project ||
    state.branch_id !== branch ||
    state.writer_id !== writer ||
    state.reader_id !== reader ||
    state.original_nonce !== prior.nonce
  )
    throw Error('Original rename fixture changed');
  const root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch,
    oldRole = 'c02_receipt_' + prior.nonce,
    firstRole = oldRole + '_renamed',
    role = oldRole + '_final',
    oldDatabase = oldRole + '_db',
    firstDatabase = oldDatabase + '_renamed',
    database = oldDatabase + '_final';
  const rolePassword = (generation: number) =>
    platformPassword + '-receipt-' + generation + '-' + prior.nonce;
  let result = 'running';
  const done = (v: string) => state.checks.includes(v);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-rename-results-checkpoint.json'),
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
        original_role: oldRole,
        original_database: oldDatabase,
        role,
        database,
        native_oids: prior.native_oids,
        checks: state.checks,
        operations: state.operations,
        versions: state.versions,
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
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (op.state === 'failed') throw Error('Original rename Operation failed ' + id);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function mutate(
    method: string,
    target: string,
    label: string,
    data: unknown,
    version?: string,
  ) {
    const r = await context.request.fetch(target, {
      method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-rename-result-' + state.nonce + '-' + label,
        ...(version ? { 'If-Match': '"' + version + '"' } : {}),
      },
      data,
      timeout: 190000,
    });
    expect(r.status()).toBe(202);
    const value = await r.json();
    if (state.operations[label]) expect(value.operation.id).toBe(state.operations[label]);
    else {
      state.operations[label] = value.operation.id;
      await save();
    }
    await observe(value.operation.id);
    return value;
  }
  async function current(kind: string, name: string) {
    const row = (await get(base + '/' + kind)).items.find((v: any) => v.name === name);
    expect(row).toBeTruthy();
    return row;
  }
  async function version(label: string, kind: string, name: string) {
    if (!state.versions[label]) {
      state.versions[label] = (await current(kind, name)).version_token;
      await save();
    }
    return state.versions[label];
  }
  async function sql(
    statement: string,
    endpoint = writer,
    target = original.platform_database,
    loginRole = original.platform_role,
    password = platformPassword,
  ) {
    const r = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { role: loginRole, database: target, password, sql: statement },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function zero(endpoint: string, label: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state !== 'suspended')
      await mutate('POST', root + '/endpoints/' + endpoint + '/suspend', 'zero-' + label, {});
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  async function native(endpoint = writer, target = database) {
    const row = (
      await sql(
        "SELECT oid::text,pg_get_userbyid(datdba) FROM pg_database WHERE datname='" + target + "'",
        endpoint,
      )
    ).rows[0];
    expect(row[0]).toBe(prior.native_oids.database);
    expect(row[1]).toBe(role);
    expect(
      (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + role + "'", endpoint))
        .rows[0][0],
    ).toBe(prior.native_oids.role);
    expect(
      (await sql('SELECT marker FROM public.catalog_receipt_probe WHERE id=1', endpoint, target))
        .rows[0][0],
    ).toBe('retained-catalog-receipt-data');
    expect(
      (await sql('SELECT current_user', endpoint, target, role, rolePassword(3))).rows[0][0],
    ).toBe(role);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('retained_original_catalog_scope_and_oids')) {
      expect((await current('roles', oldRole)).version_token).toBe('4');
      expect((await current('databases', oldDatabase)).version_token).toBe('4');
      expect(
        (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + oldRole + "'")).rows[0][0],
      ).toBe(prior.native_oids.role);
      expect(
        (await sql("SELECT oid::text FROM pg_database WHERE datname='" + oldDatabase + "'"))
          .rows[0][0],
      ).toBe(prior.native_oids.database);
      expect(
        (
          await sql(
            'SELECT marker FROM public.catalog_receipt_probe WHERE id=1',
            writer,
            oldDatabase,
            oldRole,
            rolePassword(2),
          )
        ).rows[0][0],
      ).toBe('retained-catalog-receipt-data');
      await save('retained_original_catalog_scope_and_oids');
    }
    if (!done('later_role_name_password_and_original_rename_receipt')) {
      const originalVersion = await version('role_first', 'roles', oldRole);
      await mutate(
        'POST',
        base + '/roles/' + oldRole + '/rename',
        'role-first',
        { new_name: firstRole },
        originalVersion,
      );
      await mutate('PATCH', base + '/roles/' + firstRole, 'rotate-after-rename', {
        password: rolePassword(3),
      });
      const nextVersion = await version('role_second', 'roles', firstRole);
      await mutate(
        'POST',
        base + '/roles/' + firstRole + '/rename',
        'role-second',
        { new_name: role },
        nextVersion,
      );
      await mutate(
        'POST',
        base + '/roles/' + oldRole + '/rename',
        'role-first',
        { new_name: firstRole },
        originalVersion,
      );
      expect((await current('roles', role)).version_token).toBe('7');
      await native(writer, oldDatabase);
      await save('later_role_name_password_and_original_rename_receipt');
    }
    if (!done('later_database_name_and_original_rename_receipt')) {
      const originalVersion = await version('database_first', 'databases', oldDatabase);
      await mutate(
        'POST',
        base + '/databases/' + oldDatabase + '/rename',
        'database-first',
        { new_name: firstDatabase },
        originalVersion,
      );
      const nextVersion = await version('database_second', 'databases', firstDatabase);
      await mutate(
        'POST',
        base + '/databases/' + firstDatabase + '/rename',
        'database-second',
        { new_name: database },
        nextVersion,
      );
      await mutate(
        'POST',
        base + '/databases/' + oldDatabase + '/rename',
        'database-first',
        { new_name: firstDatabase },
        originalVersion,
      );
      expect((await current('databases', database)).version_token).toBe('8');
      await native();
      await save('later_database_name_and_original_rename_receipt');
    }
    if (!done('original_reader_writer_cold_latest_names_and_credentials')) {
      await zero(reader, 'reader-before');
      await zero(writer, 'writer-before-reader');
      await native(reader);
      await zero(reader, 'reader-after');
      await native(writer);
      await page.evaluate(
        ([key, value]) => sessionStorage.setItem(key, value),
        ['neon-control:catalog-branch:' + project, branch],
      );
      await page.goto('/#/projects/' + project + '/databases');
      await page.getByLabel('管理分支', { exact: true }).selectOption(branch);
      await expect(page.getByRole('row').filter({ hasText: database })).toContainText(role, {
        timeout: 190000,
      });
      await save('original_reader_writer_cold_latest_names_and_credentials');
    }
    if (!done('retained_customer_data_and_all_computes_zero')) {
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
      await save('retained_customer_data_and_all_computes_zero');
    }
    result = 'pass';
  } finally {
    await save();
  }
});
