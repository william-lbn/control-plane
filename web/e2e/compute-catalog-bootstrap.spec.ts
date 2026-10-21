import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Bootstrapped Compute preserves native catalog changes through cold starts', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!file || !privateDir || !adminFile) throw Error('Protected original fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8')),
    original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'));
  const project = fixture.project_id,
    branch = original.branch_id,
    writer = original.writer_id,
    reader = original.reader_id;
  const platformPassword = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const state: any = fixture.resume_compute_catalog_bootstrap_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_compute_catalog_bootstrap_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        reader_id: reader,
        checks: [],
        operations: {},
      };
  if (
    state.project_id !== project ||
    state.branch_id !== branch ||
    state.writer_id !== writer ||
    state.reader_id !== reader
  )
    throw Error('Original Compute fixture changed');
  const root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch,
    role = 'c02_cold_' + state.nonce,
    renamed = role + '_sql_old',
    database = role + '_db';
  const managedPassword = platformPassword + '-cold-managed-' + state.nonce,
    sqlPassword = platformPassword + '-cold-sql-' + state.nonce;
  let result = 'running';
  const done = (v: string) => state.checks.includes(v);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'compute-catalog-bootstrap-checkpoint.json'),
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
        renamed_role: renamed,
        database,
        checks: state.checks,
        operations: state.operations,
        native_oids: state.native_oids,
        replacement_oid: state.replacement_oid,
        new_branch_id: state.new_branch_id,
        new_writer_id: state.new_writer_id,
        new_reader_id: state.new_reader_id,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function headers() {
    const csrf = (await context.cookies()).find((v) => v.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('CSRF required');
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
          const v = await get(root + '/operations/' + id);
          if (v.state === 'failed') throw Error('Original Compute Operation failed ' + id);
          return v.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function create(target: string, label: string, data: unknown) {
    const r = await context.request.post(target, {
      headers: { ...(await headers()), 'Idempotency-Key': 'c02-cold-' + state.nonce + '-' + label },
      data,
      timeout: 190000,
    });
    expect(r.status()).toBe(202);
    const v = await r.json();
    if (state.operations[label]) expect(v.operation.id).toBe(state.operations[label]);
    else {
      state.operations[label] = v.operation.id;
      await save();
    }
    await observe(v.operation.id);
    return v;
  }
  async function sql(
    statement: string,
    endpoint = writer,
    target = original.platform_database,
    password = platformPassword,
  ) {
    const r = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { role: original.platform_role, database: target, password, sql: statement },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function zero(endpoint: string, label: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state !== 'suspended')
      await create(root + '/endpoints/' + endpoint + '/suspend', 'zero-' + label, {});
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  async function verify(endpoint: string) {
    const v = (
      await sql(
        "SELECT oid::text,rolcanlogin,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolreplication,rolbypassrls,rolconnlimit,md5(rolpassword),COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_authid WHERE rolname='" +
          role +
          "'",
        endpoint,
      )
    ).rows[0];
    expect(v.slice(0, 9)).toEqual([
      state.replacement_oid,
      true,
      false,
      false,
      false,
      false,
      false,
      false,
      7,
    ]);
    expect(v[9] === state.native_password_digest).toBe(true);
    expect(v[10]).toBe('neon-control/branch/' + branch);
    expect(
      (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + renamed + "'", endpoint))
        .rows[0][0],
    ).toBe(state.native_oids.role);
    expect(
      (
        await sql(
          "SELECT oid::text,pg_get_userbyid(datdba),datconnlimit FROM pg_database WHERE datname='" +
            database +
            "'",
          endpoint,
        )
      ).rows[0],
    ).toEqual([state.native_oids.database, original.platform_role, 17]);
    expect(
      (await sql('SELECT marker FROM public.cold_catalog_probe WHERE id=1', endpoint, database))
        .rows[0][0],
    ).toBe('retained-cold-catalog-data');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('fresh_writer_and_reader_bootstrap')) {
      if (!state.new_branch_id) {
        const v = await create(root + '/branches', 'fresh-branch', {
          name: 'ci-cold-bootstrap-' + state.nonce,
          parent_branch_id: fixture.parent_branch_id,
          password: platformPassword,
          function_copy_policy: 'omit',
          autoscaling: {
            min_cpu_milli: 1000,
            max_cpu_milli: 1000,
            min_memory_mib: 1024,
            max_memory_mib: 1024,
          },
        });
        state.new_branch_id = v.resource.id;
        await save();
      }
      state.new_writer_id = (await get(root + '/endpoints')).items.find(
        (v: any) => v.branch_id === state.new_branch_id && v.endpoint_type === 'read_write',
      )?.id;
      if (!state.new_writer_id) throw Error('Original new Writer missing');
      await save();
      expect((await sql('SELECT current_user', state.new_writer_id)).rows[0][0]).toBe(
        original.platform_role,
      );
      if (!state.new_reader_id) {
        const v = await create(root + '/endpoints', 'fresh-reader', {
          branch_id: state.new_branch_id,
          type: 'read_only',
          autoscaling: {
            min_cpu_milli: 1000,
            max_cpu_milli: 1000,
            min_memory_mib: 1024,
            max_memory_mib: 1024,
          },
        });
        state.new_reader_id = v.resource.id;
        await save();
      }
      expect((await sql('SELECT pg_is_in_recovery()', state.new_reader_id)).rows[0][0]).toBe(true);
      await zero(state.new_reader_id, 'fresh-reader');
      await zero(state.new_writer_id, 'fresh-writer');
      expect((await sql('SELECT current_user', state.new_writer_id)).rows[0][0]).toBe(
        original.platform_role,
      );
      await zero(state.new_writer_id, 'fresh-writer-cold');
      await save('fresh_writer_and_reader_bootstrap');
    }
    if (!done('owned_original_objects_created')) {
      await create(base + '/roles', 'create-role', { name: role, password: managedPassword });
      await create(base + '/databases', 'create-database', { name: database, owner: role });
      await sql(
        'CREATE TABLE IF NOT EXISTS public.cold_catalog_probe(id integer PRIMARY KEY,marker text NOT NULL)',
        writer,
        database,
      );
      await sql(
        "INSERT INTO public.cold_catalog_probe VALUES(1,'retained-cold-catalog-data') ON CONFLICT(id) DO NOTHING",
        writer,
        database,
      );
      state.native_oids = {
        role: (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + role + "'")).rows[0][0],
        database: (await sql("SELECT oid::text FROM pg_database WHERE datname='" + database + "'"))
          .rows[0][0],
      };
      await save('owned_original_objects_created');
    }
    if (!done('explicit_sql_replacement_and_owner_options_observed')) {
      // Only this test's new objects are changed. Its original role and all data
      // remain retained; the pre-existing external replacement is never altered.
      const old = (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + renamed + "'"))
        .rows;
      if (!old.length) await sql('ALTER ROLE "' + role + '" RENAME TO "' + renamed + '"');
      else expect(old[0][0]).toBe(state.native_oids.role);
      const replacement = (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + role + "'"))
        .rows;
      if (!replacement.length)
        await sql(
          'CREATE ROLE "' +
            role +
            '" LOGIN NOINHERIT NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 7 PASSWORD ' +
            "'" +
            sqlPassword.replaceAll("'", "''") +
            "'",
        );
      await sql('COMMENT ON ROLE "' + role + '" IS ' + "'neon-control/branch/" + branch + "'");
      await sql('ALTER DATABASE "' + database + '" OWNER TO "' + original.platform_role + '"');
      await sql('ALTER DATABASE "' + database + '" CONNECTION LIMIT 17');
      const row = (
        await sql("SELECT oid::text,md5(rolpassword) FROM pg_authid WHERE rolname='" + role + "'")
      ).rows[0];
      state.replacement_oid = row[0];
      state.native_password_digest = row[1];
      expect(state.replacement_oid).not.toBe(state.native_oids.role);
      await verify(writer);
      await create(base + '/roles', 'create-after-sql', {
        name: role + '_later',
        password: managedPassword,
      });
      await verify(writer);
      await save('explicit_sql_replacement_and_owner_options_observed');
    }
    if (!done('original_reader_and_writer_cold_preserve_sql_catalog')) {
      await zero(reader, 'reader-before');
      await zero(writer, 'writer-before');
      await verify(reader);
      await zero(reader, 'reader-after');
      await verify(writer);
      await page.evaluate(
        ([key, value]) => sessionStorage.setItem(key, value),
        ['neon-control:catalog-branch:' + project, branch],
      );
      await page.goto('/#/projects/' + project + '/databases');
      await page.getByLabel('管理分支', { exact: true }).selectOption(branch);
      await expect(page.getByRole('row').filter({ hasText: database })).toBeVisible();
      await save('original_reader_and_writer_cold_preserve_sql_catalog');
    }
    if (!done('original_customer_data_and_zero_computes')) {
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
      await zero(state.new_writer_id, 'final-new-writer');
      await zero(state.new_reader_id, 'final-new-reader');
      await save('original_customer_data_and_zero_computes');
    }
    result = 'pass';
  } finally {
    await save();
  }
});
