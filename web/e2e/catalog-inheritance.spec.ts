import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Inherited SQL catalog and original credential generations survive first startup', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_CONNECTION_DEFAULTS_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!file || !privateDir || !adminFile)
    throw Error('Protected retained inheritance fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8')),
    original = JSON.parse(await readFile(fixture.resume_checkpoint_file, 'utf8')),
    source = JSON.parse(await readFile(fixture.source_catalog_bindings_checkpoint_file, 'utf8'));
  const project = fixture.project_id,
    branch = original.branch_id,
    writer = original.writer_id,
    reader = original.reader_id,
    root = '/api/v1/projects/' + project;
  if (
    source.project_id !== project ||
    source.branch_id !== branch ||
    source.writer_id !== writer ||
    !/^[0-9]{13}$/.test(source.nonce)
  )
    throw Error('Original accepted catalog scope required');
  const platformPassword = (
    await readFile(path.join(privateDir, 'customer-default-password'), 'utf8')
  ).trim();
  const state: any = fixture.resume_catalog_inheritance_checkpoint_file
    ? JSON.parse(await readFile(fixture.resume_catalog_inheritance_checkpoint_file, 'utf8'))
    : {
        nonce: Date.now().toString(),
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        reader_id: reader,
        checks: [],
        operations: {},
        versions: {},
        zero_sequences: {},
      };
  if (state.project_id !== project || state.branch_id !== branch || state.writer_id !== writer)
    throw Error('Original inheritance checkpoint scope changed');
  const sourceRole = 'c02_bound_' + source.nonce + '_renamed',
    sourceDB = 'c02_bound_' + source.nonce + '_db_renamed',
    childRole = sourceRole + '_child',
    owner = 'c02_bound_' + source.nonce + '_owner',
    driftRole = 'c02_bound_' + source.nonce + '_drift',
    driftDB = 'c02_bound_' + source.nonce + '_db_drift';
  const sourcePassword = platformPassword + '-binding-2-' + source.nonce,
    childPlatform = platformPassword + '-inherit-platform-' + state.nonce,
    childPassword = platformPassword + '-inherit-customer-' + state.nonce,
    laterChildPassword = platformPassword + '-inherit-later-' + state.nonce,
    dataPlatform = platformPassword + '-inherit-data-' + state.nonce;
  const bounds = {
    min_cpu_milli: 1000,
    max_cpu_milli: 1000,
    min_memory_mib: 1024,
    max_memory_mib: 1024,
  };
  let result = 'running';
  const done = (check: string) => state.checks.includes(check);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-inheritance-checkpoint.json'),
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
        source_role: sourceRole,
        source_database: sourceDB,
        child_role: childRole,
        checks: state.checks,
        operations: state.operations,
        child_branch_id: state.child_branch_id,
        child_writer_id: state.child_writer_id,
        child_reader_id: state.child_reader_id,
        data_branch_id: state.data_branch_id,
        original_data_writer_id: state.original_data_writer_id,
        data_writer_id: state.data_writer_id,
        native_oids: source.native_oids,
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
          const operation = await get(root + '/operations/' + id);
          if (operation.state === 'failed')
            throw Error('Original inheritance operation failed ' + id);
          return operation.state;
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
      const operation = await get(root + '/operations/' + state.operations[label]);
      return { operation, resource: { id: operation.resource_id } };
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
        'Idempotency-Key': 'c02-inherit-' + state.nonce + '-' + label,
        ...(version ? { 'If-Match': '"' + version + '"' } : {}),
      },
      ...(data ? { data } : {}),
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await observe(value.operation.id);
    return value;
  }
  async function sql(
    statement: string,
    endpoint = writer,
    database = original.platform_database,
    role = original.platform_role,
    password = platformPassword,
  ) {
    const response = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: { role, database, password, sql: statement },
      timeout: 190000,
    });
    expect(response.status()).toBe(200);
    return response.json();
  }
  const literal = (value: string) => "'" + value.replaceAll("'", "''") + "'";
  async function snapshot(endpoint: string, password: string) {
    const roleNames = [driftRole, 'c02_identity_1792496820961_replace', 'c02_cold_1792538268953'];
    const databaseNames = [driftDB, 'c02_cold_1792538268953_db', 'postgres'];
    const roles = (
      await sql(
        'SELECT oid::text,rolname,md5(rolpassword),rolcanlogin,rolinherit,rolconnlimit::text FROM pg_authid WHERE rolname IN (' +
          roleNames.map(literal).join(',') +
          ') ORDER BY rolname',
        endpoint,
        original.platform_database,
        original.platform_role,
        password,
      )
    ).rows;
    const databases = (
      await sql(
        "SELECT oid::text,datname,datdba::text,datconnlimit::text,md5(COALESCE(shobj_description(oid,'pg_database'),'')) FROM pg_database WHERE datname IN (" +
          databaseNames.map(literal).join(',') +
          ') ORDER BY datname',
        endpoint,
        original.platform_database,
        original.platform_role,
        password,
      )
    ).rows;
    expect(roles).toHaveLength(roleNames.length);
    expect(databases).toHaveLength(databaseNames.length);
    return { roles, databases };
  }
  async function healthy(endpoint: string, platform: string, role: string, password: string) {
    expect(
      (
        await sql(
          'SELECT oid::text FROM pg_roles WHERE rolname=' + literal(role),
          endpoint,
          original.platform_database,
          original.platform_role,
          platform,
        )
      ).rows[0][0],
    ).toBe(source.native_oids.role);
    expect(
      (
        await sql(
          'SELECT oid::text FROM pg_database WHERE datname=' + literal(sourceDB),
          endpoint,
          original.platform_database,
          original.platform_role,
          platform,
        )
      ).rows[0][0],
    ).toBe(source.native_oids.database);
    expect(
      (
        await sql(
          'SELECT marker FROM public.catalog_binding_probe WHERE id=1',
          endpoint,
          sourceDB,
          role,
          password,
        )
      ).rows[0][0],
    ).toBe('retained-catalog-binding-data');
  }
  async function current(branchID: string, kind: string, name: string) {
    return (await get(root + '/branches/' + branchID + '/' + kind)).items.find(
      (item: any) => item.name === name,
    );
  }
  async function zero(endpoint: string, label: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state === 'suspended') return;
    let sequence = state.zero_sequences[label] || 0;
    if (state.operations['zero-' + label + '-' + sequence]) sequence++;
    state.zero_sequences[label] = sequence;
    await save();
    await mutate(
      'POST',
      root + '/endpoints/' + endpoint + '/suspend',
      'zero-' + label + '-' + sequence,
      {},
    );
    await expect
      .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
        timeout: 150000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  async function excluded(branchID: string) {
    for (const [kind, name] of [
      ['roles', driftRole],
      ['databases', driftDB],
    ]) {
      const item = await current(branchID, kind, name);
      expect(item.managed).toBe(false);
      expect(item.state).toBe('external_identity');
      expect(item.management_excluded).toBe(true);
    }
    const response = await context.request.patch(
      root + '/branches/' + branchID + '/roles/' + driftRole,
      {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-inherit-' + state.nonce + '-excluded-' + branchID,
        },
        data: { password: childPassword },
        timeout: 190000,
      },
    );
    expect(response.status()).toBe(409);
    expect(JSON.stringify(await response.json())).toContain(
      'catalog_inheritance_identity_excluded',
    );
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('first_fork_preserves_original_catalog_and_excludes_replacements')) {
      if (!state.private_parent_snapshot) {
        state.private_parent_snapshot = await snapshot(writer, platformPassword);
        await save();
      }
      if (!state.child_branch_id) {
        const value = await mutate('POST', root + '/branches', 'create-child', {
          name: 'ci-catalog-inherit-' + state.nonce,
          parent_branch_id: branch,
          password: childPlatform,
          function_copy_policy: 'omit',
          autoscaling: bounds,
        });
        state.child_branch_id = value.resource.id;
        await save();
      }
      state.child_writer_id = (await get(root + '/endpoints')).items.find(
        (item: any) =>
          item.branch_id === state.child_branch_id && item.endpoint_type === 'read_write',
      )?.id;
      if (!state.child_writer_id) throw Error('Original inherited Writer required');
      await save();
      await healthy(state.child_writer_id, childPlatform, sourceRole, sourcePassword);
      expect(await snapshot(state.child_writer_id, childPlatform)).toEqual(
        state.private_parent_snapshot,
      );
      await excluded(state.child_branch_id);
      await save('first_fork_preserves_original_catalog_and_excludes_replacements');
    }
    const childBase = root + '/branches/' + state.child_branch_id;
    if (!done('inherited_credential_uid_rename_rotate_owner_and_parent_isolation')) {
      let item = await current(state.child_branch_id, 'roles', sourceRole);
      await mutate(
        'POST',
        childBase + '/roles/' + sourceRole + '/rename',
        'rename-inherited-role',
        { new_name: childRole },
        item?.version_token,
      );
      if (!state.operations['rotate-child'])
        await healthy(state.child_writer_id, childPlatform, childRole, sourcePassword);
      await mutate('PATCH', childBase + '/roles/' + childRole, 'rotate-child', {
        password: childPassword,
      });
      item = await current(state.child_branch_id, 'databases', sourceDB);
      await mutate(
        'PATCH',
        childBase + '/databases/' + sourceDB,
        'owner-away',
        { owner },
        item?.version_token,
      );
      item = await current(state.child_branch_id, 'databases', sourceDB);
      await mutate(
        'PATCH',
        childBase + '/databases/' + sourceDB,
        'owner-back',
        { owner: childRole },
        item?.version_token,
      );
      await healthy(state.child_writer_id, childPlatform, childRole, childPassword);
      await healthy(writer, platformPassword, sourceRole, sourcePassword);
      expect(await snapshot(state.child_writer_id, childPlatform)).toEqual(
        state.private_parent_snapshot,
      );
      await save('inherited_credential_uid_rename_rotate_owner_and_parent_isolation');
    }
    if (!done('new_reader_and_cold_computes_preserve_catalog_and_credentials')) {
      if (!state.child_reader_id) {
        const value = await mutate('POST', root + '/endpoints', 'create-child-reader', {
          branch_id: state.child_branch_id,
          type: 'read_only',
          autoscaling: bounds,
        });
        state.child_reader_id = value.resource.id;
        await save();
      }
      for (const endpoint of [state.child_writer_id, state.child_reader_id]) {
        await zero(endpoint, 'child-before-' + endpoint);
        await healthy(endpoint, childPlatform, childRole, childPassword);
        expect(await snapshot(endpoint, childPlatform)).toEqual(state.private_parent_snapshot);
        await zero(endpoint, 'child-after-' + endpoint);
      }
      await save('new_reader_and_cold_computes_preserve_catalog_and_credentials');
    }
    if (!done('data_only_fork_keeps_original_generation_after_parent_rotation')) {
      if (!state.data_branch_id) {
        const value = await mutate('POST', root + '/branches', 'create-data-only', {
          name: 'ci-catalog-data-only-' + state.nonce,
          parent_branch_id: state.child_branch_id,
          create_endpoint: false,
          function_copy_policy: 'omit',
          autoscaling: bounds,
        });
        state.data_branch_id = value.resource.id;
        await save();
      }
      await mutate('PATCH', childBase + '/roles/' + childRole, 'rotate-child-after-data-fork', {
        password: laterChildPassword,
      });
      if (!state.data_writer_id) {
        const value = await mutate('POST', root + '/endpoints', 'create-data-writer', {
          branch_id: state.data_branch_id,
          type: 'read_write',
          password: dataPlatform,
          autoscaling: bounds,
        });
        state.data_writer_id = value.resource.id;
        state.original_data_writer_id = value.resource.id;
        await save();
      }
      await healthy(state.data_writer_id, dataPlatform, childRole, childPassword);
      await healthy(state.child_writer_id, childPlatform, childRole, laterChildPassword);
      await excluded(state.data_branch_id);
      expect(await snapshot(state.data_writer_id, dataPlatform)).toEqual(
        state.private_parent_snapshot,
      );
      await save('data_only_fork_keeps_original_generation_after_parent_rotation');
    }
    if (!done('replacement_writer_preserves_initialized_branch_catalog')) {
      if (!state.operations['retire-data-writer']) {
        await zero(state.original_data_writer_id, 'data-before-retirement');
        const endpoint = await get(root + '/endpoints/' + state.original_data_writer_id);
        await mutate(
          'DELETE',
          root + '/endpoints/' + state.original_data_writer_id,
          'retire-data-writer',
          { confirm_selector: endpoint.selector },
          String(endpoint.version),
        );
      } else await observe(state.operations['retire-data-writer']);
      const value = await mutate('POST', root + '/endpoints', 'replace-data-writer', {
        branch_id: state.data_branch_id,
        type: 'read_write',
        password: dataPlatform,
        autoscaling: bounds,
      });
      state.data_writer_id = value.resource.id;
      await save();
      expect(state.data_writer_id).not.toBe(state.original_data_writer_id);
      await healthy(state.data_writer_id, dataPlatform, childRole, childPassword);
      expect(await snapshot(state.data_writer_id, dataPlatform)).toEqual(
        state.private_parent_snapshot,
      );
      await save('replacement_writer_preserves_initialized_branch_catalog');
    }
    if (!done('original_parent_data_retained_and_all_computes_zero')) {
      await healthy(writer, platformPassword, sourceRole, sourcePassword);
      expect(
        (
          await sql(
            'SELECT marker FROM public.customer_default_probe WHERE id=1',
            writer,
            'c02_default_' + original.nonce,
          )
        ).rows[0][0],
      ).toBe('retained-c02-default-data');
      await page.goto('/#/projects/' + project + '/databases');
      await page.getByLabel('管理分支', { exact: true }).selectOption(state.child_branch_id);
      await expect(page.getByRole('row').filter({ hasText: sourceDB }).first()).toContainText(
        childRole,
        { timeout: 190000 },
      );
      for (const endpoint of [
        ...new Set([
          writer,
          reader,
          state.child_writer_id,
          state.child_reader_id,
          state.data_writer_id,
        ]),
      ])
        await zero(endpoint as string, 'final-' + endpoint);
      await save('original_parent_data_retained_and_all_computes_zero');
    }
    // Catalog navigation may preload the project's default branch before the
    // retained work branch is selected. Retire only that normal UI wake too.
    const uiParentWriter = (await get(root + '/endpoints')).items.find(
      (item: any) =>
        item.branch_id === fixture.parent_branch_id && item.endpoint_type === 'read_write',
    );
    if (uiParentWriter) await zero(uiParentWriter.id, 'final-ui-parent');
    result = 'pass';
  } finally {
    await save();
  }
});
