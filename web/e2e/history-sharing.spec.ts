import { test, expect } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('UI explicitly shares one real SQL history across current project editors and revokes access', async ({
  page,
  browser,
  context,
  baseURL,
}, info) => {
  test.setTimeout(480000);
  const ownerFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    accountFile = process.env.NEON_E2E_CONSOLE_ACCOUNT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!ownerFile || !accountFile || !adminFile || !privateDir || !baseURL)
    throw new Error('Protected SQL owner and completed non-MFA test account required');
  const protectedDir: string = privateDir;
  const owner = JSON.parse(await readFile(ownerFile, 'utf8')),
    account = JSON.parse(await readFile(accountFile, 'utf8')),
    project = owner.project_id,
    writer = owner.writer_id;
  if (!account.checks?.includes('password_login_works_after_explicit_disable'))
    throw new Error('Retained console test account required');
  const resume = process.env.NEON_E2E_HISTORY_SHARE_RESUME;
  const state: {
    project_id: string;
    username: string;
    history_id: string;
    member_id: string;
    member_added: boolean;
    query_attempted: boolean;
    checks: string[];
  } = resume
    ? JSON.parse(await readFile(resume, 'utf8'))
    : {
        project_id: project,
        username: account.username,
        history_id: '',
        member_id: '',
        member_added: false,
        query_attempted: false,
        checks: [],
      };
  if (state.project_id !== project || state.username !== account.username)
    throw new Error('Original sharing checkpoint scope mismatch');
  const hadOriginalHistory = !!state.history_id;
  let result = 'running',
    queries = 0;
  const base = `/api/v1/projects/${project}/query-history`,
    shared = `/api/v1/projects/${project}/shared-query-history`;
  const readerContext = await browser.newContext({ baseURL }),
    reader = await readerContext.newPage();
  page.setDefaultTimeout(20000);
  reader.setDefaultTimeout(20000);
  for (const current of [page, reader])
    current.on('request', (r) => {
      if (r.method() === 'POST' && /\/query(-script)?$/.test(new URL(r.url()).pathname)) queries++;
    });
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(protectedDir, 'history-sharing-checkpoint.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks: state.checks,
        project_id: project,
        history_id: state.history_id,
        reader_id: state.member_id,
        sql_requests: queries,
        existing_history_deleted: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function grant(value: string) {
    await page.goto(`/#/projects/${project}/permissions`);
    const control = page.getByLabel('项目授权 ' + account.username, { exact: true });
    await expect(control).toBeVisible();
    if ((await control.inputValue()) === value) return;
    const waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname ===
          `/api/v1/projects/${project}/permissions/${state.member_id}` &&
        r.request().method() === (value === 'inherit' ? 'DELETE' : 'PUT'),
    );
    await page.getByLabel('项目授权 ' + account.username, { exact: true }).selectOption(value);
    expect((await waiting).status()).toBe(200);
    await expect(page.getByLabel('项目授权 ' + account.username, { exact: true })).toHaveValue(
      value,
    );
  }
  async function ownerSharing(enable: boolean) {
    await page.goto(`/#/projects/${project}/query`);
    await page
      .getByRole('button', { name: '管理历史共享 ' + state.history_id, exact: true })
      .click();
    const panel = page.getByRole('region', { name: '本人历史共享设置', exact: true });
    await expect(panel.getByTestId('history-sharing-current')).toBeVisible();
    if (enable)
      await panel
        .getByLabel('我确认将这一条敏感 SQL 历史共享给项目 SQL 编辑者', { exact: true })
        .check();
    const waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/' + state.history_id + '/sharing' &&
        r.request().method() === 'PUT',
    );
    await panel
      .getByRole('button', { name: enable ? '确认共享此历史' : '撤销此历史共享', exact: true })
      .click();
    const response = await waiting;
    expect(response.status()).toBe(200);
    await expect(panel.getByTestId('history-sharing-current')).toContainText(
      enable ? '已共享' : '仅本人',
    );
    return response;
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await reader.goto('/#/projects');
    await reader.getByLabel('用户名', { exact: true }).fill(account.username);
    await reader.getByLabel('密码', { exact: true }).fill(account.password);
    await reader.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(reader.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const session = await (await readerContext.request.get('/api/v1/session')).json();
    state.member_id = session.user.id;
    await save();
    const p = await (await context.request.get(`/api/v1/projects/${project}`)).json(),
      org = p.org_id;
    if (!org) throw new Error('Project organization required');
    if (!state.member_added) {
      const members = await (
        await context.request.get(`/api/v1/organizations/${org}/members`)
      ).json();
      if (members.items.some((m: { user_id: string }) => m.user_id === state.member_id))
        throw new Error('Do not modify an existing unrelated membership');
      expect((await readerContext.request.get(shared)).status()).toBe(404);
      await page.goto(`/#/projects/${project}/permissions`);
      await expect(page.getByRole('heading', { name: '项目权限', exact: true })).toBeVisible();
      const organizations = await (await context.request.get('/api/v1/organizations')).json();
      const organization = organizations.items.find((item: { id: string }) => item.id === org);
      if (!organization) throw new Error('Original organization missing');
      await expect(page.getByRole('button', { name: '切换组织', exact: true })).toContainText(
        organization.name,
      );
      await page.goto('/#/organization');
      await page.getByLabel('成员用户名', { exact: true }).fill(account.username);
      await page.getByLabel('新成员组织角色', { exact: true }).selectOption('collaborator');
      const waiting = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/api/v1/organizations/${org}/members` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '添加已有本地账号', exact: true }).click();
      expect((await waiting).status()).toBe(201);
      state.member_added = true;
      await save();
    }
    await grant('editor');
    await save('UI_grants_only_original_project_Editor_to_temporary_collaborator');
    if (!state.history_id) {
      if (state.query_attempted)
        throw new Error(
          'Original SQL outcome unconfirmed; inspect original receipt, never resubmit',
        );
      await page.goto(`/#/projects/${project}/query`);
      await page.getByRole('combobox', { name: '目标 Endpoint', exact: true }).selectOption(writer);
      await page.getByLabel('数据库角色', { exact: true }).fill(owner.role);
      await page.getByLabel('目标数据库', { exact: true }).fill(owner.database);
      await page
        .getByLabel('SQL 查询', { exact: true })
        .fill("SELECT 'explicit-history-share-中文'::text AS marker");
      await page
        .getByLabel('数据库角色密码', { exact: true })
        .fill((await readFile(owner.password_file, 'utf8')).trim());
      state.query_attempted = true;
      await save();
      const waiting = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query` &&
          r.request().method() === 'POST',
        { timeout: 190000 },
      );
      await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
      const response = await waiting;
      expect(response.status()).toBe(200);
      state.history_id = response.headers()['x-neon-sql-history-id'];
      expect(state.history_id).toMatch(/^qh_[a-f0-9]{24}$/);
      await save();
    }
    const original = await (await context.request.get(base + '/' + state.history_id)).json();
    expect(original.sql).toBe("SELECT 'explicit-history-share-中文'::text AS marker");
    await writeFile(info.outputPath('original-history.json'), JSON.stringify(original) + '\n');
    expect((await readerContext.request.get(base + '/' + state.history_id)).status()).toBe(404);
    expect((await readerContext.request.get(shared + '/' + state.history_id)).status()).toBe(404);
    await save('real_UI_SQL_history_is_private_before_explicit_consent');
    await page.goto(`/#/projects/${project}/compute`);
    await page.getByRole('combobox', { name: '选择 Endpoint', exact: true }).selectOption(writer);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    const sharedResponse = await ownerSharing(true),
      originalRequest = sharedResponse.request();
    await save('author_UI_explicitly_shares_only_selected_history_after_compute_zero');
    await reader.goto(`/#/projects/${project}/query`);
    await reader.reload();
    const panel = reader.getByRole('region', { name: '项目共享 SQL 执行历史', exact: true });
    await panel.getByRole('button', { name: '刷新项目共享历史', exact: true }).click();
    await expect(panel.getByTestId('shared-history-' + state.history_id)).toContainText('admin');
    const listing = await (await readerContext.request.get(shared)).json(),
      item = listing.items.find((v: { id: string }) => v.id === state.history_id);
    expect(item.sql).toBeUndefined();
    expect(item.sealed_sql).toBeUndefined();
    await reader.getByLabel('数据库角色密码', { exact: true }).fill('synthetic-not-submitted');
    await panel
      .getByRole('button', { name: '加载共享历史 ' + state.history_id, exact: true })
      .click();
    await expect(reader.getByLabel('SQL 查询', { exact: true })).toHaveValue(original.sql);
    await expect(reader.getByLabel('数据库角色密码', { exact: true })).toHaveValue('');
    await save('other_Editor_UI_loads_original_SQL_without_password_results_or_execution');
    await ownerSharing(false);
    expect((await readerContext.request.get(shared + '/' + state.history_id)).status()).toBe(404);
    await panel.getByRole('button', { name: '刷新项目共享历史', exact: true }).click();
    await expect(panel.getByTestId('shared-history-' + state.history_id)).toHaveCount(0);
    const replay = await context.request.put(base + '/' + state.history_id + '/sharing', {
      headers: {
        'X-CSRF-Token': originalRequest.headers()['x-csrf-token'],
        'Idempotency-Key': originalRequest.headers()['idempotency-key'],
        'If-Match': originalRequest.headers()['if-match'],
      },
      data: originalRequest.postDataJSON(),
    });
    expect(replay.status()).toBe(200);
    expect((await readerContext.request.get(shared + '/' + state.history_id)).status()).toBe(404);
    await save('UI_revocation_and_old_idempotency_replay_cannot_reexpose_private_SQL');
    await ownerSharing(true);
    expect((await readerContext.request.get(shared + '/' + state.history_id)).status()).toBe(200);
    await grant('inherit');
    expect((await readerContext.request.get(shared + '/' + state.history_id)).status()).toBe(404);
    await grant('editor');
    expect((await readerContext.request.get(shared + '/' + state.history_id)).status()).toBe(200);
    await save('UI_permission_revocation_denies_old_browser_and_restore_rechecks_access');
    await ownerSharing(false);
    expect((await context.request.get(base + '/' + state.history_id)).status()).toBe(200);
    await grant('inherit');
    await page.goto('/#/organization');
    page.once('dialog', (d) => void d.accept());
    const removing = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/organizations/${org}/members/${state.member_id}` &&
        r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '移除成员 ' + account.username, exact: true }).click();
    expect((await removing).status()).toBe(200);
    state.member_added = false;
    await save('original_history_retained_private_and_temporary_access_removed_through_UI');
    await page.goto(`/#/projects/${project}/compute`);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠');
    expect(queries).toBe(hadOriginalHistory ? 0 : 1);
    await save('all_sharing_and_permission_steps_leave_compute_zero_without_extra_SQL');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  } finally {
    await readerContext.close();
  }
});
