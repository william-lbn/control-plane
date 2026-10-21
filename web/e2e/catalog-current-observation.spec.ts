import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Legacy inherited OIDs connect to current intent without rewriting first observation', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!file || !privateDir || !adminFile)
    throw Error('Protected accepted inheritance fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8'));
  const original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'));
  const inherited = JSON.parse(
    await readFile(fixture.source_catalog_inheritance_checkpoint_file, 'utf8'),
  );
  const project = fixture.project_id,
    branch = inherited.child_branch_id,
    root = '/api/v1/projects/' + project,
    base = root + '/branches/' + branch;
  if (
    inherited.project_id !== project ||
    inherited.branch_id !== original.branch_id ||
    inherited.checks.length !== 6
  )
    throw Error('Original accepted source required');
  const platform = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const childPlatform = platform + '-inherit-platform-' + inherited.nonce;
  const role = 'c02_customer_1792429437379',
    database = 'c02_default_1792429437379',
    renamedRole = role + '_observed';
  const state: any = fixture.resume_catalog_current_observation_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_current_observation_checkpoint_file, 'utf8'))
    : {
        project_id: project,
        branch_id: branch,
        nonce: Date.now().toString(),
        checks: [],
        operations: {},
        versions: {},
        zero_sequences: {},
      };
  if (state.project_id !== project || state.branch_id !== branch)
    throw Error('Observation checkpoint scope changed');
  const nextPassword = platform + '-current-observation-' + state.nonce;
  let result = 'running';
  const done = (check: string) => state.checks.includes(check);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-current-observation-checkpoint.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        branch_id: branch,
        role,
        renamed_role: renamedRole,
        database,
        native_oids: { role: 196608, database: 196612 },
        checks: state.checks,
        operations: state.operations,
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
  const inventory = () => get(base + '/catalog-identities');
  async function observe(operation: string) {
    await expect
      .poll(
        async () => {
          const value = await get(root + '/operations/' + operation);
          if (value.state === 'failed')
            throw Error('Original observation operation failed ' + operation);
          return value.state;
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
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return;
    }
    if (version && !state.versions[label]) {
      state.versions[label] = version;
      await save();
    }
    const response = await context.request.fetch(target, {
      method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-observe-' + state.nonce + '-' + label,
        ...(state.versions[label] ? { 'If-Match': '"' + state.versions[label] + '"' } : {}),
      },
      data,
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    state.operations[label] = (await response.json()).operation.id;
    await save();
    await observe(state.operations[label]);
  }
  async function sql(
    statement: string,
    endpoint = inherited.child_writer_id,
    db = 'postgres',
    loginRole = 'cloud_admin',
    password = childPlatform,
  ) {
    const response = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { sql: statement, database: db, role: loginRole, password },
      timeout: 190000,
    });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function zero(endpoint: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state === 'suspended') return;
    let sequence = state.zero_sequences[endpoint] || 0;
    if (state.operations['zero-' + endpoint + '-' + sequence]) sequence++;
    state.zero_sequences[endpoint] = sequence;
    await save();
    await mutate(
      'POST',
      root + '/endpoints/' + endpoint + '/suspend',
      'zero-' + endpoint + '-' + sequence,
      {},
    );
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  async function initial(kind: string, name: string, oid: number) {
    const response = await context.request.post(base + '/catalog-identities', {
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-observe-' + state.nonce + '-initial-' + kind,
        'If-Match': '"1"',
      },
      data: { kind, name, oid, acknowledge_initial_observation: true },
      timeout: 190000,
    });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function bridgeAPI(kind: string, name: string) {
    const response = await context.request.post(
      base + '/catalog-identities/' + state['initial_' + kind].binding_id + '/observe-current',
      {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-observe-' + state.nonce + '-bridge-' + kind,
          'If-Match': '"1"',
        },
        data: { name, acknowledge_current_observation: true },
        timeout: 190000,
      },
    );
    expect(response.status()).toBe(200);
    return response.json();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('inherited_original_parent_credential_and_native_oid_first_observation')) {
      const before = await inventory();
      expect(
        before.native_objects.find((v: any) => v.kind === 'role' && v.name === role),
      ).toMatchObject({ oid: 196608, managed: true });
      state.initial_role = await initial('role', role, 196608);
      await save();
      state.initial_database = await initial('database', database, 196612);
      await save();
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            inherited.child_writer_id,
            database,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      await save('inherited_original_parent_credential_and_native_oid_first_observation');
    }
    if (!done('console_explicit_current_observation_and_owner_order')) {
      // Persist the real UI request before sending it, so an interrupted run can
      // replay its original key without creating a second observation.
      if (!state.bridge_role) {
        if (state.role_request) {
          const response = await context.request.post(state.role_request.url, {
            headers: {
              ...(await headers()),
              'Idempotency-Key': state.role_request.key,
              'If-Match': state.role_request.version,
            },
            data: state.role_request.body,
            timeout: 190000,
          });
          expect(response.status()).toBe(200);
          state.bridge_role = await response.json();
          await save();
        } else {
          await page.goto('/#/projects/' + project + '/databases');
          await page.getByLabel('管理分支', { exact: true }).selectOption(branch);
          await page.getByRole('button', { name: '读取身份变化', exact: true }).click();
          const form = page.getByRole('form', { name: '衔接当前身份', exact: true });
          await expect(
            form
              .getByLabel('衔接当前身份对象', { exact: true })
              .locator('option[value="' + state.initial_role.binding_id + '"]'),
          ).toHaveCount(1, { timeout: 190000 });
          await form
            .getByLabel('衔接当前身份对象', { exact: true })
            .selectOption(state.initial_role.binding_id);
          await form.getByRole('checkbox').check();
          await page.route('**/catalog-identities/*/observe-current', async (route) => {
            const request = route.request();
            state.role_request = {
              url: new URL(request.url()).pathname,
              key: request.headers()['idempotency-key'],
              version: request.headers()['if-match'],
              body: request.postDataJSON(),
            };
            await save();
            await route.continue();
          });
          const responsePromise = page.waitForResponse(
            (response) =>
              response.request().method() === 'POST' && response.url().endsWith('/observe-current'),
            { timeout: 190000 },
          );
          await form.getByRole('button', { name: '确认衔接当前身份', exact: true }).click();
          const response = await responsePromise;
          expect(response.status()).toBe(200);
          state.bridge_role = await response.json();
          await save();
          await page.unroute('**/catalog-identities/*/observe-current');
          await expect(form.getByRole('status')).toContainText('当前身份已衔接');
        }
      }
      state.bridge_database = await bridgeAPI('database', database);
      await save();
      for (const [kind, name, oid] of [
        ['role', role, 196608],
        ['database', database, 196612],
      ]) {
        expect(state['bridge_' + kind]).toMatchObject({
          source_binding_id: state['initial_' + kind].binding_id,
          object_oid: oid,
          version_token: '1',
          basis: 'explicit_current_observation',
          credentials: 'not_observed',
          options: 'not_recorded',
        });
        expect(
          (await inventory()).current_bindings.find(
            (v: any) => v.kind === kind && v.expected_name === name,
          ),
        ).toMatchObject({
          expected_oid: oid,
          source_type: 'current_observation',
          status: 'catalog_matches',
        });
      }
      await save('console_explicit_current_observation_and_owner_order');
    }
    if (!done('bound_inherited_rename_rotation_and_original_receipt_replay')) {
      const item = (await get(base + '/roles')).items.find((v: any) => v.name === role);
      await mutate(
        'POST',
        base + '/roles/' + role + '/rename',
        'rename-legacy',
        { new_name: renamedRole },
        item?.version_token,
      );
      await mutate('PATCH', base + '/roles/' + renamedRole, 'rotate-legacy', {
        password: nextPassword,
      });
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            inherited.child_writer_id,
            database,
            renamedRole,
            nextPassword,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      expect(
        (await sql("SELECT oid::text FROM pg_roles WHERE rolname='" + renamedRole + "'"))
          .rows[0][0],
      ).toBe('196608');
      expect(
        (
          await sql(
            "SELECT oid::text,datdba::text FROM pg_database WHERE datname='" + database + "'",
          )
        ).rows[0],
      ).toEqual(['196612', '196608']);
      const response = await context.request.post(state.role_request.url, {
        headers: {
          ...(await headers()),
          'Idempotency-Key': state.role_request.key,
          'If-Match': state.role_request.version,
        },
        data: state.role_request.body,
        timeout: 190000,
      });
      expect(response.status()).toBe(200);
      expect(await response.json()).toEqual(state.bridge_role);
      const value = await inventory();
      expect(
        value.items.find((v: any) => v.binding_id === state.initial_role.binding_id),
      ).toMatchObject({
        expected_name: role,
        expected_oid: 196608,
        intent_version: '1',
        status: 'intent_changed',
      });
      expect(
        value.current_bindings.find(
          (v: any) => v.kind === 'role' && v.expected_name === renamedRole,
        ),
      ).toMatchObject({ expected_oid: 196608, status: 'catalog_matches' });
      await save('bound_inherited_rename_rotation_and_original_receipt_replay');
    }
    if (!done('child_writer_reader_cold_catalog_and_source_retained')) {
      await zero(inherited.child_reader_id);
      await zero(inherited.child_writer_id);
      for (const endpoint of [inherited.child_writer_id, inherited.child_reader_id])
        expect(
          (
            await sql(
              'SELECT marker FROM public.customer_default_probe WHERE id=1',
              endpoint,
              database,
              renamedRole,
              nextPassword,
            )
          ).rows[0][0],
        ).toBe('retained-c02-default-data');
      expect(
        (
          await sql(
            'SELECT oid::text,rolname FROM pg_roles WHERE oid=196608',
            original.writer_id,
            'postgres',
            'cloud_admin',
            platform,
          )
        ).rows[0],
      ).toEqual(['196608', role]);
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            original.writer_id,
            database,
            'cloud_admin',
            platform,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      await save('child_writer_reader_cold_catalog_and_source_retained');
    }
    const parentWriter = (await get(root + '/endpoints')).items.find(
      (v: any) => v.branch_id === fixture.parent_branch_id && v.endpoint_type === 'read_write',
    );
    for (const endpoint of [
      ...new Set(
        [
          original.writer_id,
          original.reader_id,
          inherited.child_writer_id,
          inherited.child_reader_id,
          inherited.data_writer_id,
          parentWriter?.id,
        ].filter(Boolean),
      ),
    ])
      await zero(endpoint as string);
    await save('retained_original_scopes_and_observation_computes_zero');
    result = 'pass';
  } finally {
    await save();
  }
});
