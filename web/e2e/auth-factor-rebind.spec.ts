import { expect, test } from '@playwright/test';
import { createHmac, randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

function totp(uri: string) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const secret = new URL(uri).searchParams.get('secret')!;
  let bits = 0,
    value = 0;
  const bytes: number[] = [];
  for (const ch of secret.toUpperCase().replace(/=/g, '')) {
    const n = alphabet.indexOf(ch);
    if (n < 0) throw Error('Invalid private enrollment URI');
    value = (value << 5) | n;
    bits += 5;
    if (bits >= 8) {
      bytes.push((value >> (bits - 8)) & 255);
      bits -= 8;
    }
  }
  const message = Buffer.alloc(8);
  message.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const digest = createHmac('sha1', Buffer.from(bytes)).update(message).digest();
  return String((digest.readUInt32BE(digest[19] & 15) & 0x7fffffff) % 1000000).padStart(6, '0');
}

test('Physical branch TOTP migration keeps parent data and requires fresh child MFA with independent recovery', async ({
  page,
  context,
  baseURL,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile || !baseURL)
    throw Error('Protected Linux factor fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const project = fixture.project_id;
  const root = '/api/v1/projects/' + project;
  const state: any = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : {
        project_id: project,
        source_branch_id: fixture.branch_id,
        nonce: String(Date.now()),
        checks: [],
        operations: {},
        versions: {},
        password: randomBytes(24).toString('base64url'),
        platform_password: randomBytes(24).toString('base64url'),
        users: {},
      };
  if (state.project_id !== project || state.source_branch_id !== fixture.branch_id)
    throw Error('Original factor acceptance scope changed');
  let result = 'running';
  const done = (name: string) => state.checks.includes(name);
  const authPath = (branch: string) => root + '/branches/' + branch + '/auth';
  const appPath = (branch: string) => '/auth/v1/' + branch;
  const lastAppRequest = new Map<string, number>();
  const spec = {
    database: fixture.database,
    allowed_origins: [],
    two_factor_enabled: true,
    inherit_two_factor: true,
  };
  const bounds = {
    min_cpu_milli: 1000,
    max_cpu_milli: 1000,
    min_memory_mib: 1024,
    max_memory_mib: 1024,
  };
  async function save(check?: string) {
    if (check && !done(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'auth-factor-checkpoint.private.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        checks: state.checks,
        operations: state.operations,
        parent_branch_id: state.parent_branch_id,
        child_branch_id: state.child_branch_id,
        parent_writer_id: state.parent_writer_id,
        child_writer_id: state.child_writer_id,
        migration: state.migration,
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
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Original factor operation failed ' + id + ' ' + op.error_code);
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
    version?: number,
  ) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return {
        operation: { id: state.operations[label] },
        resource: { id: (await get(root + '/operations/' + state.operations[label])).resource_id },
      };
    }
    if (version !== undefined && state.versions[label] === undefined) {
      state.versions[label] = version;
      await save();
    }
    const response = await context.request.fetch(target, {
      method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': 'a04-' + state.nonce + '-' + label,
        ...(state.versions[label] === undefined
          ? {}
          : { 'If-Match': '"' + state.versions[label] + '"' }),
      },
      ...(data === undefined ? {} : { data }),
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await observe(value.operation.id);
    return value;
  }
  async function visit(branch: string) {
    await page.goto('/?auth_branch=' + branch + '#/projects/' + project + '/auth');
    await expect(page.getByLabel('Auth 分支', { exact: true })).toHaveValue(branch);
  }
  async function enable(branch: string, label: string) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      await visit(branch);
      return;
    }
    await visit(branch);
    await page.getByLabel('Auth 数据库', { exact: true }).fill(fixture.database);
    await page.getByLabel('启用应用 TOTP 双重认证', { exact: true }).check();
    await page.getByLabel('迁移父分支双重认证', { exact: true }).check();
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === authPath(branch) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await observe(value.operation.id);
    await expect(page.getByTestId('auth-factor-migration')).toBeVisible();
  }
  async function disable(branch: string, label: string) {
    await mutate(
      'DELETE',
      authPath(branch),
      label,
      undefined,
      (await get(authPath(branch))).generation,
    );
  }
  async function app(branch: string, action: string, data?: unknown) {
    // Keep independent functional scenarios inside the runtime's existing
    // per-client limits. Do not weaken the production limiter or retry failures.
    const wait = (lastAppRequest.get(branch) ?? 0) + 2250 - Date.now();
    if (wait > 0) await new Promise((resolve) => setTimeout(resolve, wait));
    lastAppRequest.set(branch, Date.now());
    const response = await context.request.fetch(appPath(branch) + '/' + action, {
      method: data === undefined ? 'GET' : 'POST',
      headers: { Origin: new URL(baseURL!).origin },
      ...(data === undefined ? {} : { data }),
      timeout: 190000,
    });
    return { status: response.status(), value: await response.json() };
  }
  async function sql(endpoint: string, statement: string) {
    const response = await context.request.post(root + '/endpoints/' + endpoint + '/query', {
      headers: await headers(),
      data: {
        role: 'cloud_admin',
        database: fixture.database,
        password: state.platform_password,
        sql: statement,
      },
      timeout: 190000,
    });
    expect(response.status()).toBe(200);
    return response.json();
  }
  const literal = (v: string) => "'" + v.replaceAll("'", "''") + "'";
  async function nativeHash(endpoint: string) {
    const ids = Object.values(state.users)
      .map((u: any) => literal(u.id))
      .join(',');
    const statement = `SELECT md5(COALESCE((SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM neon_auth."user" u WHERE id IN (${ids})),'')),md5(COALESCE((SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM neon_auth.account a WHERE "userId" IN (${ids})),'')),md5(COALESCE((SELECT jsonb_agg(to_jsonb(f) ORDER BY id)::text FROM neon_auth."twoFactor" f WHERE "userId" IN (${ids})),''))`;
    return (await sql(endpoint, statement)).rows[0];
  }
  async function passwordLogin(branch: string) {
    const value = await app(branch, 'sign-in/email', {
      email: state.users.confirmed.email,
      password: state.password,
    });
    expect(value.status).toBe(200);
    expect(value.value.twoFactorRedirect).toBe(true);
    expect((await app(branch, 'get-session')).value).toBeNull();
    expect((await app(branch, 'token')).status).toBeGreaterThanOrEqual(400);
  }
  async function signup(branch: string, kind: string) {
    if (state.users[kind]) {
      expect(
        (
          await app(branch, 'sign-in/email', {
            email: state.users[kind].email,
            password: state.password,
          })
        ).status,
      ).toBe(200);
      return;
    }
    const email = 'a04-' + kind + '-' + state.nonce + '@example.test';
    const value = await app(branch, 'sign-up/email', {
      email,
      password: state.password,
      name: 'Branch MFA ' + kind,
    });
    expect(value.status).toBe(200);
    state.users[kind] = { id: value.value.user.id, email };
    await save();
  }
  async function zero(endpoint: string) {
    if ((await get(root + '/endpoints/' + endpoint)).observed_state !== 'suspended') {
      await mutate('POST', root + '/endpoints/' + endpoint + '/suspend', 'zero-' + endpoint, {});
      await expect
        .poll(async () => (await get(root + '/endpoints/' + endpoint)).observed_state, {
          timeout: 150000,
          intervals: [2000, 5000],
        })
        .toBe('suspended');
    }
  }
  try {
    await save();
    page.setDefaultTimeout(30000);
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!done('root_inheritance_rejected_before_operation')) {
      const original = await get(authPath(fixture.branch_id));
      const denied = await context.request.post(authPath(fixture.branch_id), {
        headers: {
          ...(await headers()),
          'Idempotency-Key': 'a04-' + state.nonce + '-root-reject',
          'If-Match': '"' + original.generation + '"',
        },
        data: spec,
      });
      expect(denied.status()).toBe(422);
      expect((await denied.json()).code).toBe('auth_factor_parent_required');
      expect((await get(authPath(fixture.branch_id))).generation).toBe(original.generation);
      state.original_source_auth = {
        generation: original.generation,
        state: original.state,
        spec: original.spec,
      };
      await save('root_inheritance_rejected_before_operation');
    }
    if (!state.parent_branch_id) {
      const value = await mutate('POST', root + '/branches', 'create-parent', {
        name: 'ci-auth-factor-parent-' + state.nonce,
        parent_branch_id: fixture.branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: bounds,
      });
      state.parent_branch_id = value.resource.id;
      await save();
    }
    const parent = state.parent_branch_id;
    state.parent_writer_id = (await get(root + '/endpoints')).items.find(
      (e: any) => e.branch_id === parent && e.endpoint_type === 'read_write',
    )?.id;
    if (!state.parent_writer_id) throw Error('Original parent writer required');
    await save();
    if (!done('physical_parent_enabled_with_explicit_migration')) {
      await enable(parent, 'enable-parent');
      await save('physical_parent_enabled_with_explicit_migration');
    }
    if (!done('parent_verified_pending_and_plain_accounts_persisted')) {
      await signup(parent, 'confirmed');
      if (!state.totp_uri) {
        const v = await app(parent, 'two-factor/enable', {
          password: state.password,
          method: 'totp',
        });
        expect(v.status).toBe(200);
        state.totp_uri = v.value.totpURI;
        state.parent_backups = v.value.backupCodes;
        await save();
      }
      expect(
        (
          await app(parent, 'two-factor/verify-totp', {
            code: totp(state.totp_uri),
            trustDevice: false,
          })
        ).status,
      ).toBe(200);
      await app(parent, 'sign-out', {});
      await signup(parent, 'pending');
      if (!state.pending_uri) {
        const v = await app(parent, 'two-factor/enable', {
          password: state.password,
          method: 'totp',
        });
        expect(v.status).toBe(200);
        state.pending_uri = v.value.totpURI;
        await save();
      }
      await app(parent, 'sign-out', {});
      await signup(parent, 'plain');
      await app(parent, 'sign-out', {});
      state.parent_hash = await nativeHash(state.parent_writer_id);
      await save('parent_verified_pending_and_plain_accounts_persisted');
    }
    if (!done('disabled_parent_physical_fork_preserves_source_history')) {
      await disable(parent, 'disable-parent-before-fork');
      if (!state.child_branch_id) {
        const v = await mutate('POST', root + '/branches', 'create-child', {
          name: 'ci-auth-factor-child-' + state.nonce,
          parent_branch_id: parent,
          password: state.platform_password,
          function_copy_policy: 'omit',
          autoscaling: bounds,
        });
        state.child_branch_id = v.resource.id;
        await save();
      }
      await save('disabled_parent_physical_fork_preserves_source_history');
    }
    const child = state.child_branch_id;
    state.child_writer_id = (await get(root + '/endpoints')).items.find(
      (e: any) => e.branch_id === child && e.endpoint_type === 'read_write',
    )?.id;
    if (!state.child_writer_id) throw Error('Original child writer required');
    await save();
    if (!done('ui_child_explicit_migration_and_durable_counts')) {
      await enable(child, 'enable-child');
      const current = await get(authPath(child));
      state.migration = current.factor_migration;
      expect(current.factor_migration.source_branch_id).toBe(parent);
      expect(current.factor_migration.migrated_factors).toBeGreaterThanOrEqual(1);
      expect(current.factor_migration.pending_enrollments_removed).toBeGreaterThanOrEqual(1);
      expect(current.factor_migration.recovery_codes_reset).toBe(true);
      await page.reload();
      await expect(page.getByTestId('auth-factor-migration')).toContainText('原恢复码已停用');
      await save('ui_child_explicit_migration_and_durable_counts');
    }
    if (!done('child_password_requires_fresh_totp_and_old_recovery_is_rejected')) {
      await passwordLogin(child);
      expect(
        (
          await app(child, 'two-factor/verify-backup-code', {
            code: state.parent_backups[0],
            trustDevice: false,
          })
        ).status,
      ).toBeGreaterThanOrEqual(400);
      await visit(child);
      await page.getByLabel('TOTP 验证码', { exact: true }).fill(totp(state.totp_uri));
      const waiting = page.waitForResponse(
        (r) => new URL(r.url()).pathname === appPath(child) + '/two-factor/verify-totp',
      );
      await page.getByRole('button', { name: '确认 TOTP 绑定或登录', exact: true }).click();
      expect((await waiting).status()).toBe(200);
      expect((await app(child, 'get-session')).value.user.id).toBe(state.users.confirmed.id);
      expect((await app(child, 'token')).status).toBe(200);
      await save('child_password_requires_fresh_totp_and_old_recovery_is_rejected');
    }
    if (!done('child_fresh_recovery_codes_single_use_and_parent_unchanged')) {
      if (!state.child_backups) {
        const v = await app(child, 'two-factor/generate-backup-codes', {
          password: state.password,
        });
        expect(v.status).toBe(200);
        state.child_backups = v.value.backupCodes;
        await save();
      }
      await app(child, 'sign-out', {});
      await passwordLogin(child);
      expect(
        (
          await app(child, 'two-factor/verify-backup-code', {
            code: state.child_backups[0],
            trustDevice: false,
          })
        ).status,
      ).toBe(200);
      await app(child, 'sign-out', {});
      await passwordLogin(child);
      expect(
        (
          await app(child, 'two-factor/verify-backup-code', {
            code: state.child_backups[0],
            trustDevice: false,
          })
        ).status,
      ).toBeGreaterThanOrEqual(400);
      expect(
        (
          await app(child, 'two-factor/verify-totp', {
            code: totp(state.totp_uri),
            trustDevice: false,
          })
        ).status,
      ).toBe(200);
      expect(await nativeHash(state.parent_writer_id)).toEqual(state.parent_hash);
      await save('child_fresh_recovery_codes_single_use_and_parent_unchanged');
    }
    if (!done('pending_child_binding_restarts_and_plain_account_keeps_password')) {
      await app(child, 'sign-out', {});
      let v = await app(child, 'sign-in/email', {
        email: state.users.pending.email,
        password: state.password,
      });
      expect(v.status).toBe(200);
      expect(v.value.twoFactorRedirect).not.toBe(true);
      const rows = (
        await sql(
          state.child_writer_id,
          'SELECT count(*)::text FROM neon_auth."twoFactor" WHERE "userId"=' +
            literal(state.users.pending.id),
        )
      ).rows;
      expect(rows[0][0]).toBe('0');
      await app(child, 'sign-out', {});
      v = await app(child, 'sign-in/email', {
        email: state.users.plain.email,
        password: state.password,
      });
      expect(v.status).toBe(200);
      expect(v.value.user.id).toBe(state.users.plain.id);
      await app(child, 'sign-out', {});
      await save('pending_child_binding_restarts_and_plain_account_keeps_password');
    }
    if (!done('parent_fresh_login_original_recovery_and_foreign_cookie_rejected')) {
      await enable(parent, 'reenable-parent');
      await passwordLogin(parent);
      expect(
        (
          await app(parent, 'two-factor/verify-totp', {
            code: totp(state.totp_uri),
            trustDevice: false,
          })
        ).status,
      ).toBe(200);
      const cookies = (await context.cookies()).filter((c) =>
        c.name.startsWith('neon_app_' + parent + '.'),
      );
      expect(cookies.length).toBeGreaterThan(0);
      const sourceLogin = 'control_auth_' + parent.replace(/^br_/, '');
      expect(
        (
          await sql(
            state.parent_writer_id,
            'SELECT rolcanlogin::text FROM pg_roles WHERE rolname=' + literal(sourceLogin),
          )
        ).rows[0][0],
      ).toBe('true');
      expect(
        (
          await sql(
            state.child_writer_id,
            'SELECT rolcanlogin::text FROM pg_roles WHERE rolname=' + literal(sourceLogin),
          )
        ).rows[0][0],
      ).toBe('false');
      const foreign = await context.request.get(appPath(child) + '/get-session', {
        headers: { Cookie: cookies.map((c) => c.name + '=' + c.value).join('; ') },
      });
      expect(await foreign.json()).toBeNull();
      await app(parent, 'sign-out', {});
      await passwordLogin(parent);
      expect(
        (
          await app(parent, 'two-factor/verify-backup-code', {
            code: state.parent_backups[0],
            trustDevice: false,
          })
        ).status,
      ).toBe(200);
      await visit(parent);
      await page.getByLabel('MFA 确认密码', { exact: true }).fill(state.password);
      const recovered = page.waitForResponse(
        (r) => new URL(r.url()).pathname === appPath(parent) + '/two-factor/get-totp-uri',
      );
      await page.getByRole('button', { name: '重新读取 TOTP 绑定地址', exact: true }).click();
      const recoveredResponse = await recovered;
      expect(recoveredResponse.status()).toBe(200);
      state.recovered_uri = (await recoveredResponse.json()).totpURI;
      expect(
        new URL(state.recovered_uri).searchParams.get('secret') ===
          new URL(state.totp_uri).searchParams.get('secret'),
      ).toBe(true);
      expect(await page.getByLabel('TOTP 绑定地址', { exact: true }).getAttribute('type')).toBe(
        'password',
      );
      await save();
      await app(parent, 'sign-out', {});
      await save('parent_fresh_login_original_recovery_and_foreign_cookie_rejected');
    }
    // The parent is no longer needed. Finish its existing cleanup first so the
    // remaining child checks keep only one test Compute running.
    if (!done('services_disabled_and_source_parent_child_computes_zero')) {
      await disable(parent, 'disable-parent-final');
      await zero(state.parent_writer_id);
    }
    if (!done('child_reenable_retains_original_migration_and_factor')) {
      await disable(child, 'disable-child');
      await enable(child, 'reenable-child');
      expect((await get(authPath(child))).factor_migration).toEqual(state.migration);
      await passwordLogin(child);
      expect(
        (
          await app(child, 'two-factor/verify-totp', {
            code: totp(state.recovered_uri),
            trustDevice: false,
          })
        ).status,
      ).toBe(200);
      await app(child, 'sign-out', {});
      await save('child_reenable_retains_original_migration_and_factor');
    }
    if (!done('services_disabled_and_source_parent_child_computes_zero')) {
      await disable(parent, 'disable-parent-final');
      await disable(child, 'disable-child-final');
      for (const endpoint of [state.parent_writer_id, state.child_writer_id, fixture.writer_id])
        await zero(endpoint);
      const original = await get(authPath(fixture.branch_id));
      expect({
        generation: original.generation,
        state: original.state,
        spec: original.spec,
      }).toEqual(state.original_source_auth);
      await save('services_disabled_and_source_parent_child_computes_zero');
    }
    result = 'pass';
    await save();
  } finally {
    await save();
  }
});
