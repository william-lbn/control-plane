import { expect, test } from '@playwright/test';
import { generateKeyPairSync, randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Only a fresh, owned canary is mutated. Checkpoints retain the exact inputs,
// keys and Operation IDs on Linux so a failed run resumes its original work.
test('Platform OID drift and dormant Backend database name migration retain original data', async ({
  page,
  context,
  baseURL,
}, info) => {
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!privateDir || !admin || !zip || !baseURL)
    throw Error('Protected Linux canary inputs required');
  const resume = process.env.NEON_E2E_CATALOG_PLATFORM_RESUME;
  const key = generateKeyPairSync('ed25519').publicKey.export({ format: 'jwk' });
  const state: any = resume
    ? JSON.parse(await readFile(resume, 'utf8'))
    : {
        nonce: Date.now().toString(),
        password: randomBytes(30).toString('base64url'),
        checks: [],
        requests: {},
        responses: {},
        provider: { ...key, alg: 'EdDSA', kid: 'catalog-platform', use: 'sig' },
      };
  const oldDB = 'catalog_backend_original',
    newDB = 'catalog_backend_renamed',
    role = 'catalog_backend_owner';
  let result = 'running';
  const root = () => '/api/v1/projects/' + state.project;
  const base = () => root() + '/branches/' + state.branch;
  const functions = () => base() + '/functions/canary';
  const done = (v: string) => state.checks.includes(v);
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'catalog-platform-checkpoint.private.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: state.project,
        branch_id: state.branch,
        writer_id: state.writer,
        function_id: state.functionID,
        checks: state.checks,
        operations: Object.fromEntries(
          Object.entries(state.responses)
            .filter(([, v]: [string, any]) => v.operation)
            .map(([k, v]: [string, any]) => [k, v.operation.id]),
        ),
        database_oids: state.databaseOIDs,
        baseline_id: state.baselineID,
        credentials_in_evidence: false,
        deferred_checks: state.deferredChecks || [],
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function headers() {
    const csrf = (await context.cookies()).find((v) => v.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Current CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(url: string) {
    const r = await context.request.get(url, { timeout: 190000 });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const value = await get(root() + '/operations/' + id);
          if (['failed', 'cancelled'].includes(value.state))
            throw Error('Retain original Operation ' + id + ' ' + value.error_code);
          return value.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function mutate(
    label: string,
    url: string,
    data: unknown,
    version?: string,
    method = 'POST',
  ) {
    if (state.responses[label]) {
      const prior = state.responses[label];
      if (prior.operation) await observe(prior.operation.id);
      return prior;
    }
    if (!state.requests[label]) {
      state.requests[label] = {
        url,
        data,
        version,
        method,
        key: 'c02-platform-' + state.nonce + '-' + label,
      };
      await save();
    }
    const original = state.requests[label];
    const r = await context.request.fetch(original.url, {
      method: original.method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': original.key,
        ...(original.version ? { 'If-Match': '"' + original.version + '"' } : {}),
      },
      data: original.data,
      timeout: 190000,
    });
    expect([200, 201, 202]).toContain(r.status());
    state.responses[label] = await r.json();
    if (label === 'create-project' || label === 'create-project-own-org')
      state.project = state.responses[label].resource.id;
    await save();
    if (state.responses[label].operation) await observe(state.responses[label].operation.id);
    return state.responses[label];
  }
  async function sql(
    statement: string,
    database = 'postgres',
    login = 'cloud_admin',
    password = state.password,
  ) {
    const r = await context.request.post(root() + '/endpoints/' + state.writer + '/query', {
      headers: await headers(),
      data: { sql: statement, database, role: login, password },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function onceSQL(
    label: string,
    statement: string,
    database = 'postgres',
    login = 'cloud_admin',
    password = state.password,
  ) {
    if (done(label)) return;
    await sql(statement, database, login, password);
    await save(label);
  }
  async function service(label: string, name: string, spec: unknown, disable = false) {
    const current = await get(base() + '/' + name);
    return mutate(
      label,
      base() + '/' + name,
      spec,
      String(current.generation),
      disable ? 'DELETE' : 'POST',
    );
  }
  async function stopFunction(label: string) {
    const current = await get(functions());
    if (current.state === 'suspended' && done(label)) return;
    await mutate(
      label,
      functions() + '/suspend',
      { acknowledge_background_work_stop: true },
      String(current.version),
    );
    await save(label);
  }
  async function invoke() {
    const r = await context.request.post(functions() + '/console-invoke', {
      headers: await headers(),
      data: { method: 'GET', path: '/', body: '' },
      timeout: 190000,
    });
    expect(r.status()).toBe(200);
    const value = await r.json();
    expect(value.status_code).toBe(200);
    return JSON.parse(Buffer.from(value.body, 'base64').toString('utf8'));
  }
  const authSpec = (database: string) => ({
    database,
    allowed_origins: [new URL(baseURL!).origin],
  });
  const dataSpec = (database: string) => ({
    database,
    schema: 'app',
    issuer: 'https://catalog.identity.example.test',
    audience: state.branch,
    jwks: { keys: [state.provider] },
    allowed_origins: ['https://app.example.test'],
  });
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!state.org) {
      const name = 'ci-catalog-platform-' + state.nonce;
      let matches = (await get('/api/v1/organizations')).items.filter((v: any) => v.name === name);
      if (matches.length === 0 && !state.organizationRequested) {
        state.organizationRequested = true;
        await save();
        const response = await context.request.post('/api/v1/organizations', {
          headers: await headers(),
          data: { name },
        });
        expect(response.status()).toBe(201);
        state.org = (await response.json()).id;
        await save();
      } else {
        if (matches.length === 0) {
          await expect
            .poll(
              async () => {
                matches = (await get('/api/v1/organizations')).items.filter(
                  (v: any) => v.name === name,
                );
                return matches.length;
              },
              { timeout: 30000, intervals: [2000] },
            )
            .toBe(1);
        }
        expect(matches).toHaveLength(1);
        state.org = matches[0].id;
        await save();
      }
    }
    await page.reload();
    await page.getByLabel('切换组织', { exact: true }).click();
    await page
      .getByRole('option', { name: 'ci-catalog-platform-' + state.nonce + ' · admin', exact: true })
      .click();
    if (!state.project) {
      const accepted = await mutate(
        'create-project-own-org',
        '/api/v1/organizations/' + state.org + '/projects',
        {
          name: 'ci-catalog-platform-' + state.nonce,
          region_id: 'rke2-lab',
          postgres_version: 16,
          password: state.password,
          autoscaling: {
            min_cpu_milli: 1000,
            max_cpu_milli: 1000,
            min_memory_mib: 1024,
            max_memory_mib: 1024,
          },
        },
      );
      state.project = accepted.resource.id;
      await save();
      await observe(accepted.operation.id);
    }
    if (!state.writer) {
      const e = (await get(root() + '/endpoints')).items;
      expect(e).toHaveLength(1);
      state.writer = e[0].id;
      state.branch = e[0].branch_id;
      await save();
    }
    await mutate('create-owner', base() + '/roles', {
      name: role,
      password: state.password + '-owner',
    });
    await mutate('create-database', base() + '/databases', { name: oldDB, owner: role });
    if (!done('owned_native_schema_and_explicit_grants')) {
      await onceSQL('probe-createrole', 'ALTER ROLE control_probe CREATEROLE');
      for (const [i, statement] of [
        'GRANT CREATE, CONNECT ON DATABASE ' + oldDB + ' TO control_probe WITH GRANT OPTION',
        'REVOKE TEMP ON DATABASE ' + oldDB + ' FROM PUBLIC',
        'REVOKE ALL ON SCHEMA public FROM PUBLIC',
        'CREATE SCHEMA IF NOT EXISTS app',
        'CREATE TABLE IF NOT EXISTS app.function_probe(id bigserial PRIMARY KEY,marker text NOT NULL)',
        'GRANT USAGE ON SCHEMA app TO control_probe WITH GRANT OPTION',
        'GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA app TO control_probe WITH GRANT OPTION',
        'GRANT USAGE,SELECT,UPDATE ON ALL SEQUENCES IN SCHEMA app TO control_probe WITH GRANT OPTION',
        "INSERT INTO app.function_probe(marker) SELECT 'original-canary' WHERE NOT EXISTS(SELECT 1 FROM app.function_probe WHERE marker='original-canary')",
      ].entries())
        await onceSQL('grant-' + i, statement, oldDB, role, state.password + '-owner');
      state.databaseOIDs = (
        await sql("SELECT oid::text FROM pg_database WHERE datname='" + oldDB + "'")
      ).rows;
      await save('owned_native_schema_and_explicit_grants');
    }
    if (
      !done('backend-rls-prepared') &&
      !done('dormant_backend_ui_rename_and_same_operation_replay')
    ) {
      await onceSQL(
        'canary-enable-rls',
        'ALTER TABLE app.function_probe ENABLE ROW LEVEL SECURITY',
        oldDB,
        role,
        state.password + '-owner',
      );
      await onceSQL(
        'canary-force-rls',
        'ALTER TABLE app.function_probe FORCE ROW LEVEL SECURITY',
        oldDB,
        role,
        state.password + '-owner',
      );
      await onceSQL(
        'canary-rls-policy',
        'CREATE POLICY catalog_canary_rows ON app.function_probe USING (true) WITH CHECK (true)',
        oldDB,
        role,
        state.password + '-owner',
      );
      await save('backend-rls-prepared');
    }
    if (!done('platform_original_record_ui_and_lost_response')) {
      await page.goto('/#/projects/' + state.project + '/databases');
      const panel = page.getByRole('region', { name: '平台与默认目录身份' });
      await panel.getByRole('button', { name: '核对平台与默认身份' }).click();
      await expect(
        panel.getByText('尚无这一原生时间线的平台身份记录。首次记录仅证明当前观察时点。'),
      ).toBeVisible();
      let injected = false;
      let original: any;
      const pattern = '**/catalog-platform-identities';
      await page.route(pattern, async (route) => {
        if (route.request().method() === 'POST' && !injected) {
          injected = true;
          original = {
            data: route.request().postDataJSON(),
            key: route.request().headers()['idempotency-key'],
          };
          state.requests['platform-record-original'] = original;
          await save();
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          state.baselineID = (await response.json()).baseline_id;
          await save();
          await route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({
              code: 'controlled_observation_loss',
              message: 'Recover original request',
            }),
          });
        } else await route.continue();
      });
      await panel.getByLabel('确认从当前观察记录平台身份').check();
      await panel.getByRole('button', { name: '记录当前平台原身份', exact: true }).click();
      await expect(panel.getByRole('button', { name: '恢复原平台身份记录' })).toBeVisible();
      await panel.getByRole('button', { name: '恢复原平台身份记录' }).click();
      await expect(
        panel.getByText('当前平台原身份已记录。后续观察保留同一组原生 OID。'),
      ).toBeVisible();
      await page.unroute(pattern);
      const receipt = await context.request.post(base() + '/catalog-platform-identities', {
        headers: { ...(await headers()), 'Idempotency-Key': original.key },
        data: original.data,
      });
      expect(receipt.status()).toBe(200);
      expect((await receipt.json()).baseline_id).toBe(state.baselineID);
      await save('platform_original_record_ui_and_lost_response');
    }
    if (!done('platform_role_options_drift_detected_and_restored')) {
      await onceSQL('platform-role-option-away', 'ALTER ROLE control_probe NOCREATEROLE');
      const observed = await get(base() + '/catalog-platform-identities');
      const original = observed.items.find(
        (v: any) => v.kind === 'role' && v.expected_name === 'control_probe',
      );
      expect(original.status).toBe('role_options_changed');
      expect(original.observed_oid).toBe(original.expected_oid);
      expect(observed.baseline_id).toBe(state.baselineID);
      await onceSQL('restore-platform-role-option', 'ALTER ROLE control_probe CREATEROLE');
      const restored = await get(base() + '/catalog-platform-identities');
      expect(restored.items.every((v: any) => v.status === 'catalog_matches')).toBe(true);
      state.deferredChecks = [
        {
          name: 'default_database_external_rename_maintenance_qualification',
          status: 'deferred',
          reason:
            'SQLSTATE 55006: owned default postgres is held by compute_monitor, vm-monitor and pg_cron; requires a dedicated native maintenance window. Original OID comparison logic has separate focused checks.',
        },
      ];
      await save('platform_role_options_drift_detected_and_restored');
    }
    if (!done('native_backends_initialized_and_test_rows_retained')) {
      await service('auth-enable-original', 'auth', authSpec(oldDB));
      if (!state.appUser) {
        const signup = await context.request.post('/auth/v1/' + state.branch + '/sign-up/email', {
          headers: { Origin: new URL(baseURL).origin },
          data: {
            email: 'canary-' + state.nonce + '@example.test',
            password: state.password + '-app',
            name: 'Catalog canary',
          },
        });
        expect(signup.status()).toBe(200);
        state.appUser = (await signup.json()).user.id;
        await save();
      }
      await service('data-enable-original', 'data-api', dataSpec(oldDB));
      await service('storage-enable-original', 'storage', { database: oldDB });
      await mutate('owned-storage-marker', base() + '/storage/buckets', {
        name: 'catalog-retained',
        access: 'private',
      });
      if (!done('tiny-object-created')) {
        const response = await context.request.put(
          base() + '/storage/buckets/catalog-retained/objects?key=original.txt',
          {
            headers: { ...(await headers()), 'Content-Type': 'text/plain', 'If-None-Match': '*' },
            data: Buffer.from('original-catalog-canary'),
            timeout: 190000,
          },
        );
        expect([200, 201]).toContain(response.status());
        await save('tiny-object-created');
      }
      await page.goto('/#/projects/' + state.project + '/functions');
      if (!state.functionID) {
        await page.getByLabel('函数名称', { exact: true }).fill('canary');
        await page.getByLabel('函数数据库', { exact: true }).fill(oldDB);
        await page.getByLabel('授权 Schema', { exact: true }).fill('app');
        const pending = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname === base() + '/functions' && r.request().method() === 'POST',
        );
        await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
        const response = await pending;
        expect(response.status()).toBe(202);
        state.functionID = (await response.json()).function_id;
        await save();
      }
      if (!done('function-deployed')) {
        await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
        const pending = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname === functions() + '/deployments' &&
            r.request().method() === 'POST',
        );
        await page.getByRole('button', { name: '部署函数版本', exact: true }).click();
        const response = await pending;
        expect(response.status()).toBe(202);
        state.responses['function-deploy'] = await response.json();
        await save();
        await observe(state.responses['function-deploy'].operation.id);
        await save('function-deployed');
      }
      const original = await invoke();
      expect(original.database).toBe(oldDB);
      expect(original.rows).toBe(1);
      await save('native_backends_initialized_and_test_rows_retained');
    }
    if (!done('active_backends_reject_migration')) {
      const original = (await get(base() + '/databases')).items.find((v: any) => v.name === oldDB);
      const denied = await context.request.post(base() + '/databases/' + oldDB + '/rename', {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'c02-active-' + state.nonce,
          'If-Match': '"' + original.version_token + '"',
        },
        data: { new_name: newDB, migrate_backend_dependencies: true },
      });
      expect(denied.status()).toBe(409);
      expect((await denied.json()).code).toBe('catalog_backend_quiescence_required');
      await save('active_backends_reject_migration');
    }
    if (!done('dormant_backend_ui_rename_and_same_operation_replay')) {
      await stopFunction('function-stop-before-rename');
      for (const name of ['auth', 'data-api', 'storage'])
        await service('disable-before-' + name, name, {}, true);
      if (!state.responses['backend-rename']) {
        await page.goto('/#/projects/' + state.project + '/databases');
        const panel = page.getByRole('region', { name: '目录重命名' });
        await panel.getByLabel('重命名原对象').selectOption(oldDB);
        await panel.getByLabel('重命名新名称').fill(newDB);
        await panel.getByLabel('确认更新应用连接名称').check();
        await panel.getByLabel('同步迁移已停用 Backend 服务的数据库名称').check();
        const pending = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname === base() + '/databases/' + oldDB + '/rename' &&
            r.request().method() === 'POST',
        );
        await panel.getByRole('button', { name: '提交重命名', exact: true }).click();
        const response = await pending;
        state.requests['backend-rename'] = {
          url: base() + '/databases/' + oldDB + '/rename',
          data: response.request().postDataJSON(),
          headers: response.request().headers(),
        };
        const accepted = await response.json();
        if (response.status() === 202) state.responses['backend-rename'] = accepted;
        await save();
        expect(response.status()).toBe(202);
      }
      await observe(state.responses['backend-rename'].operation.id);
      const original = state.requests['backend-rename'];
      const replay = await context.request.post(original.url, {
        headers: {
          ...(await headers()),
          'Idempotency-Key': original.headers['idempotency-key'],
          'If-Match': original.headers['if-match'],
        },
        data: original.data,
      });
      expect(replay.status()).toBe(202);
      expect((await replay.json()).operation.id).toBe(
        state.responses['backend-rename'].operation.id,
      );
      await save('dormant_backend_ui_rename_and_same_operation_replay');
    }
    if (!done('original_oid_backend_rows_and_current_signed_sql_scope')) {
      expect(
        (await sql("SELECT oid::text FROM pg_database WHERE datname='" + newDB + "'")).rows,
      ).toEqual(state.databaseOIDs);
      for (const name of ['auth', 'data-api', 'storage']) {
        const value = await get(base() + '/' + name);
        expect(value.spec.database).toBe(newDB);
        expect(value.state).toBe('disabled');
      }
      const current = await get(functions());
      expect(current.database_name).toBe(newDB);
      expect(current.state).toBe('suspended');
      expect(
        (
          await sql(
            'SELECT marker FROM app.function_probe ORDER BY id',
            newDB,
            role,
            state.password + '-owner',
          )
        ).rows,
      ).toEqual([['original-canary']]);
      expect(
        (await sql('SELECT id FROM neon_auth."user" WHERE id=\'' + state.appUser + "'", newDB))
          .rows,
      ).toEqual([[state.appUser]]);
      expect(
        (await sql("SELECT name FROM neon_storage.buckets WHERE name='catalog-retained'", newDB))
          .rows,
      ).toEqual([['catalog-retained']]);
      await save('original_oid_backend_rows_and_current_signed_sql_scope');
    }
    if (!done('backend_reenable_preserves_auth_and_small_object')) {
      await service('auth-enable-renamed', 'auth', authSpec(newDB));
      const login = await context.request.post('/auth/v1/' + state.branch + '/sign-in/email', {
        headers: { Origin: new URL(baseURL).origin },
        data: {
          email: 'canary-' + state.nonce + '@example.test',
          password: state.password + '-app',
        },
      });
      expect(login.status()).toBe(200);
      expect((await login.json()).user.id).toBe(state.appUser);
      await service('data-enable-renamed', 'data-api', dataSpec(newDB));
      await service('storage-enable-renamed', 'storage', { database: newDB });
      const original = await context.request.get(
        base() + '/storage/buckets/catalog-retained/objects?key=original.txt',
        { timeout: 190000 },
      );
      expect(original.status()).toBe(200);
      expect(await original.text()).toBe('original-catalog-canary');
      for (const name of ['auth', 'data-api', 'storage'])
        await service('disable-final-' + name, name, {}, true);
      await save('backend_reenable_preserves_auth_and_small_object');
    }
    if (!done('function_resumes_original_bundle_on_new_sql_generation')) {
      const current = await get(functions());
      await mutate('resume-renamed', functions() + '/resume', {}, String(current.version));
      const invoked = await invoke();
      expect(invoked.database).toBe(newDB);
      expect(invoked.rows).toBe(1);
      await save('function_resumes_original_bundle_on_new_sql_generation');
    }
    if (!done('retired_canary_preserves_data_and_original_receipts')) {
      await stopFunction('function-stop-final');
      await mutate('suspend-writer', root() + '/endpoints/' + state.writer + '/suspend', {});
      await save('retired_canary_preserves_data_and_original_receipts');
    }
    result = 'pass';
    await save();
  } finally {
    await save();
  }
});
