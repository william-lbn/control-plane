import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ actionTimeout: 30000 });
test('Catalog original identities: durable observations of actual external DDL', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !privateDir)
    throw Error('Protected retained C02 fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'));
  const project = fixture.project_id,
    branch = original.branch_id,
    writer = original.writer_id;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw Error('Exact retained native scope required');
  const password = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch;
  const state: any = fixture.resume_catalog_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        checks: [],
        operations: [],
        project_id: project,
        branch_id: branch,
        writer_id: writer,
      };
  if (state.project_id !== project || state.branch_id !== branch || state.writer_id !== writer)
    throw Error('Original test scope changed');
  const role = 'c02_identity_' + state.nonce,
    owner = role + '_owner',
    replacement = role + '_replace',
    database = role + '_db';
  let result = 'running';
  const done = (name: string) => state.checks.includes(name);
  const quote = (name: string) => '"' + name.replaceAll('"', '""') + '"';
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-identities-checkpoint.json'),
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
        role,
        owner,
        replacement,
        database,
        checks: state.checks,
        operations: state.operations,
        native_oids: state.oids,
        binding_ids: state.bindings,
        customer_data_preserved: true,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function headers() {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(url: string) {
    const r = await context.request.get(url);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function create(url: string, label: string, body: unknown) {
    const r = await context.request.post(url, {
      headers: { ...(await headers()), 'Idempotency-Key': 'c02-id-' + state.nonce + '-' + label },
      data: body,
    });
    expect(r.status()).toBe(202);
    const v = await r.json();
    if (!state.operations.includes(v.operation.id)) {
      state.operations.push(v.operation.id);
      await save();
    }
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + v.operation.id);
          if (op.state === 'failed') throw Error('Owned original Operation failed ' + op.id);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
    return v;
  }
  async function sql(statement: string, target = original.platform_database || 'postgres') {
    const r = await context.request.post(root + '/endpoints/' + writer + '/query', {
      headers: await headers(),
      data: { role: original.platform_role, database: target, password, sql: statement },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function inventory() {
    return get(base + '/catalog-identities');
  }
  function identity(v: any, kind: string, name: string) {
    const row = v.items.find((x: any) => x.kind === kind && x.expected_name === name);
    expect(row).toBeTruthy();
    expect(row.credentials).toBe('not_observed');
    return row;
  }
  async function catalog() {
    await page.evaluate(
      ([key, value]) => sessionStorage.setItem(key, value),
      ['neon-control:catalog-branch:' + project, branch],
    );
    await page.goto(`/#/projects/${project}/databases`);
    await page.getByLabel('管理分支', { exact: true }).selectOption(branch);
    await expect(page.getByRole('button', { name: '刷新目录', exact: true })).toBeEnabled({
      timeout: 190000,
    });
  }
  const panel = page.getByRole('region', { name: '外部 DDL 身份观察', exact: true });
  async function readPanel() {
    await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled();
    await panel.getByRole('button', { name: '读取身份变化', exact: true }).click();
    await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled({
      timeout: 190000,
    });
    await expect(panel.getByRole('alert')).toHaveCount(0);
  }
  async function initialize(kind: string, name: string, lost = false) {
    const v = await inventory();
    const object = v.native_objects.find((x: any) => x.kind === kind && x.name === name);
    expect(object?.can_initialize).toBe(true);
    await panel.getByLabel('首次记录原身份', { exact: true }).selectOption(kind + ':' + object.oid);
    await panel
      .getByLabel('我确认仅从当前时点记录原身份；此前是否发生过重建无法由名称或标记证明。', {
        exact: true,
      })
      .check();
    let key = '',
      body = '',
      binding = '';
    if (lost)
      await page.route(
        '**' + base + '/catalog-identities',
        async (route) => {
          if (route.request().method() !== 'POST') return route.continue();
          key = route.request().headers()['idempotency-key'];
          body = route.request().postData()!;
          state.original_request = { key, body, version: route.request().headers()['if-match'] };
          await save();
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          binding = (await response.json()).binding_id;
          state.original_receipt = binding;
          await save();
          await route.abort('failed');
        },
        { times: 1 },
      );
    await panel.getByRole('button', { name: '确认并记录原身份', exact: true }).click();
    if (lost) {
      const retry = panel.getByRole('button', { name: '按原请求重试记录', exact: true });
      await expect(retry).toBeEnabled();
      const reply = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/catalog-identities' &&
          r.request().method() === 'POST',
      );
      await retry.click();
      const r = await reply;
      expect(r.status()).toBe(200);
      expect(r.request().headers()['idempotency-key']).toBe(key);
      expect(r.request().postData()).toBe(body);
      expect((await r.json()).binding_id).toBe(binding);
    }
    await expect(panel.getByRole('status')).toContainText('原身份已记录');
    await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled({
      timeout: 190000,
    });
    const row = identity(await inventory(), kind, name);
    expect(row.status).toBe('catalog_matches');
    state.bindings ||= {};
    state.bindings[kind + ':' + name] = row.binding_id;
    await save();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('owned_native_objects_ready')) {
      for (const name of [role, owner, replacement])
        await create(base + '/roles', 'role-' + name, { name, password });
      await create(base + '/databases', 'database', { name: database, owner: role });
      await sql(
        'CREATE TABLE IF NOT EXISTS public.identity_probe(id integer PRIMARY KEY, marker text NOT NULL)',
        database,
      );
      await sql(
        "INSERT INTO public.identity_probe VALUES(1,'retained-identity-data') ON CONFLICT(id) DO NOTHING",
        database,
      );
      const v = await inventory();
      state.oids = Object.fromEntries(
        v.native_objects
          .filter((x: any) => [role, owner, replacement, database].includes(x.name))
          .map((x: any) => [x.name, x.oid]),
      );
      expect(Object.keys(state.oids)).toHaveLength(4);
      await save('owned_native_objects_ready');
    }
    await catalog();
    await readPanel();
    if (!done('original_identity_receipt_replayed_in_console')) {
      // An interrupted test may already have an accepted original record.
      const old = (await inventory()).items.find(
        (x: any) => x.kind === 'role' && x.expected_name === role,
      );
      if (!old) await initialize('role', role, true);
      else {
        expect(old.expected_oid).toBe(state.oids[role]);
        if (!state.original_request)
          throw Error('Original accepted UI request must be recovered before claiming replay');
        const r = await context.request.post(base + '/catalog-identities', {
          headers: {
            ...(await headers()),
            'Idempotency-Key': state.original_request.key,
            'If-Match': state.original_request.version,
          },
          data: JSON.parse(state.original_request.body),
        });
        expect(r.status()).toBe(200);
        expect((await r.json()).binding_id).toBe(old.binding_id);
        state.bindings ||= {};
        state.bindings['role:' + role] = old.binding_id;
      }
      await save('original_identity_receipt_replayed_in_console');
    }
    if (!done('database_and_replacement_baselines_retained')) {
      for (const [kind, name] of [
        ['database', database],
        ['role', replacement],
      ]) {
        const old = (await inventory()).items.find(
          (x: any) => x.kind === kind && x.expected_name === name,
        );
        if (!old) await initialize(kind, name);
        else {
          state.bindings ||= {};
          state.bindings[kind + ':' + name] = old.binding_id;
        }
      }
      await save('database_and_replacement_baselines_retained');
    }
    if (!done('same_oid_rename_and_owner_role_rename_detected')) {
      await sql('ALTER ROLE ' + quote(role) + ' RENAME TO ' + quote(role + '_outside'));
      let v = await inventory();
      expect(identity(v, 'role', role).status).toBe('renamed');
      expect(identity(v, 'role', role).observed_oid).toBe(state.oids[role]);
      expect(identity(v, 'database', database).status).toBe('owner_role_renamed');
      await readPanel();
      await expect(panel.getByRole('table', { name: '身份变化记录' })).toContainText(
        '原对象已改名',
      );
      await sql('ALTER ROLE ' + quote(role + '_outside') + ' RENAME TO ' + quote(role));
      v = await inventory();
      expect(identity(v, 'role', role).status).toBe('catalog_matches');
      await save('same_oid_rename_and_owner_role_rename_detected');
    }
    if (!done('nologin_and_nonconnectable_database_observed')) {
      await sql('ALTER ROLE ' + quote(role) + ' NOLOGIN');
      await sql('ALTER DATABASE ' + quote(database) + ' ALLOW_CONNECTIONS false');
      const v = await inventory();
      expect(identity(v, 'role', role).status).toBe('role_options_changed');
      expect(identity(v, 'database', database).status).toBe('database_options_changed');
      expect(v.native_objects.some((x: any) => x.name === role && x.oid === state.oids[role])).toBe(
        true,
      );
      expect(
        v.native_objects.some((x: any) => x.name === database && x.oid === state.oids[database]),
      ).toBe(true);
      await sql('ALTER ROLE ' + quote(role) + ' LOGIN');
      await sql('ALTER DATABASE ' + quote(database) + ' ALLOW_CONNECTIONS true');
      await save('nologin_and_nonconnectable_database_observed');
    }
    if (!done('external_owner_change_preserved_native_data')) {
      await sql('ALTER DATABASE ' + quote(database) + ' OWNER TO ' + quote(owner));
      expect(identity(await inventory(), 'database', database).status).toBe('owner_changed');
      expect(
        (await sql('SELECT marker FROM public.identity_probe WHERE id=1', database)).rows[0][0],
      ).toBe('retained-identity-data');
      await sql('ALTER DATABASE ' + quote(database) + ' OWNER TO ' + quote(role));
      await save('external_owner_change_preserved_native_data');
    }
    if (!done('same_name_collision_does_not_replace_original_identity')) {
      await sql('ALTER ROLE ' + quote(role) + ' RENAME TO ' + quote(role + '_outside'));
      await sql('CREATE ROLE ' + quote(role) + ' NOLOGIN');
      const v = await inventory();
      expect(identity(v, 'role', role).status).toBe('name_collision');
      expect(identity(v, 'role', role).expected_oid).toBe(state.oids[role]);
      const collisionOID = v.native_objects.find(
        (x: any) => x.kind === 'role' && x.name === role,
      ).oid;
      state.collision_oid = collisionOID;
      await save();
      // Only this test's new, dependency-free collision role may be removed.
      const actual = (await sql('SELECT oid::text FROM pg_roles WHERE rolname=' + "'" + role + "'"))
        .rows[0][0];
      expect(actual).toBe(String(collisionOID));
      expect(collisionOID).not.toBe(state.oids[role]);
      await sql('DROP ROLE ' + quote(role));
      await sql('ALTER ROLE ' + quote(role + '_outside') + ' RENAME TO ' + quote(role));
      await save('same_name_collision_does_not_replace_original_identity');
    }
    if (!done('drop_recreate_with_copied_marker_detected_by_oid')) {
      const before = (
        await sql('SELECT oid::text FROM pg_roles WHERE rolname=' + "'" + replacement + "'")
      ).rows[0][0];
      expect(before).toBe(String(state.oids[replacement]));
      await sql('DROP ROLE ' + quote(replacement));
      await sql('CREATE ROLE ' + quote(replacement) + ' NOLOGIN');
      await sql(
        'COMMENT ON ROLE ' + quote(replacement) + " IS 'neon-control/branch/" + branch + "'",
      );
      const row = identity(await inventory(), 'role', replacement);
      expect(row.status).toBe('replaced');
      expect(row.observed_oid).not.toBe(row.expected_oid);
      state.replacement_oid = row.observed_oid;
      const r = await context.request.post(base + '/catalog-identities', {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-no-rebaseline-' + state.nonce,
          'If-Match': '"' + row.intent_version + '"',
        },
        data: {
          kind: 'role',
          name: replacement,
          oid: row.observed_oid,
          acknowledge_initial_observation: true,
        },
      });
      expect(r.status()).toBe(409);
      await save('drop_recreate_with_copied_marker_detected_by_oid');
    }
    if (!done('page_reload_retains_original_baselines_and_customer_data')) {
      await page.reload();
      await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled({
        timeout: 190000,
      });
      await readPanel();
      const v = await inventory();
      for (const row of v.items)
        if (state.bindings[row.kind + ':' + row.expected_name])
          expect(row.binding_id).toBe(state.bindings[row.kind + ':' + row.expected_name]);
      expect(identity(v, 'role', role).status).toBe('catalog_matches');
      expect(identity(v, 'database', database).status).toBe('catalog_matches');
      expect(identity(v, 'role', replacement).status).toBe('replaced');
      expect(
        (await sql('SELECT marker FROM public.identity_probe WHERE id=1', database)).rows[0][0],
      ).toBe('retained-identity-data');
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            'c02_default_' + original.nonce,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      await save('page_reload_retains_original_baselines_and_customer_data');
    }
    await create(root + '/endpoints/' + writer + '/suspend', 'final-zero', {});
    await expect
      .poll(async () => (await get(root + '/endpoints/' + writer)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
    await save('original_writer_returned_to_zero');
    result = 'pass';
  } finally {
    await save();
  }
});
