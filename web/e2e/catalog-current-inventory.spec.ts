import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Current native identity generations remain distinct from first-observation history', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile)
    throw Error('Protected retained current-identity fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8'));
  const bindings = JSON.parse(
    await readFile(fixture.source_catalog_bindings_checkpoint_file, 'utf8'),
  );
  const inherited = JSON.parse(
    await readFile(fixture.source_catalog_inheritance_checkpoint_file, 'utf8'),
  );
  const project = fixture.project_id,
    branch = original.branch_id,
    root = '/api/v1/projects/' + project;
  if (
    bindings.project_id !== project ||
    inherited.project_id !== project ||
    inherited.branch_id !== branch ||
    inherited.checks.length !== 6
  )
    throw Error('Original accepted inheritance scope required');
  const sourceRole = 'c02_bound_' + bindings.nonce + '_renamed';
  const sourceDatabase = 'c02_bound_' + bindings.nonce + '_db_renamed';
  const childRole = sourceRole + '_child';
  const state: any = fixture.resume_catalog_current_inventory_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_current_inventory_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        project_id: project,
        branch_id: branch,
        checks: [],
        operations: {},
        zero_sequences: {},
      };
  if (state.project_id !== project || state.branch_id !== branch)
    throw Error('Current-identity checkpoint scope changed');
  let result = 'running';
  const done = (name: string) => state.checks.includes(name);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-current-inventory-checkpoint.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        branch_id: branch,
        writer_id: original.writer_id,
        reader_id: original.reader_id,
        child_branch_id: inherited.child_branch_id,
        child_writer_id: inherited.child_writer_id,
        child_reader_id: inherited.child_reader_id,
        data_branch_id: inherited.data_branch_id,
        data_writer_id: inherited.data_writer_id,
        source_role: sourceRole,
        source_database: sourceDatabase,
        child_role: childRole,
        native_oids: bindings.native_oids,
        checks: state.checks,
        operations: state.operations,
        catalog_mutations_admitted: 0,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function headers() {
    const csrf = (await context.cookies()).find((item) => item.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(target: string) {
    const response = await context.request.get(target, { timeout: 190000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  const inventory = (branchID: string) =>
    get(root + '/branches/' + branchID + '/catalog-identities');
  const current = (value: any, kind: string, name: string) =>
    value.current_bindings.find((item: any) => item.kind === kind && item.expected_name === name);
  function noPrivateBackings(value: any) {
    const encoded = JSON.stringify(value);
    for (const field of [
      'credential_ref',
      'credential_uid',
      'encrypted_password',
      'marker_sha256',
      'SCRAM-SHA-256',
      'neon-control/branch/',
    ])
      expect(encoded).not.toContain(field);
  }
  async function observe(operation: string) {
    await expect
      .poll(
        async () => {
          const value = await get(root + '/operations/' + operation);
          if (value.state === 'failed')
            throw Error('Original current-inventory suspension failed ' + operation);
          return value.state;
        },
        { timeout: 150000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function zero(endpoint: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state === 'suspended') return;
    let sequence = state.zero_sequences[endpoint] || 0;
    if (state.operations['zero-' + endpoint + '-' + sequence]) sequence++;
    state.zero_sequences[endpoint] = sequence;
    await save();
    const label = 'zero-' + endpoint + '-' + sequence;
    const response = await context.request.post(root + '/endpoints/' + endpoint + '/suspend', {
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'c02-current-' + state.nonce + '-' + label,
      },
      data: {},
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    state.operations[label] = (await response.json()).operation.id;
    await save();
    await observe(state.operations[label]);
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('current_create_generations_and_original_baselines_are_distinct')) {
      const value = await inventory(branch);
      noPrivateBackings(value);
      expect(value.items).toHaveLength(3);
      expect(value.items.map((item: any) => item.expected_oid).sort()).toEqual([
        212992, 213000, 213004,
      ]);
      for (const [kind, name, oid, version] of [
        ['role', sourceRole, 270336, '5'],
        ['database', sourceDatabase, 270344, '6'],
      ]) {
        const item = current(value, kind as string, name as string);
        expect(item).toMatchObject({
          expected_oid: oid,
          observed_oid: oid,
          intent_version: version,
          current_intent_version: version,
          status: 'catalog_matches',
          basis: 'current_native_identity',
          credentials: 'not_observed',
          options: 'not_recorded',
        });
        const native = value.native_objects.find(
          (row: any) => row.kind === kind && row.name === name,
        );
        expect(native).toMatchObject({
          managed: true,
          can_initialize: false,
          identity_status: 'catalog_matches',
        });
      }
      await save('current_create_generations_and_original_baselines_are_distinct');
    }
    if (!done('inherited_current_identity_is_visible_at_independent_child_generations')) {
      const child = await inventory(inherited.child_branch_id),
        data = await inventory(inherited.data_branch_id);
      for (const value of [child, data]) {
        noPrivateBackings(value);
        expect(value.items).toHaveLength(0);
      }
      const childIdentity = current(child, 'role', childRole),
        dataIdentity = current(data, 'role', childRole);
      expect(childIdentity).toMatchObject({
        expected_oid: 270336,
        status: 'catalog_matches',
        intent_version: '4',
        current_intent_version: '4',
      });
      expect(dataIdentity).toMatchObject({
        expected_oid: 270336,
        status: 'catalog_matches',
        source_type: 'inherited_identity',
        intent_version: '1',
        current_intent_version: '1',
      });
      expect(childIdentity.identity_id).not.toBe(dataIdentity.identity_id);
      expect(current(data, 'database', sourceDatabase)).toMatchObject({
        expected_oid: 270344,
        status: 'catalog_matches',
        source_type: 'inherited_identity',
      });
      await save('inherited_current_identity_is_visible_at_independent_child_generations');
    }
    if (!done('same_name_replacements_and_inherited_exclusions_cannot_reinitialize')) {
      const source = await inventory(branch);
      const driftRole = 'c02_bound_' + bindings.nonce + '_drift';
      expect(current(source, 'role', driftRole)).toMatchObject({
        expected_oid: 270352,
        status: 'name_collision',
      });
      const replacement = source.native_objects.find(
        (item: any) => item.kind === 'role' && item.name === driftRole,
      );
      expect(replacement).toMatchObject({
        managed: false,
        can_initialize: false,
        identity_status: 'name_collision',
      });
      for (const [name, oid] of [
        [sourceRole, 270336],
        [driftRole, replacement.oid],
      ]) {
        const role = (await get(root + '/branches/' + branch + '/roles')).items.find(
          (item: any) => item.name === name,
        );
        const response = await context.request.post(
          root + '/branches/' + branch + '/catalog-identities',
          {
            headers: {
              ...(await headers()),
              'Idempotency-Key': 'c02-current-' + state.nonce + '-reject-' + name,
              'If-Match': '"' + role.version_token + '"',
            },
            data: { kind: 'role', name, oid, acknowledge_initial_observation: true },
            timeout: 190000,
          },
        );
        expect(response.status()).toBe(409);
        expect(JSON.stringify(await response.json())).toContain('catalog_baseline_exists');
      }
      for (const branchID of [inherited.child_branch_id, inherited.data_branch_id]) {
        const value = await inventory(branchID);
        for (const [kind, name] of [
          ['role', driftRole],
          ['database', 'c02_bound_' + bindings.nonce + '_db_drift'],
        ])
          expect(
            value.native_objects.find((item: any) => item.kind === kind && item.name === name),
          ).toMatchObject({ managed: false, can_initialize: false, management_excluded: true });
      }
      await save('same_name_replacements_and_inherited_exclusions_cannot_reinitialize');
    }
    if (!done('console_current_identity_and_inherited_source_are_readable')) {
      await page.goto('/#/projects/' + project + '/databases');
      await page.getByLabel('管理分支', { exact: true }).selectOption(inherited.data_branch_id);
      await page.getByRole('button', { name: '读取身份变化', exact: true }).click();
      const table = page.getByRole('table', { name: '当前目录身份', exact: true });
      const row = table
        .getByRole('row')
        .filter({ hasText: '角色 · ' + childRole })
        .first();
      await expect(row).toContainText('270336', { timeout: 190000 });
      await expect(row).toContainText('分支原身份继承');
      await expect(row).toContainText('历史选项：未记录');
      const candidates = page.getByLabel('首次记录原身份', { exact: true });
      await expect(candidates.locator('option').filter({ hasText: childRole })).toHaveCount(0);
      await page
        .getByText('完整系统目录（含 NOLOGIN 角色及禁止连接的数据库）', { exact: true })
        .click();
      await expect(
        page
          .getByRole('listitem')
          .filter({ hasText: 'c02_bound_' + bindings.nonce + '_db_drift · OID ' })
          .first(),
      ).toContainText('平台管理已隔离');
      await save('console_current_identity_and_inherited_source_are_readable');
    }
    const uiParentWriter = (await get(root + '/endpoints')).items.find(
      (item: any) =>
        item.branch_id === fixture.parent_branch_id && item.endpoint_type === 'read_write',
    );
    for (const endpoint of [
      ...new Set(
        [
          original.writer_id,
          original.reader_id,
          inherited.child_writer_id,
          inherited.child_reader_id,
          inherited.data_writer_id,
          uiParentWriter?.id,
        ].filter(Boolean),
      ),
    ])
      await zero(endpoint as string);
    await save('all_original_identities_retained_and_observation_computes_zero');
    result = 'pass';
  } finally {
    await save();
  }
});
