import { expect, test, type BrowserContext } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import path from 'node:path';

test('Application organizations: UI membership, invitations, roles and physical branch isolation', async ({
  page,
  context,
  browser,
  baseURL,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile || baseURL !== 'http://192.168.146.100:30788')
    throw Error('Protected exact lab fixture required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    f.project_id !== 'prj_6f3c51bd15b772e3' ||
    f.branch_id !== 'br_39167d49b9123c61' ||
    f.database !== 'owner_transfer_v1'
  )
    throw Error('Retained source fixture required');
  const source = JSON.parse(await readFile(f.source_credentials_file, 'utf8'));
  const state: any = f.resume_file
    ? JSON.parse(await readFile(f.resume_file, 'utf8'))
    : {
        project_id: f.project_id,
        source_branch_id: f.branch_id,
        nonce: String(Date.now()),
        checks: [],
        running: '',
        operations: {},
        requests: {},
        password: source.password,
        member_password: randomBytes(30).toString('base64url'),
        managed_password: randomBytes(30).toString('base64url'),
        platform_password: randomBytes(30).toString('base64url'),
        users: { admin: source.user },
      };
  if (state.project_id !== f.project_id || state.source_branch_id !== f.branch_id || state.running)
    throw Error('Inspect original pending mutation before resuming');
  const root = '/api/v1/projects/' + f.project_id,
    authPath = () => root + '/branches/' + state.branch_id + '/auth',
    appPath = () => '/auth/v1/' + state.branch_id;
  let result = 'running',
    lastApp = 0;
  const extra: BrowserContext[] = [];
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  async function save() {
    await writeFile(
      path.join(privateDir!, 'auth-org-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks: state.checks,
        project_id: state.project_id,
        branch_id: state.branch_id,
        writer_id: state.writer_id,
        operations: state.operations,
        root_auth_unchanged: state.root_auth_unchanged || false,
        credentials_exported: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function step(name: string, work: () => Promise<void>) {
    if (state.checks.includes(name)) return;
    state.running = name;
    await save();
    await work();
    state.checks.push(name);
    state.running = '';
    await save();
  }
  async function headers() {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(url: string) {
    const response = await context.request.get(url, { timeout: 190000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Original operation failed: ' + id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function mutate(
    url: string,
    label: string,
    data?: object,
    version?: number,
    method = 'POST',
  ) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return get(root + '/operations/' + state.operations[label]);
    }
    state.requests[label] ??= {
      url,
      method,
      data,
      version,
      key: 'a03-org-' + state.nonce + '-' + label,
    };
    await save();
    const request = state.requests[label];
    const response = await context.request.fetch(request.url, {
      method: request.method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': request.key,
        ...(request.version === undefined ? {} : { 'If-Match': `"${request.version}"` }),
      },
      ...(request.data === undefined ? {} : { data: request.data }),
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await observe(value.operation.id);
    return get(root + '/operations/' + value.operation.id);
  }
  async function visit() {
    await page.goto('/?auth_branch=' + state.branch_id + '#/projects/' + f.project_id + '/auth');
    await expect(page.getByLabel('Auth 分支', { exact: true })).toHaveValue(state.branch_id);
  }
  async function enableUI(label: string) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      await visit();
      return;
    }
    await visit();
    await page.getByRole('button', { name: '刷新 Auth 状态', exact: true }).click();
    await expect(page.getByRole('button', { name: '启用 Auth', exact: true })).toBeEnabled();
    await page.getByLabel('Auth 数据库', { exact: true }).fill(f.database);
    await page.getByLabel('启用应用 TOTP 双重认证', { exact: true }).check();
    await page.getByLabel('迁移父分支双重认证', { exact: true }).check();
    await page.getByLabel('启用应用管理员接口', { exact: true }).check();
    await expect(page.getByLabel('应用管理员授权用户 ID', { exact: true })).toHaveValue('');
    await page.getByLabel('启用应用组织管理', { exact: true }).check();
    await page.getByLabel('应用组织用户配额', { exact: true }).fill('2');
    await page.getByLabel('应用组织成员配额', { exact: true }).fill('3');
    await page.getByLabel('应用组织创建者角色', { exact: true }).selectOption('owner');
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === authPath() && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(202);
    state.operations[label] = (await response.json()).operation.id;
    await save();
    await observe(state.operations[label]);
    await expect(page.getByRole('button', { name: '禁用 Auth', exact: true })).toBeEnabled();
  }
  // UI writes also refresh a list. Stay inside the existing 30/minute limiter.
  async function pace() {
    const delay = lastApp + 4250 - Date.now();
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    lastApp = Date.now();
  }
  async function app(action: string, data?: object, client = context) {
    await pace();
    if (action === 'sign-in/email') {
      // The maintained library resets this special bucket after ten idle
      // seconds. Keep its limit; never retry an already committed admin write.
      if (Date.now() - (state.last_login_at || 0) >= 10500) state.login_burst_count = 0;
      if ((state.login_burst_count || 0) >= 3) {
        const delay = (state.last_login_at || 0) + 10500 - Date.now();
        if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
        state.login_burst_count = 0;
      }
      state.last_login_at = Date.now();
      state.login_burst_count = (state.login_burst_count || 0) + 1;
      await save();
      lastApp = Date.now();
    }
    const response = await client.request.fetch(appPath() + '/' + action, {
      method: data === undefined ? 'GET' : 'POST',
      headers: { Origin: baseURL! },
      ...(data === undefined ? {} : { data }),
      timeout: 190000,
    });
    const value = await response.json().catch(() => null);
    return { status: response.status(), value };
  }
  async function signIn(client = context, kind = 'admin') {
    const user = state.users[kind],
      password = kind === 'admin' ? state.password : state.member_password;
    expect((await app('sign-in/email', { email: user.email, password }, client)).status).toBe(200);
  }
  function panel() {
    return page.getByRole('region', { name: '应用组织', exact: true });
  }
  async function press(name: string, action: string) {
    await pace();
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === appPath() + '/organization/' + action,
    );
    await panel().getByRole('button', { name, exact: true }).click();
    return waiting;
  }
  async function shot(name: string) {
    await page.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  async function switchUser(kind: string) {
    await app('sign-out', {});
    await signIn(context, kind);
    await visit();
  }
  async function selectOrganization() {
    expect((await press('读取我的应用组织', 'list')).status()).toBe(200);
    await panel().getByLabel('当前应用组织', { exact: true }).selectOption(state.organization_id);
  }
  async function disabledAndSuspended(label: string) {
    const auth = await get(authPath());
    if (auth.state !== 'disabled')
      await mutate(authPath(), label + '-disable', undefined, auth.generation, 'DELETE');
    await mutate(root + '/endpoints/' + state.writer_id + '/suspend', label + '-suspend', {});
    await expect
      .poll(async () => (await get(root + '/endpoints/' + state.writer_id)).observed_state, {
        timeout: 180000,
        intervals: [2000, 5000],
      })
      .toBe('suspended');
  }
  try {
    await save();
    page.setDefaultTimeout(30000);
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (
      state.checks.includes('UI_organization_policy_and_copied_application_admin') &&
      !state.checks.includes(
        'UI_delete_only_child_organization_preserves_parent_pending_invitation',
      )
    ) {
      await signIn();
      await visit();
      if (state.checks.includes('physical_child_invalidates_copied_pending_invitation'))
        await selectOrganization();
    }
    await step('one_owned_parent_branch_preserves_retained_sources', async () => {
      state.original_source_auth = await get(root + '/branches/' + f.branch_id + '/auth');
      state.original_root_auth = await get(root + '/branches/br_6f3c51bd15b772e3/auth');
      expect(state.original_source_auth.state).toBe('disabled');
      expect(state.original_source_auth.generation).toBe(6);
      const op = await mutate(root + '/branches', 'create-parent', {
        name: 'ci-auth-org-' + state.nonce,
        parent_branch_id: f.branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: {
          min_cpu_milli: 1000,
          max_cpu_milli: 1000,
          min_memory_mib: 1024,
          max_memory_mib: 1024,
        },
      });
      state.branch_id = op.resource_id;
      state.writer_id = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.branch_id && e.endpoint_type === 'read_write',
      )?.id;
      if (!state.writer_id) throw Error('Owned writer required');
      state.parent_branch_id = state.branch_id;
      state.parent_writer_id = state.writer_id;
    });
    await step('UI_organization_policy_and_copied_application_admin', async () => {
      await enableUI('enable-parent');
      await signIn();
      expect((await app('get-session')).value.user.id).toBe(state.users.admin.id);
      const spec = (await get(authPath())).spec;
      expect(spec.organization_enabled).toBe(true);
      expect(spec.organization_limit).toBe(2);
      expect(spec.organization_membership_limit).toBe(3);
      expect(spec.admin_bootstrap_user_id || '').toBe('');
      for (const kind of ['member', 'other']) {
        const email = 'a03-org-' + kind + '-' + state.nonce + '@example.test';
        const r = await app('admin/create-user', {
          name: 'Owned organization ' + kind,
          email,
          password: state.member_password,
          role: kind === 'other' ? 'admin' : 'user',
          data: { emailVerified: true },
        });
        expect(r.status).toBe(200);
        state.users[kind] = { id: r.value.user.id, email };
        await save();
      }
      await visit();
      await expect(panel()).toBeVisible();
      await panel()
        .getByLabel('应用组织名称', { exact: true })
        .fill('Owned organization ' + state.nonce);
      await panel()
        .getByLabel('应用组织标识', { exact: true })
        .fill('org-' + state.nonce);
      const created = await press('创建应用组织', 'create');
      expect(created.status()).toBe(200);
      state.organization_id = (await created.json()).id;
      await save();
      await expect(panel().getByLabel('当前应用组织', { exact: true })).toHaveValue(
        state.organization_id,
      );
      await shot('organization-policy');
    });
    await step('UI_owner_update_active_scope_and_last_owner_retention', async () => {
      await selectOrganization();
      await panel()
        .getByLabel('应用组织名称', { exact: true })
        .fill('Updated organization ' + state.nonce);
      expect((await press('更新组织名称', 'update')).status()).toBe(200);
      expect((await press('设为当前组织', 'set-active')).status()).toBe(200);
      const r = await app('token');
      expect(r.status).toBe(200);
      const claims = JSON.parse(Buffer.from(r.value.token.split('.')[1], 'base64url').toString());
      expect(claims.organization_id).toBe(state.organization_id);
      expect(claims.organization_role).toBe('owner');
      expect((await press('清除当前组织', 'set-active')).status()).toBe(200);
      const empty = await app('token');
      expect(
        JSON.parse(Buffer.from(empty.value.token.split('.')[1], 'base64url').toString())
          .organization_id,
      ).toBeUndefined();
      await panel().getByLabel('确认退出或删除应用组织', { exact: true }).check();
      expect((await press('退出应用组织', 'leave')).status()).toBeGreaterThanOrEqual(400);
      await panel().getByLabel('确认退出或删除应用组织', { exact: true }).uncheck();
      expect(
        (await app('organization/get-full-organization?organizationId=' + state.organization_id))
          .value.members,
      ).toHaveLength(1);
    });
    await step('UI_invitation_recipient_verification_acceptance_and_admin_isolation', async () => {
      await panel().getByLabel('组织邀请邮箱', { exact: true }).fill(state.users.member.email);
      const r = await press('邀请应用组织成员', 'invite-member');
      expect(r.status()).toBe(200);
      state.accepted_invitation_id = (await r.json()).id;
      await save();
      await switchUser('other');
      expect(
        (await app('organization/get-full-organization?organizationId=' + state.organization_id))
          .status,
      ).toBe(403);
      expect(
        (
          await app('organization/accept-invitation', {
            invitationId: state.accepted_invitation_id,
          })
        ).status,
      ).toBe(403);
      await switchUser('member');
      expect((await press('读取我收到的邀请', 'list-user-invitations')).status()).toBe(200);
      await expect(panel().getByRole('table', { name: '应用组织邀请' })).toContainText(
        state.accepted_invitation_id,
      );
      await panel()
        .getByLabel('应用组织邀请 ID', { exact: true })
        .fill(state.accepted_invitation_id);
      expect((await press('读取所选邀请', 'get-invitation')).status()).toBe(200);
      expect((await press('接受组织邀请', 'accept-invitation')).status()).toBe(200);
      await selectOrganization();
      await panel().getByLabel('应用组织名称', { exact: true }).fill('Forbidden update');
      expect((await press('更新组织名称', 'update')).status()).toBe(403);
      await switchUser('admin');
      await selectOrganization();
    });
    await step('UI_membership_role_change_removal_and_current_JWT_claims', async () => {
      expect((await press('读取组织成员', 'list-members')).status()).toBe(200);
      const full = await app(
        'organization/get-full-organization?organizationId=' + state.organization_id,
      );
      state.member_id = full.value.members.find((m: any) => m.userId === state.users.member.id).id;
      await save();
      await panel().getByLabel('目标组织成员', { exact: true }).selectOption(state.member_id);
      await panel().getByLabel('目标组织角色', { exact: true }).selectOption('admin');
      expect((await press('设置组织角色', 'update-member-role')).status()).toBe(200);
      const client = await browser.newContext({ baseURL });
      extra.push(client);
      await signIn(client, 'member');
      expect(
        (await app('organization/set-active', { organizationId: state.organization_id }, client))
          .status,
      ).toBe(200);
      const r = await app('token', undefined, client);
      expect(
        JSON.parse(Buffer.from(r.value.token.split('.')[1], 'base64url').toString())
          .organization_role,
      ).toBe('admin');
      await panel().getByLabel('目标组织成员', { exact: true }).selectOption(state.member_id);
      expect((await press('移除组织成员', 'remove-member')).status()).toBe(200);
      const removed = await app('token', undefined, client);
      expect(
        JSON.parse(Buffer.from(removed.value.token.split('.')[1], 'base64url').toString())
          .organization_id,
      ).toBeUndefined();
      expect(
        (
          await app(
            'organization/get-full-organization?organizationId=' + state.organization_id,
            undefined,
            client,
          )
        ).status,
      ).toBe(403);
    });
    await step('UI_cancel_reject_and_rejoin_with_one_pending_copy_fixture', async () => {
      await panel().getByLabel('目标组织角色', { exact: true }).selectOption('member');
      await panel().getByLabel('组织邀请邮箱', { exact: true }).fill(state.users.other.email);
      let r = await press('邀请应用组织成员', 'invite-member');
      expect(r.status()).toBe(200);
      state.canceled_invitation_id = (await r.json()).id;
      await save();
      expect((await press('撤销组织邀请', 'cancel-invitation')).status()).toBe(200);
      r = await press('邀请应用组织成员', 'invite-member');
      expect(r.status()).toBe(200);
      state.rejected_invitation_id = (await r.json()).id;
      await save();
      await switchUser('other');
      await panel()
        .getByLabel('应用组织邀请 ID', { exact: true })
        .fill(state.rejected_invitation_id);
      expect((await press('拒绝组织邀请', 'reject-invitation')).status()).toBe(200);
      await switchUser('admin');
      await selectOrganization();
      await panel().getByLabel('组织邀请邮箱', { exact: true }).fill(state.users.member.email);
      r = await press('邀请应用组织成员', 'invite-member');
      expect(r.status()).toBe(200);
      const join = (await r.json()).id;
      await switchUser('member');
      await panel().getByLabel('应用组织邀请 ID', { exact: true }).fill(join);
      expect((await press('接受组织邀请', 'accept-invitation')).status()).toBe(200);
      await switchUser('admin');
      await selectOrganization();
      await panel().getByLabel('组织邀请邮箱', { exact: true }).fill(state.users.other.email);
      r = await press('邀请应用组织成员', 'invite-member');
      expect(r.status()).toBe(200);
      state.pending_invitation_id = (await r.json()).id;
      await save();
      expect((await press('读取组织邀请', 'list-invitations')).status()).toBe(200);
      await expect(panel().getByRole('table', { name: '应用组织邀请' })).toContainText(
        state.pending_invitation_id,
      );
      await shot('organization-members-and-invitations');
      state.parent_auth_active = await get(authPath());
      await disabledAndSuspended('parent');
    });
    await step('physical_child_invalidates_copied_pending_invitation', async () => {
      const op = await mutate(root + '/branches', 'create-child', {
        name: 'ci-auth-org-child-' + state.nonce,
        parent_branch_id: state.parent_branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: {
          min_cpu_milli: 1000,
          max_cpu_milli: 1000,
          min_memory_mib: 1024,
          max_memory_mib: 1024,
        },
      });
      state.branch_id = op.resource_id;
      state.writer_id = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.branch_id && e.endpoint_type === 'read_write',
      )?.id;
      if (!state.writer_id) throw Error('Owned child writer required');
      state.child_branch_id = state.branch_id;
      state.child_writer_id = state.writer_id;
      await save();
      await enableUI('enable-child');
      await signIn();
      await visit();
      await selectOrganization();
      expect((await press('读取组织成员', 'list-members')).status()).toBe(200);
      const full = await app(
        'organization/get-full-organization?organizationId=' + state.organization_id,
      );
      expect(full.value.members.map((m: any) => m.userId).sort()).toEqual(
        [state.users.admin.id, state.users.member.id].sort(),
      );
      const invites = await app(
        'organization/list-invitations?organizationId=' + state.organization_id,
      );
      expect(invites.value.find((i: any) => i.id === state.pending_invitation_id).status).toBe(
        'canceled',
      );
      const recipient = await browser.newContext({ baseURL });
      extra.push(recipient);
      await signIn(recipient, 'other');
      expect(
        (
          await app(
            'organization/accept-invitation',
            { invitationId: state.pending_invitation_id },
            recipient,
          )
        ).status,
      ).toBe(400);
      await shot('copied-organization-isolation');
    });
    await step(
      'UI_delete_only_child_organization_preserves_parent_pending_invitation',
      async () => {
        await panel().getByLabel('确认退出或删除应用组织', { exact: true }).check();
        expect((await press('删除应用组织', 'delete')).status()).toBe(200);
        expect((await app('organization/list')).value).toHaveLength(0);
        await disabledAndSuspended('child');
        state.branch_id = state.parent_branch_id;
        state.writer_id = state.parent_writer_id;
        const saved = await get(authPath());
        await mutate(authPath(), 'reenable-parent', saved.spec, saved.generation);
        await signIn();
        const invites = await app(
          'organization/list-invitations?organizationId=' + state.organization_id,
        );
        expect(invites.value.find((i: any) => i.id === state.pending_invitation_id).status).toBe(
          'pending',
        );
        expect(
          (await app('organization/get-full-organization?organizationId=' + state.organization_id))
            .value.members,
        ).toHaveLength(2);
        state.parent_native_preserved = true;
        await save();
        await disabledAndSuspended('parent-final');
      },
    );
    await step(
      'owned_Auth_instances_disabled_writers_zero_original_sources_preserved',
      async () => {
        for (const id of [state.parent_branch_id, state.child_branch_id])
          expect((await get(root + '/branches/' + id + '/auth')).state).toBe('disabled');
        for (const [id, original] of [
          [f.branch_id, state.original_source_auth],
          ['br_6f3c51bd15b772e3', state.original_root_auth],
        ]) {
          const current = await get(root + '/branches/' + id + '/auth');
          expect({
            generation: current.generation,
            state: current.state,
            spec: current.spec,
          }).toEqual({
            generation: original.generation,
            state: original.state,
            spec: original.spec,
          });
        }
        state.root_auth_unchanged = true;
      },
    );
    result = 'pass';
    await save();
  } finally {
    await save();
    for (const client of extra) await client.close();
  }
});
