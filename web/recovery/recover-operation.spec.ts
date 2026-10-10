import { expect, test } from '@playwright/test';
import { readFile, writeFile, mkdir } from 'node:fs/promises';

type Fixture = {
  project_id: string;
  operation_id: string;
  writer_id: string;
  child_endpoint_id: string;
  database_password_file: string;
  // Opt-in recovery of a known failed lifecycle reader. Keep the original
  // writable-branch recovery contract unchanged when this field is absent.
  purpose?:
    | 'lifecycle-reader'
    | 'endpoint-deletion'
    | 'historical-restore'
    | 'catalog-database'
    | 'native-writer';
  child_branch_id?: string;
  names?: { receipt: string; laterTable: string; laterRole: string; laterDatabase: string };
  sourcePoint?: { timestamp: string; lsn: string };
  resolved_parent_lsn?: string;
  manual_catalog_repair?: { database_oid: string; role_oid: string; verification_receipt: string };
  surviving_reader_id?: string;
  original_failure_job?: string;
  previous_attempts?: number;
};

test('UI recovers the existing operation after infrastructure failure', async ({
  page,
  context,
  baseURL,
}, info) => {
  const source = process.env.NEON_E2E_RECOVERY_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!source || !adminFile || !baseURL)
    throw new Error('Explicit recovery fixture and trusted login required');
  const fixture: Fixture = JSON.parse(await readFile(source, 'utf8'));
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^op_[a-f0-9]+$/.test(fixture.operation_id) ||
    ![fixture.writer_id, fixture.child_endpoint_id].every((id) => /^ep_[a-f0-9]{16}$/.test(id)) ||
    (fixture.writer_id === fixture.child_endpoint_id && fixture.purpose !== 'catalog-database') ||
    (fixture.purpose !== undefined &&
      ![
        'lifecycle-reader',
        'endpoint-deletion',
        'historical-restore',
        'catalog-database',
        'native-writer',
      ].includes(fixture.purpose))
  )
    throw new Error('Invalid explicit recovery identity');
  if (
    [
      'lifecycle-reader',
      'endpoint-deletion',
      'historical-restore',
      'catalog-database',
      'native-writer',
    ].includes(fixture.purpose || '') &&
    (!Number.isSafeInteger(fixture.previous_attempts) ||
      fixture.previous_attempts! < 1 ||
      fixture.previous_attempts! > 10)
  )
    throw new Error('Reader recovery requires the observed failed attempt count');
  if (
    fixture.purpose === 'endpoint-deletion' &&
    (!/^ep_[a-f0-9]{16}$/.test(fixture.surviving_reader_id || '') ||
      [fixture.writer_id, fixture.child_endpoint_id].includes(fixture.surviving_reader_id!) ||
      !/^publication-ui-[0-9]+$/.test(fixture.original_failure_job || ''))
  )
    throw new Error('Explicit endpoint retirement recovery identity required');
  if (
    fixture.purpose === 'historical-restore' &&
    (!/^br_[a-f0-9]{16}$/.test(fixture.child_branch_id || '') ||
      !/^publication-ui-[0-9]+$/.test(fixture.original_failure_job || '') ||
      !fixture.names ||
      !Object.values(fixture.names).every((name) => /^[a-z_0-9]{1,63}$/.test(name)) ||
      !/^[A-Fa-f0-9]+\/[A-Fa-f0-9]+$/.test(fixture.resolved_parent_lsn || ''))
  )
    throw new Error('Explicit original historical restore identity and safe probes required');
  if (
    fixture.purpose === 'catalog-database' &&
    (!/^br_[a-f0-9]{16}$/.test(fixture.child_branch_id || '') ||
      !/^publication-ui-[0-9]+$/.test(fixture.original_failure_job || '') ||
      fixture.writer_id !== fixture.child_endpoint_id ||
      !fixture.names ||
      !Object.values(fixture.names).every((name) => /^[a-z_0-9]{1,63}$/.test(name)))
  )
    throw new Error('Explicit original catalog database recovery identity required');
  const password = (await readFile(fixture.database_password_file, 'utf8')).trim();
  const adminPassword = (await readFile(adminFile, 'utf8')).trim();
  const checks: string[] = [];
  let result = 'running';
  await mkdir(info.outputDir, { recursive: true });
  async function save() {
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          checks,
          project_id: fixture.project_id,
          operation_id: fixture.operation_id,
          purpose: fixture.purpose || 'writable-branch',
          original_failure_job: fixture.original_failure_job,
          credentials_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  async function shot(name: string) {
    await page.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  async function sql(endpoint: string, statement: string, database?: string) {
    await page.goto('/#/projects/' + fixture.project_id + '/query');
    await page.locator('select').first().selectOption(endpoint);
    if (database) await page.getByLabel('目标数据库', { exact: true }).fill(database);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const response = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + endpoint + '/query') && r.request().method() === 'POST',
      { timeout: 190_000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const completed = await response;
    expect(completed.status()).toBe(200);
    return completed.json() as Promise<{ rows: unknown[][] }>;
  }
  async function suspend(endpoint: string) {
    await page.goto('/#/projects/' + fixture.project_id + '/compute');
    await page.locator('select').first().selectOption(endpoint);
    const response = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + endpoint + '/suspend') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    expect((await response).status()).toBe(202);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145_000,
    });
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill(adminPassword);
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const original = await context.request.get(
      baseURL + '/api/v1/projects/' + fixture.project_id + '/operations/' + fixture.operation_id,
    );
    expect(original.status()).toBe(200);
    const failed = await original.json();
    expect(failed.state).toBe('failed');
    expect(failed.retryable).toBe(true);
    expect(failed.resource_id).toBe(
      ['historical-restore', 'catalog-database'].includes(fixture.purpose || '')
        ? fixture.child_branch_id
        : fixture.child_endpoint_id,
    );
    expect(failed.action).toBe(
      fixture.purpose === 'endpoint-deletion'
        ? 'delete_endpoint'
        : fixture.purpose === 'historical-restore'
          ? 'create_branch'
          : fixture.purpose === 'catalog-database'
            ? 'create_database'
            : 'create_endpoint',
    );
    if (fixture.purpose === 'native-writer') {
      expect(failed.attempts).toBe(fixture.previous_attempts);
      expect(fixture.child_branch_id).toMatch(/^br_[a-f0-9]{16}$/);
      expect(fixture.original_failure_job).toMatch(/^publication-ui-[0-9]+$/);
      const response = await context.request.get(
        baseURL + '/api/v1/projects/' + fixture.project_id + '/endpoints',
      );
      expect(response.status()).toBe(200);
      const endpoints = (await response.json()).items;
      const child = endpoints.find((e: { id: string }) => e.id === fixture.child_endpoint_id);
      const parent = endpoints.find((e: { id: string }) => e.id === fixture.writer_id);
      expect(child.endpoint_type).toBe('read_write');
      expect(parent.endpoint_type).toBe('read_write');
      expect(child.branch_id).toBe(fixture.child_branch_id);
      expect(child.branch_id).not.toBe(parent.branch_id);
      checks.push('original_failed_native_writer_and_distinct_branch_identity_verified');
      await save();
    }
    if (fixture.purpose === 'catalog-database') {
      expect(failed.attempts).toBe(fixture.previous_attempts);
      const response = await context.request.get(
        baseURL +
          '/api/v1/projects/' +
          fixture.project_id +
          '/branches/' +
          fixture.child_branch_id +
          '/databases',
      );
      expect(response.status()).toBe(200);
      const databases = (await response.json()).items;
      const original = databases.find(
        (database: { name: string }) => database.name === fixture.names!.laterDatabase,
      );
      expect(original.owner).toBe(fixture.names!.laterRole);
      checks.push('original_failed_database_catalog_intent_and_owner_verified');
      await save();
    }
    if (fixture.purpose === 'historical-restore') {
      expect(failed.attempts).toBe(fixture.previous_attempts);
      const response = await context.request.get(
        baseURL + '/api/v1/projects/' + fixture.project_id + '/branches/' + fixture.child_branch_id,
      );
      expect(response.status()).toBe(200);
      const branch = await response.json();
      expect(branch.restore_source).toBe('timestamp');
      // Timestamp-to-LSN resolution is persisted by admission. The source
      // SQL pg_current_wal_flush_lsn snapshot can precede that exact point.
      expect(branch.parent_lsn).toBe(fixture.resolved_parent_lsn);
      expect(Date.parse(branch.parent_timestamp)).toBe(Date.parse(fixture.sourcePoint!.timestamp));
      checks.push('original_failed_restore_operation_branch_and_fixed_point_verified');
      await save();
    }
    if (fixture.purpose === 'endpoint-deletion') {
      expect(failed.attempts).toBe(fixture.previous_attempts);
      expect(failed.steps.map((step: { state: string }) => step.state)).toEqual([
        'succeeded',
        'succeeded',
        'failed',
        'queued',
      ]);
      const response = await context.request.get(
        baseURL + '/api/v1/projects/' + fixture.project_id + '/endpoints',
      );
      expect(response.status()).toBe(200);
      const endpoints = (await response.json()).items;
      // A failed retirement keeps its durable deleting intent until the
      // original Operation can commit a tombstone. It is still listed.
      expect(endpoints.map((endpoint: { id: string }) => endpoint.id).sort()).toEqual(
        [fixture.writer_id, fixture.child_endpoint_id, fixture.surviving_reader_id].sort(),
      );
      expect(
        endpoints.find((endpoint: { id: string }) => endpoint.id === fixture.child_endpoint_id)
          .state,
      ).toBe('deleting');
      checks.push('explicit_failed_endpoint_retirement_and_surviving_writer_reader_verified');
      await save();
    }
    if (fixture.purpose === 'lifecycle-reader') {
      expect(failed.attempts).toBe(fixture.previous_attempts);
      const response = await context.request.get(
        baseURL + '/api/v1/projects/' + fixture.project_id + '/endpoints',
      );
      expect(response.status()).toBe(200);
      const endpoints = (await response.json()).items;
      const reader = endpoints.find((e: { id: string }) => e.id === fixture.child_endpoint_id);
      const writer = endpoints.find((e: { id: string }) => e.id === fixture.writer_id);
      expect(reader.endpoint_type).toBe('read_only');
      expect(writer.endpoint_type).toBe('read_write');
      expect(reader.branch_id).toBe(writer.branch_id);
      checks.push('explicit_failed_reader_and_writer_identity_verified_before_retry');
      await save();
    }
    if (fixture.purpose === 'catalog-database' && fixture.manual_catalog_repair) {
      // Explicit operator recovery, not automatic adoption. The protected
      // fixture carries a prior read-only verification receipt and exact OIDs.
      // Recheck ownership and absence of user relations immediately before
      // repairing only this interrupted CREATE's missing branch comment.
      const repair = fixture.manual_catalog_repair;
      expect(repair.verification_receipt).toMatch(
        /^endpoint-catalog-unknown-verification-attempt[0-9]+$/,
      );
      expect(repair.database_oid).toMatch(/^[1-9][0-9]+$/);
      expect(repair.role_oid).toMatch(/^[1-9][0-9]+$/);
      const database = fixture.names!.laterDatabase,
        owner = fixture.names!.laterRole;
      const marker = 'neon-control/branch/' + fixture.child_branch_id;
      expect(
        (
          await sql(
            fixture.writer_id,
            `SELECT d.oid::text,d.datname,pg_get_userbyid(d.datdba),COALESCE(shobj_description(d.oid,'pg_database'),''),r.oid::text,COALESCE(shobj_description(r.oid,'pg_authid'),'') FROM pg_database d JOIN pg_roles r ON r.oid=d.datdba WHERE d.datname='${database}'`,
          )
        ).rows,
      ).toEqual([[repair.database_oid, database, owner, '', repair.role_oid, marker]]);
      expect(
        (
          await sql(
            fixture.writer_id,
            "SELECT current_database(),count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND c.relkind IN ('r','p','m','f')",
            database,
          )
        ).rows,
      ).toEqual([[database, 0]]);
      await sql(
        fixture.writer_id,
        `DO $recovery$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_database d JOIN pg_roles r ON r.oid=d.datdba WHERE d.oid=${repair.database_oid}::oid AND d.datname='${database}' AND r.oid=${repair.role_oid}::oid AND r.rolname='${owner}' AND COALESCE(shobj_description(d.oid,'pg_database'),'')='' AND shobj_description(r.oid,'pg_authid')='${marker}') THEN RAISE EXCEPTION 'Original catalog ownership changed; abort repair'; END IF; EXECUTE 'COMMENT ON DATABASE "${database}" IS ''${marker}'''; END $recovery$`,
      );
      checks.push('explicit_operator_verifies_original_empty_database_oids_before_comment_repair');
      await save();
    }
    await page.goto('/#/projects/' + fixture.project_id + '/operations');
    await page
      .getByRole('button', { name: '查看操作 ' + fixture.operation_id + ' 的步骤', exact: true })
      .click();
    const dialog = page.getByRole('dialog', { name: '操作详情' });
    await expect(dialog.getByRole('button', { name: '重试原操作', exact: true })).toBeVisible();
    await shot('failed-operation');
    const accepted = page.waitForResponse(
      (r) =>
        r.url().endsWith('/operations/' + fixture.operation_id + '/retry') &&
        r.request().method() === 'POST',
    );
    await dialog.getByRole('button', { name: '重试原操作', exact: true }).click();
    expect((await accepted).status()).toBe(202);
    await expect(dialog.locator('.detail-row').last().locator('.status')).toContainText('已完成', {
      timeout: 490_000,
    });
    const operation = await context.request.get(
      baseURL + '/api/v1/projects/' + fixture.project_id + '/operations/' + fixture.operation_id,
    );
    expect(operation.status()).toBe(200);
    const recovered = await operation.json();
    expect(recovered.id).toBe(fixture.operation_id);
    expect(recovered.resource_id).toBe(
      ['historical-restore', 'catalog-database'].includes(fixture.purpose || '')
        ? fixture.child_branch_id
        : fixture.child_endpoint_id,
    );
    expect(recovered.attempts).toBe((fixture.previous_attempts ?? 1) + 1);
    checks.push('same_operation_and_endpoint_recovered_from_ui');
    await save();
    await shot('operation-recovered');
    if (fixture.purpose === 'catalog-database') {
      expect(
        (
          await sql(
            fixture.writer_id,
            `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='${fixture.names!.laterDatabase}'`,
          )
        ).rows,
      ).toEqual([[fixture.names!.laterRole]]);
      expect(
        (
          await sql(
            fixture.writer_id,
            `SELECT marker,count(*) OVER() FROM public.${fixture.names!.receipt}`,
          )
        ).rows,
      ).toEqual([['after', 1]]);
      checks.push('original_database_owner_and_source_data_verified_in_postgres');
      await save();
      await suspend(fixture.writer_id);
      checks.push('original_catalog_fixture_compute_suspended_with_data_retained');
      await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
      await expect(
        page.getByRole('heading', { name: '监控与运行洞察', exact: true }),
      ).toBeVisible();
      await expect(page.getByTestId('monitor-runtime-state')).toHaveText('已休眠');
      await shot('monitoring-after-catalog-recovery');
      checks.push('monitoring_after_original_catalog_recovery');
      result = 'pass';
      return;
    }

    if (fixture.purpose === 'historical-restore') {
      expect(
        (
          await sql(
            fixture.child_endpoint_id,
            `SELECT marker FROM public.${fixture.names!.receipt} WHERE id=1`,
          )
        ).rows,
      ).toEqual([['before']]);
      expect(
        (
          await sql(
            fixture.child_endpoint_id,
            `SELECT to_regclass('public.${fixture.names!.laterTable}') IS NULL, NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='${fixture.names!.laterRole}'), NOT EXISTS(SELECT 1 FROM pg_database WHERE datname='${fixture.names!.laterDatabase}')`,
          )
        ).rows,
      ).toEqual([[true, true, true]]);
      checks.push('original_recovered_historical_data_schema_roles_and_databases_preserved');
      await save();
      await suspend(fixture.child_endpoint_id);
      expect(
        (
          await sql(
            fixture.writer_id,
            `SELECT marker,count(*) OVER() FROM public.${fixture.names!.receipt}`,
          )
        ).rows,
      ).toEqual([['after', 1]]);
      await suspend(fixture.writer_id);
      checks.push('source_current_data_unchanged_and_original_computes_suspended');
      await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
      await expect(
        page.getByRole('heading', { name: '监控与运行洞察', exact: true }),
      ).toBeVisible();
      await expect(page.getByTestId('monitor-runtime-state')).toHaveText('已休眠');
      await shot('monitoring-after-historical-recovery');
      checks.push('monitoring_after_original_historical_restore_recovery');
      result = 'pass';
      return;
    }

    if (fixture.purpose === 'endpoint-deletion') {
      expect(recovered.steps.map((step: { state: string }) => step.state)).toEqual([
        'succeeded',
        'succeeded',
        'succeeded',
        'succeeded',
      ]);
      const endpointPath =
        baseURL +
        '/api/v1/projects/' +
        fixture.project_id +
        '/endpoints/' +
        fixture.child_endpoint_id;
      expect((await context.request.get(endpointPath)).status()).toBe(404);
      expect((await context.request.get(endpointPath + '/connection-info')).status()).toBe(404);
      const lifecycle = await context.request.get(
        baseURL + '/api/v1/projects/' + fixture.project_id + '/lifecycle',
      );
      expect(lifecycle.status()).toBe(200);
      expect(
        (await lifecycle.json()).tombstones.some(
          (t: { resource_type: string; resource_id: string; physical_gc_state: string }) =>
            t.resource_type === 'endpoint' &&
            t.resource_id === fixture.child_endpoint_id &&
            t.physical_gc_state === 'held',
        ),
      ).toBe(true);
      checks.push('original_endpoint_closed_with_held_tombstone_after_same_operation_retry');
      await save();
      expect(
        (await sql(fixture.writer_id, 'SELECT * FROM public.endpoint_retention_probe ORDER BY id'))
          .rows,
      ).toEqual([[1, 'retained']]);
      expect(
        (
          await sql(
            fixture.surviving_reader_id!,
            'SELECT * FROM public.endpoint_retention_probe ORDER BY id',
          )
        ).rows,
      ).toEqual([[1, 'retained']]);
      expect(
        (
          await sql(
            fixture.surviving_reader_id!,
            "SELECT pg_is_in_recovery(),current_setting('transaction_read_only')",
          )
        ).rows,
      ).toEqual([[true, 'on']]);
      checks.push('surviving_writer_and_reader_keep_exact_sql_rows_and_read_only_replica');
      await save();
      await suspend(fixture.surviving_reader_id!);
      await suspend(fixture.writer_id);
      checks.push('original_owned_survivors_suspended_from_ui_with_data_retained');
      await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
      await expect(
        page.getByRole('heading', { name: '监控与运行洞察', exact: true }),
      ).toBeVisible();
      await expect(page.getByTestId('monitor-runtime-state')).toHaveText('已休眠');
      await shot('monitoring-after-endpoint-recovery');
      checks.push('monitoring_after_endpoint_recovery');
      result = 'pass';
      return;
    }

    if (fixture.purpose === 'lifecycle-reader') {
      expect(
        (await sql(fixture.child_endpoint_id, 'SELECT id, marker FROM public.lifecycle_probe'))
          .rows,
      ).toEqual([[1, 'retained-parent']]);
      expect(
        (
          await sql(
            fixture.child_endpoint_id,
            "SELECT pg_is_in_recovery(),current_setting('transaction_read_only')",
          )
        ).rows,
      ).toEqual([[true, 'on']]);
      checks.push('recovered_reader_inherits_retained_data_and_is_read_only');
      await save();
      await suspend(fixture.child_endpoint_id);
      expect(
        (await sql(fixture.writer_id, 'SELECT id, marker FROM public.lifecycle_probe')).rows,
      ).toEqual([[1, 'retained-parent']]);
      await suspend(fixture.writer_id);
      checks.push('writer_cold_wake_retains_data_and_both_computes_return_to_zero');
      await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
      await expect(
        page.getByRole('heading', { name: '监控与运行洞察', exact: true }),
      ).toBeVisible();
      await shot('monitoring-after-recovery');
      checks.push('monitoring_after_recovery');
      result = 'pass';
      return;
    }

    expect(
      (await sql(fixture.child_endpoint_id, 'SELECT id, marker FROM public.ui_release_probe')).rows,
    ).toEqual([[1, 'parent']]);
    await sql(fixture.child_endpoint_id, "INSERT INTO public.ui_release_probe VALUES(2, 'child')");
    checks.push('recovered_child_inherits_parent_and_accepts_writes');
    await save();
    await shot('child-data');
    await suspend(fixture.child_endpoint_id);
    expect(
      (await sql(fixture.writer_id, 'SELECT id, marker FROM public.ui_release_probe ORDER BY id'))
        .rows,
    ).toEqual([[1, 'parent']]);
    checks.push('parent_cold_resume_preserves_branch_isolation');
    await suspend(fixture.writer_id);
    checks.push('both_computes_suspended_via_ui');
    await page.goto('/#/projects/' + fixture.project_id + '/monitoring');
    await expect(page.getByRole('heading', { name: '监控与运行洞察', exact: true })).toBeVisible();
    await shot('monitoring-after-recovery');
    checks.push('monitoring_after_recovery');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    await shot('failure');
    throw error;
  } finally {
    await save();
  }
});
