import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ actionTimeout: 30000 });
test('Original catalog identity restoration: explicit credentials, native OIDs and retained cold replicas', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!file || !adminFile || !privateDir)
    throw Error('Protected original C02 inventory/password fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8'));
  const original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'));
  const catalog = JSON.parse(await readFile(fixture.resume_catalog_checkpoint_file, 'utf8'));
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
    throw Error('Retained scope differs from accepted inventory');
  const root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch;
  const password = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const role = 'c02_identity_' + catalog.nonce,
    owner = role + '_owner',
    database = role + '_db',
    replacement = role + '_replace';
  const roleBinding = catalog.bindings['role:' + role],
    dbBinding = catalog.bindings['database:' + database],
    replacementBinding = catalog.bindings['role:' + replacement];
  const state: any = fixture.resume_restore_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_restore_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        checks: [],
        operations: [],
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
    throw Error('Original restoration scope changed');
  let result = 'running';
  const done = (name: string) => state.checks.includes(name);
  const quote = (name: string) => '"' + name.replaceAll('"', '""') + '"';
  const literal = (value: string) => "'" + value.replaceAll("'", "''") + "'";
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-identity-restore-checkpoint.json'),
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
        replacement,
        original_role_oid: catalog.oids[role],
        original_database_oid: catalog.oids[database],
        binding_ids: { role: roleBinding, database: dbBinding, replacement: replacementBinding },
        checks: state.checks,
        operations: state.operations,
        restore_operations: state.restore_operations || [],
        native_identity: state.native_identity,
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
  async function inventory() {
    return get(base + '/catalog-identities');
  }
  function row(v: any, binding: string) {
    const value = v.items.find((x: any) => x.binding_id === binding);
    expect(value).toBeTruthy();
    expect(value.credentials).toBe('not_observed');
    return value;
  }
  async function sql(
    statement: string,
    target = original.platform_database,
    endpoint = writer,
    loginRole = original.platform_role,
  ) {
    const r = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { role: loginRole, database: target, password, sql: statement },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function observe(id: string, restore = false) {
    if (!state.operations.includes(id)) state.operations.push(id);
    if (restore) {
      state.restore_operations ||= [];
      if (!state.restore_operations.includes(id)) state.restore_operations.push(id);
    }
    await save();
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (op.state === 'failed') throw Error('Original restore Operation failed ' + id);
          return op.state;
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
          'Idempotency-Key': 'c02-restore-' + state.nonce + '-zero-' + label,
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
  async function restoreRequest(binding: string, label: string, expected = 202) {
    const v = row(await inventory(), binding);
    const r = await context.request.post(base + '/catalog-identities/' + binding + '/restore', {
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-restore-' + state.nonce + '-' + label,
        'If-Match': '"' + v.intent_version + '"',
      },
      data: { acknowledge_restore: true, acknowledge_role_credential_reset: v.kind === 'role' },
    });
    expect(r.status()).toBe(expected);
    if (expected === 202) {
      const op = (await r.json()).operation.id;
      await observe(op, true);
      return op;
    }
  }
  async function catalogPage() {
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
  const form = page.getByRole('form', { name: '恢复原目录身份', exact: true });
  async function readPanel() {
    await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled();
    await panel.getByRole('button', { name: '读取身份变化', exact: true }).click();
    await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled({
      timeout: 190000,
    });
  }
  async function uiRestore(binding: string, lost: boolean) {
    await readPanel();
    await form.getByLabel('待恢复的原对象', { exact: true }).selectOption(binding);
    await form.getByLabel('我确认把原 OID 恢复到记录的原名称。', { exact: true }).check();
    if (binding === roleBinding) {
      await expect(form.getByRole('button', { name: '恢复原名称', exact: true })).toBeDisabled();
      await form
        .getByLabel('我确认将角色密码重设为平台原有受管理凭据；当前外部密码未观察。', {
          exact: true,
        })
        .check();
    } else
      await expect(
        form.getByLabel('我确认将角色密码重设为平台原有受管理凭据；当前外部密码未观察。', {
          exact: true,
        }),
      ).toHaveCount(0);
    const url = base + '/catalog-identities/' + binding + '/restore';
    if (lost)
      await page.route(
        '**' + url,
        async (route) => {
          state.original_restore_request = {
            key: route.request().headers()['idempotency-key'],
            version: route.request().headers()['if-match'],
            body: route.request().postData(),
            url,
          };
          await save();
          const response = await route.fetch();
          expect(response.status()).toBe(202);
          state.role_operation = (await response.json()).operation.id;
          await save();
          await route.abort('failed');
        },
        { times: 1 },
      );
    if (lost) {
      await form.getByRole('button', { name: '恢复原名称', exact: true }).click();
      const retry = form.getByRole('button', { name: '重试原身份恢复提交', exact: true });
      await expect(retry).toBeEnabled();
      const reply = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'POST',
      );
      await retry.click();
      const response = await reply;
      expect(response.status()).toBe(202);
      expect(response.request().headers()['idempotency-key']).toBe(
        state.original_restore_request.key,
      );
      expect(response.request().postData()).toBe(state.original_restore_request.body);
      expect((await response.json()).operation.id).toBe(state.role_operation);
      await observe(state.role_operation, true);
    } else {
      const reply = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'POST',
      );
      await form.getByRole('button', { name: '恢复原名称', exact: true }).click();
      const response = await reply;
      expect(response.status()).toBe(202);
      state.database_operation = (await response.json()).operation.id;
      await save();
      await observe(state.database_operation, true);
    }
    await expect(form.getByRole('status')).toContainText('succeeded', { timeout: 190000 });
    await expect(panel.getByRole('button', { name: '读取身份变化', exact: true })).toBeEnabled();
  }
  const identitySQL = `SELECT current_user,current_database(),d.oid::text,d.datdba::text,COALESCE(d.datacl::text,''),c.oid::text,c.relowner::text,(SELECT marker FROM public.identity_probe WHERE id=1) FROM pg_database d JOIN pg_class c ON c.relname='identity_probe' JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname='public' WHERE d.datname=current_database()`;
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('original_inventory_scope_and_replacement_rejected')) {
      const v = await inventory();
      expect(row(v, roleBinding).expected_oid).toBe(catalog.oids[role]);
      expect(row(v, dbBinding).expected_oid).toBe(catalog.oids[database]);
      expect(row(v, replacementBinding).status).toBe('replaced');
      await restoreRequest(replacementBinding, 'reject-replacement', 409);
      state.native_identity = (await sql(identitySQL, database, writer, role)).rows;
      expect(state.native_identity[0][2]).toBe(String(catalog.oids[database]));
      await save('original_inventory_scope_and_replacement_rejected');
    }
    if (!done('explicit_credential_ack_required')) {
      const r = await context.request.post(
        base + '/catalog-identities/' + roleBinding + '/restore',
        {
          headers: {
            ...(await headers()),
            'Idempotency-Key': 'c02-restore-no-credential-ack-' + state.nonce,
            'If-Match': '"2"',
          },
          data: { acknowledge_restore: true },
        },
      );
      expect(r.status()).toBe(422);
      await save('explicit_credential_ack_required');
    }
    await catalogPage();
    if (!done('same_role_original_request_recovers_known_credential')) {
      if (state.role_operation) await observe(state.role_operation, true);
      else {
        const actual = (await sql('SELECT rolname FROM pg_roles WHERE oid=' + catalog.oids[role]))
          .rows[0][0];
        if (actual === role)
          await sql('ALTER ROLE ' + quote(role) + ' RENAME TO ' + quote(role + '_restore_outside'));
        else expect(actual).toBe(role + '_restore_outside');
        await sql(
          'ALTER ROLE ' +
            quote(role + '_restore_outside') +
            ' PASSWORD ' +
            literal(password + '-outside-restoration'),
        );
        await uiRestore(roleBinding, true);
      }
      expect((await sql(identitySQL, database, writer, role)).rows).toEqual(state.native_identity);
      expect(row(await inventory(), roleBinding).status).toBe('catalog_matches');
      await save('same_role_original_request_recovers_known_credential');
    }
    if (!done('database_restoration_preserves_data_acl_and_owners')) {
      if (state.database_operation) await observe(state.database_operation, true);
      else {
        await sql(
          'ALTER DATABASE ' +
            quote(database) +
            ' RENAME TO ' +
            quote(database + '_restore_outside'),
        );
        await sql(
          'ALTER DATABASE ' + quote(database + '_restore_outside') + ' OWNER TO ' + quote(owner),
        );
        await restoreRequest(dbBinding, 'reject-owner-conflict', 409);
        await sql(
          'ALTER DATABASE ' + quote(database + '_restore_outside') + ' OWNER TO ' + quote(role),
        );
        await uiRestore(dbBinding, false);
      }
      expect((await sql(identitySQL, database, writer, role)).rows).toEqual(state.native_identity);
      await save('database_restoration_preserves_data_acl_and_owners');
    }
    if (!done('collision_and_provenance_conflicts_refuse_adoption')) {
      await sql('ALTER ROLE ' + quote(role) + ' RENAME TO ' + quote(role + '_restore_outside'));
      await sql('CREATE ROLE ' + quote(role) + ' NOLOGIN');
      const collisionOID = (
        await sql('SELECT oid::text FROM pg_roles WHERE rolname=' + literal(role))
      ).rows[0][0];
      expect(collisionOID).not.toBe(String(catalog.oids[role]));
      state.collision_oid = collisionOID;
      await save();
      await restoreRequest(roleBinding, 'reject-collision', 409);
      expect(
        (await sql('SELECT oid::text FROM pg_roles WHERE rolname=' + literal(role))).rows[0][0],
      ).toBe(collisionOID);
      await sql('DROP ROLE ' + quote(role));
      await sql(
        'COMMENT ON ROLE ' + quote(role + '_restore_outside') + " IS 'owned-external-comment'",
      );
      await restoreRequest(roleBinding, 'reject-provenance', 409);
      await sql(
        'COMMENT ON ROLE ' +
          quote(role + '_restore_outside') +
          ' IS ' +
          literal('neon-control/branch/' + branch),
      );
      await restoreRequest(roleBinding, 'restore-after-owned-conflicts');
      expect((await sql(identitySQL, database, writer, role)).rows).toEqual(state.native_identity);
      await save('collision_and_provenance_conflicts_refuse_adoption');
    }
    if (!done('retained_cold_reader_uses_original_oid_credential_data')) {
      await zero(writer, 'before-reader');
      await zero(reader, 'before-reader');
      expect((await sql(identitySQL, database, reader, role)).rows).toEqual(state.native_identity);
      await zero(reader, 'reader-complete');
      await save('retained_cold_reader_uses_original_oid_credential_data');
    }
    if (!done('cold_writer_retains_original_oid_credential_data')) {
      expect((await sql(identitySQL, database, writer, role)).rows).toEqual(state.native_identity);
      expect(row(await inventory(), roleBinding).status).toBe('catalog_matches');
      expect(row(await inventory(), dbBinding).status).toBe('catalog_matches');
      await save('cold_writer_retains_original_oid_credential_data');
    }
    if (!done('original_customer_data_retained_and_computes_zero')) {
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            'c02_default_' + original.nonce,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      await zero(writer, 'final');
      await zero(reader, 'final');
      await save('original_customer_data_retained_and_computes_zero');
    }
    result = 'pass';
  } finally {
    await save();
  }
});
