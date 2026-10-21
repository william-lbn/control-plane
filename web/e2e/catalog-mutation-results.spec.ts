import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Original catalog receipts retain later passwords, Owner and cold data', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile)
    throw Error('Protected retained C02 fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')),
    original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'));
  const project = fixture.project_id,
    branch = original.branch_id,
    writer = original.writer_id,
    reader = original.reader_id;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer) ||
    !/^ep_[a-f0-9]{16}$/.test(reader)
  )
    throw Error('Exact retained native scope required');
  const platformPassword = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const state: any = fixture.resume_catalog_mutation_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_mutation_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
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
    state.reader_id !== reader
  )
    throw Error('Original catalog result scope changed');
  const root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch,
    role = 'c02_receipt_' + state.nonce,
    database = role + '_db',
    otherOwner =
      'c02_identity_' +
      JSON.parse(await readFile(fixture.resume_catalog_checkpoint_file, 'utf8')).nonce;
  const rolePassword = (generation: number) =>
    platformPassword + '-receipt-' + generation + '-' + state.nonce;
  let result = 'running';
  const done = (value: string) => state.checks.includes(value);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-mutation-results-checkpoint.json'),
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
        database,
        checks: state.checks,
        operations: state.operations,
        native_oids: state.native_oids,
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
    const response = await context.request.get(target, { timeout: 190000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (op.state === 'failed') throw Error('Original catalog Operation failed ' + id);
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
    data?: unknown,
    version?: string,
  ) {
    const response = await context.request.fetch(target, {
      method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-receipt-' + state.nonce + '-' + label,
        ...(version ? { 'If-Match': '"' + version + '"' } : {}),
      },
      ...(data ? { data } : {}),
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    if (state.operations[label]) expect(value.operation.id).toBe(state.operations[label]);
    else {
      state.operations[label] = value.operation.id;
      await save();
    }
    await observe(value.operation.id);
    return value;
  }
  async function sql(
    statement: string,
    endpoint = writer,
    db = original.platform_database,
    loginRole = original.platform_role,
    password = platformPassword,
  ) {
    const response = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { role: loginRole, database: db, password, sql: statement },
      timeout: 190000,
    });
    expect(response.status()).toBe(200);
    return response.json();
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
  async function current(target: string, name: string) {
    return (await get(base + '/' + target)).items.find((v: any) => v.name === name);
  }
  async function native(endpoint = writer) {
    const row = (
      await sql(
        "SELECT oid::text,datdba::text,pg_get_userbyid(datdba) FROM pg_database WHERE datname='" +
          database +
          "'",
        endpoint,
      )
    ).rows[0];
    expect(row[0]).toBe(state.native_oids.database);
    expect(row[2]).toBe(role);
    expect(
      (await sql('SELECT marker FROM public.catalog_receipt_probe WHERE id=1', endpoint, database))
        .rows[0][0],
    ).toBe('retained-catalog-receipt-data');
    expect(
      (await sql('SELECT current_user', endpoint, database, role, rolePassword(2))).rows[0][0],
    ).toBe(role);
    expect(
      (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + role + "'", endpoint))
        .rows[0][0],
    ).toBe(state.native_oids.role);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('original_role_database_and_retained_data')) {
      await mutate('POST', base + '/roles', 'create-role', {
        name: role,
        password: rolePassword(0),
      });
      await mutate('POST', base + '/databases', 'create-database', { name: database, owner: role });
      await sql(
        'CREATE TABLE IF NOT EXISTS public.catalog_receipt_probe(id integer PRIMARY KEY,marker text)',
        writer,
        database,
      );
      await sql(
        "INSERT INTO public.catalog_receipt_probe VALUES(1,'retained-catalog-receipt-data') ON CONFLICT(id) DO NOTHING",
        writer,
        database,
      );
      state.native_oids = {
        role: (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + role + "'")).rows[0][0],
        database: (await sql("SELECT oid::text FROM pg_database WHERE datname='" + database + "'"))
          .rows[0][0],
      };
      await save('original_role_database_and_retained_data');
    }
    if (!done('later_password_and_original_http_receipt')) {
      await mutate('PATCH', base + '/roles/' + role, 'rotate-first', { password: rolePassword(1) });
      await mutate('PATCH', base + '/roles/' + role, 'rotate-second', {
        password: rolePassword(2),
      });
      state.versions.role = (await current('roles', role)).version_token;
      expect(state.versions.role).toBe('4');
      await mutate('PATCH', base + '/roles/' + role, 'rotate-first', { password: rolePassword(1) });
      expect((await current('roles', role)).version_token).toBe(state.versions.role);
      expect(
        (await sql('SELECT current_user', writer, database, role, rolePassword(2))).rows[0][0],
      ).toBe(role);
      await save('later_password_and_original_http_receipt');
    }
    if (!done('later_database_owner_and_original_http_receipt')) {
      if (!state.versions.owner_first)
        state.versions.owner_first = (await current('databases', database)).version_token;
      await save();
      await mutate(
        'PATCH',
        base + '/databases/' + database,
        'owner-first',
        { owner: otherOwner },
        state.versions.owner_first,
      );
      if (!state.versions.owner_second)
        state.versions.owner_second = (await current('databases', database)).version_token;
      await save();
      await mutate(
        'PATCH',
        base + '/databases/' + database,
        'owner-second',
        { owner: role },
        state.versions.owner_second,
      );
      state.versions.database = (await current('databases', database)).version_token;
      expect(state.versions.database).toBe('4');
      await mutate(
        'PATCH',
        base + '/databases/' + database,
        'owner-first',
        { owner: otherOwner },
        state.versions.owner_first,
      );
      expect((await current('databases', database)).version_token).toBe(state.versions.database);
      await native();
      await save('later_database_owner_and_original_http_receipt');
    }
    if (!done('latest_password_owner_and_data_on_original_cold_replicas')) {
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
      await save('latest_password_owner_and_data_on_original_cold_replicas');
    }
    if (!done('original_customer_data_and_all_computes_zero')) {
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
      await save('original_customer_data_and_all_computes_zero');
    }
    result = 'pass';
  } finally {
    await save();
  }
});
