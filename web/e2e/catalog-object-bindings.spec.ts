import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Current catalog identity generations reject same-name replacements', async ({
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
  const state: any = fixture.resume_catalog_object_bindings_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_object_bindings_checkpoint_file, 'utf8'))
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
    role = 'c02_bound_' + state.nonce,
    database = role + '_db',
    renamedRole = role + '_renamed',
    renamedDB = database + '_renamed',
    otherOwner = role + '_owner',
    driftRole = role + '_drift',
    driftDB = database + '_drift',
    reuseRole = role + '_reuse',
    reuseDB = database + '_reuse';
  const rolePassword = (generation: number) =>
    platformPassword + '-binding-' + generation + '-' + state.nonce;
  let result = 'running';
  const done = (value: string) => state.checks.includes(value);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-object-bindings-checkpoint.json'),
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
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return { operation: { id: state.operations[label] } };
    }
    if (version && !state.versions[label]) {
      state.versions[label] = version;
      await save();
    }
    version = state.versions[label] || version;
    const response = await context.request.fetch(target, {
      method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-bound-' + state.nonce + '-' + label,
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

  const finalRole = renamedRole,
    finalDB = renamedDB;
  const quote = (value: string) => '"' + value.replaceAll('"', '""') + '"';
  const literal = (value: string) => "'" + value.replaceAll("'", "''") + "'";
  async function oid(kind: string, name: string, endpoint = writer) {
    const table = kind === 'role' ? 'pg_roles' : 'pg_database',
      column = kind === 'role' ? 'rolname' : 'datname';
    return (
      await sql(
        'SELECT oid::text FROM ' + table + ' WHERE ' + column + '=' + literal(name),
        endpoint,
      )
    ).rows[0]?.[0];
  }
  async function reject(
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
        'Idempotency-Key': 'c02-bound-' + state.nonce + '-reject-' + label,
        ...(version ? { 'If-Match': '"' + version + '"' } : {}),
      },
      ...(data ? { data } : {}),
      timeout: 190000,
    });
    expect(response.status()).toBe(409);
    const value = await response.json();
    expect(JSON.stringify(value)).toContain('catalog_identity_conflict');
  }
  async function healthy(endpoint = writer) {
    expect(await oid('role', finalRole, endpoint)).toBe(state.native_oids.role);
    expect(await oid('database', finalDB, endpoint)).toBe(state.native_oids.database);
    expect(
      (await sql('SELECT marker FROM public.catalog_binding_probe WHERE id=1', endpoint, finalDB))
        .rows[0][0],
    ).toBe('retained-catalog-binding-data');
    expect(
      (await sql('SELECT current_user', endpoint, finalDB, finalRole, rolePassword(2))).rows[0][0],
    ).toBe(finalRole);
  }
  async function drift(endpoint = writer) {
    expect(await oid('role', driftRole, endpoint)).toBe(state.replacement_oids.role);
    expect(await oid('role', driftRole + '_retained', endpoint)).toBe(state.drift_oids.role);
    expect(await oid('database', driftDB, endpoint)).toBe(state.replacement_oids.database);
    expect(await oid('database', driftDB + '_retained', endpoint)).toBe(state.drift_oids.database);
    const row = (
      await sql(
        'SELECT md5(rolpassword),rolcanlogin,rolinherit,rolconnlimit::text FROM pg_authid WHERE rolname=' +
          literal(driftRole),
        endpoint,
      )
    ).rows[0];
    expect(row[0]).toBe(state.private_replacement_digest);
    expect(row.slice(1)).toEqual([true, false, '7']);
    const db = (
      await sql(
        'SELECT pg_get_userbyid(datdba),datconnlimit::text FROM pg_database WHERE datname=' +
          literal(driftDB),
        endpoint,
      )
    ).rows[0];
    expect(db).toEqual(['cloud_admin', '9']);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('new_role_and_database_original_oid_bindings')) {
      await mutate('POST', base + '/roles', 'create-role', {
        name: role,
        password: rolePassword(0),
      });
      await mutate('POST', base + '/roles', 'create-owner', {
        name: otherOwner,
        password: rolePassword(0),
      });
      await mutate('POST', base + '/databases', 'create-db', { name: database, owner: role });
      state.native_oids = {
        role: await oid('role', role),
        database: await oid('database', database),
      };
      await save();
      await sql(
        'CREATE TABLE IF NOT EXISTS public.catalog_binding_probe(id integer PRIMARY KEY,marker text)',
        writer,
        database,
      );
      await sql(
        "INSERT INTO public.catalog_binding_probe VALUES(1,'retained-catalog-binding-data') ON CONFLICT(id) DO NOTHING",
        writer,
        database,
      );
      await save('new_role_and_database_original_oid_bindings');
    }
    if (!done('rotate_rename_owner_generations_and_original_receipts')) {
      await mutate('PATCH', base + '/roles/' + role, 'rotate-before-rename', {
        password: rolePassword(1),
      });
      let value = await current('roles', role);
      await mutate(
        'POST',
        base + '/roles/' + role + '/rename',
        'rename-role',
        { new_name: renamedRole },
        value?.version_token,
      );
      await mutate('PATCH', base + '/roles/' + renamedRole, 'rotate-after-rename', {
        password: rolePassword(2),
      });
      value = await current('databases', database);
      await mutate(
        'POST',
        base + '/databases/' + database + '/rename',
        'rename-db',
        { new_name: renamedDB },
        value?.version_token,
      );
      value = await current('databases', renamedDB);
      await mutate(
        'PATCH',
        base + '/databases/' + renamedDB,
        'owner-away',
        { owner: otherOwner },
        value?.version_token,
      );
      value = await current('databases', renamedDB);
      await mutate(
        'PATCH',
        base + '/databases/' + renamedDB,
        'owner-back',
        { owner: renamedRole },
        value?.version_token,
      );
      // Old HTTP keys still return the original receipts after names/passwords moved.
      const replay = await context.request.post(base + '/roles', {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-bound-' + state.nonce + '-create-role',
        },
        data: { name: role, password: rolePassword(0) },
        timeout: 190000,
      });
      expect(replay.status()).toBe(202);
      expect((await replay.json()).operation.id).toBe(state.operations['create-role']);
      await healthy();
      await save('rotate_rename_owner_generations_and_original_receipts');
    }
    if (!done('same_name_role_database_and_owner_replacements_rejected')) {
      await mutate('POST', base + '/roles', 'create-drift-role', {
        name: driftRole,
        password: rolePassword(0),
      });
      await mutate('POST', base + '/databases', 'create-drift-db', {
        name: driftDB,
        owner: driftRole,
      });
      if (!state.drift_oids) {
        state.drift_oids = {
          role: await oid('role', driftRole),
          database: await oid('database', driftDB),
        };
        await save();
      }
      if ((await oid('role', driftRole)) === state.drift_oids.role) {
        await sql(
          'ALTER ROLE ' + quote(driftRole) + ' RENAME TO ' + quote(driftRole + '_retained'),
        );
        await sql(
          'CREATE ROLE ' +
            quote(driftRole) +
            ' LOGIN NOINHERIT CONNECTION LIMIT 7 PASSWORD ' +
            literal(rolePassword(3)),
        );
        await sql(
          'COMMENT ON ROLE ' + quote(driftRole) + ' IS ' + literal('neon-control/branch/' + branch),
        );
      }
      if ((await oid('database', driftDB)) === state.drift_oids.database) {
        await sql(
          'ALTER DATABASE ' + quote(driftDB) + ' RENAME TO ' + quote(driftDB + '_retained'),
        );
        await sql('CREATE DATABASE ' + quote(driftDB) + ' OWNER cloud_admin CONNECTION LIMIT 9');
        await sql(
          'COMMENT ON DATABASE ' +
            quote(driftDB) +
            ' IS ' +
            literal('neon-control/branch/' + branch),
        );
      }
      state.replacement_oids = {
        role: await oid('role', driftRole),
        database: await oid('database', driftDB),
      };
      state.private_replacement_digest = (
        await sql('SELECT md5(rolpassword) FROM pg_authid WHERE rolname=' + literal(driftRole))
      ).rows[0][0];
      await save();
      let value = await current('roles', driftRole);
      await reject('PATCH', base + '/roles/' + driftRole, 'rotate-replacement', {
        password: rolePassword(4),
      });
      await reject('DELETE', base + '/roles/' + driftRole, 'delete-replacement');
      await reject(
        'POST',
        base + '/roles/' + driftRole + '/rename',
        'rename-replacement',
        { new_name: driftRole + '_bad' },
        value?.version_token,
      );
      value = await current('databases', driftDB);
      await reject('DELETE', base + '/databases/' + driftDB, 'delete-db-replacement');
      await reject(
        'POST',
        base + '/databases/' + driftDB + '/rename',
        'rename-db-replacement',
        { new_name: driftDB + '_bad' },
        value?.version_token,
      );
      await reject(
        'PATCH',
        base + '/databases/' + driftDB,
        'owner-db-replacement',
        { owner: otherOwner },
        value?.version_token,
      );
      value = await current('databases', finalDB);
      await reject(
        'PATCH',
        base + '/databases/' + finalDB,
        'owner-role-replacement',
        { owner: driftRole },
        value?.version_token,
      );
      await reject('POST', base + '/databases', 'create-db-with-replacement-owner', {
        name: driftDB + '_bad',
        owner: driftRole,
      });
      await drift();
      await healthy();
      await save('same_name_role_database_and_owner_replacements_rejected');
    }
    if (!done('legitimate_delete_and_name_reuse_get_new_identity')) {
      await mutate('POST', base + '/roles', 'create-reuse-role', {
        name: reuseRole,
        password: rolePassword(0),
      });
      await mutate('POST', base + '/databases', 'create-reuse-db', {
        name: reuseDB,
        owner: reuseRole,
      });
      if (!state.reuse_original_oids)
        state.reuse_original_oids = {
          role: await oid('role', reuseRole),
          database: await oid('database', reuseDB),
        };
      await save();
      await mutate('DELETE', base + '/databases/' + reuseDB, 'delete-reuse-db');
      await mutate('DELETE', base + '/roles/' + reuseRole, 'delete-reuse-role');
      await mutate('POST', base + '/roles', 'recreate-reuse-role', {
        name: reuseRole,
        password: rolePassword(1),
      });
      await mutate('POST', base + '/databases', 'recreate-reuse-db', {
        name: reuseDB,
        owner: reuseRole,
      });
      state.reuse_current_oids = {
        role: await oid('role', reuseRole),
        database: await oid('database', reuseDB),
      };
      expect(state.reuse_current_oids.role).not.toBe(state.reuse_original_oids.role);
      expect(state.reuse_current_oids.database).not.toBe(state.reuse_original_oids.database);
      await save('legitimate_delete_and_name_reuse_get_new_identity');
    }
    if (!done('writer_reader_cold_preserve_native_generations_and_original_data')) {
      for (const endpoint of [writer, reader]) {
        await zero(endpoint, 'before-' + endpoint);
        await healthy(endpoint);
        await drift(endpoint);
        await zero(endpoint, 'after-' + endpoint);
      }
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
      await save('writer_reader_cold_preserve_native_generations_and_original_data');
    }
    result = 'pass';
  } finally {
    await save();
  }
});
