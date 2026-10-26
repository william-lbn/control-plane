import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Time Travel preview freezes one original timeline and recovers the original historical fork', async ({
  page,
  context,
  baseURL,
}, info) => {
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const fixtureFile = process.env.NEON_E2E_TIME_TRAVEL_FIXTURE;
  if (!privateDir || !adminFile || !fixtureFile || !baseURL)
    throw Error('Protected owned history fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    fixture.version !== 1 ||
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !fixture.org_name.startsWith('ci-')
  )
    throw Error('Explicit owned source required');
  const resume = process.env.NEON_E2E_TIME_TRAVEL_RESUME;
  const state: any = resume
    ? JSON.parse(await readFile(resume, 'utf8'))
    : {
        nonce: Date.now().toString(),
        project: fixture.project_id,
        branch: fixture.branch_id,
        checks: [],
      };
  if (state.project !== fixture.project_id || state.branch !== fixture.branch_id)
    throw Error('Original scope changed');
  let result = 'running';
  const root = '/api/v1/projects/' + state.project;
  const source = root + '/branches/' + state.branch;
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'time-travel-checkpoint.private.json'),
      JSON.stringify(state) + '\n',
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: state.project,
        source_branch_id: state.branch,
        target_branch_id: state.accepted?.resource.id,
        operation_id: state.accepted?.operation.id,
        checks: state.checks,
        credentials_in_evidence: false,
        creates_Compute: false,
        in_place_reset_tested: false,
      }) + '\n',
    );
  }
  async function get(url: string) {
    const r = await context.request.get(url);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function endpointIdentities() {
    const value = await get(root + '/endpoints');
    return value.items
      .map((v: any) => ({
        id: v.id,
        branch_id: v.branch_id,
        state: v.state,
        version: v.version,
        selector: v.selector,
        role_name: v.role_name,
        database_name: v.database_name,
        workload_kind: v.workload_kind,
        workload_name: v.workload_name,
      }))
      .sort((a: any, b: any) => a.id.localeCompare(b.id));
  }
  async function sendOriginal() {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf || !state.request) throw Error('Retain the original accepted request');
    const r = await context.request.post(root + '/branches', {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': state.request.key },
      data: state.request.body,
    });
    expect(r.status()).toBe(202);
    return r.json();
  }
  async function replay() {
    const value = await sendOriginal();
    expect(value.resource.id).toBe(state.accepted.resource.id);
    expect(value.operation.id).toBe(state.accepted.operation.id);
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.getByRole('button', { name: '切换组织' }).click();
    await page.getByRole('option', { name: fixture.org_name + ' · admin', exact: true }).click();
    if (!state.before) {
      state.before = await endpointIdentities();
      await save();
    }
    if (state.request && !state.accepted) {
      state.accepted = await sendOriginal();
      await save();
    }
    if (!state.accepted) {
      const window = await get(source + '/restore-window');
      await page.goto('/#/projects/' + state.project + '/restore');
      await page.getByLabel('恢复源分支').selectOption(state.branch);
      await page.getByRole('button', { name: '恢复到新分支', exact: true }).click();
      await page.getByLabel('分支名称').fill('tt-preview-' + state.nonce);
      await page.getByLabel('分支起点').selectOption('lsn');
      await page
        .getByLabel('恢复 LSN', { exact: true })
        .fill(state.preview?.lsn || window.latest_lsn);
      await page.getByLabel('同时创建读写 Compute Endpoint').uncheck();
      await expect(page.getByText('正在读取父分支函数策略…', { exact: true })).toBeHidden();
      const policy = page.getByLabel('函数策略');
      if (await policy.count()) await policy.selectOption('omit');
      const observed = page.waitForResponse(
        (r) => new URL(r.url()).pathname === source + '/restore-point',
      );
      await page.getByRole('button', { name: '预览时间点', exact: true }).click();
      const response = await observed;
      expect(response.status()).toBe(200);
      const preview = await response.json();
      if (state.preview) {
        expect(preview.source_timeline_id).toBe(state.preview.source_timeline_id);
        expect(preview.lsn).toBe(state.preview.lsn);
      }
      state.preview = preview;
      expect(state.preview.branch_id).toBe(state.branch);
      expect(state.preview.retention_pinned).toBe(false);
      await expect(page.getByRole('status').filter({ hasText: '已解析 LSN' })).toContainText(
        state.preview.lsn,
      );
      await save('real_page_read_only_preview');
      let intercepted = false;
      await page.route('**' + root + '/branches', async (route) => {
        if (route.request().method() !== 'POST' || intercepted) {
          await route.continue();
          return;
        }
        intercepted = true;
        const request = route.request();
        state.request = { key: request.headers()['idempotency-key'], body: request.postDataJSON() };
        await save();
        expect(state.request.body.parent_lsn).toBe(state.preview.lsn);
        expect(state.request.body.expected_parent_timeline_id).toBe(
          state.preview.source_timeline_id,
        );
        expect(state.request.body.create_endpoint).toBe(false);
        expect(state.request.body.parent_timestamp).toBeUndefined();
        const accepted = await route.fetch();
        expect(accepted.status()).toBe(202);
        state.accepted = await accepted.json();
        await save();
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({ code: 'owned_response_lost', message: '恢复原请求' }),
        });
      });
      const original = page.waitForResponse(
        (r) => new URL(r.url()).pathname === root + '/branches' && r.request().method() === 'POST',
      );
      await page.locator('.create-actions button.primary').click();
      expect((await original).status()).toBe(503);
      await expect(page.locator('.create-actions button.primary')).toBeEnabled();
      const restored = page.waitForResponse(
        (r) => new URL(r.url()).pathname === root + '/branches' && r.request().method() === 'POST',
      );
      await page.locator('.create-actions button.primary').click();
      const recovered = await restored;
      expect(recovered.status()).toBe(202);
      const receipt = await recovered.json();
      expect(receipt.resource.id).toBe(state.accepted.resource.id);
      expect(receipt.operation.id).toBe(state.accepted.operation.id);
      expect(recovered.request().postDataJSON()).toEqual(state.request.body);
      expect(recovered.request().headers()['idempotency-key']).toBe(state.request.key);
      await save('lost_POST_ui_recovers_same_original_point_and_operation');
    }
    if (!state.accepted)
      throw Error('Original outcome unresolved; retain the checkpoint without a new proposal');
    await replay();
    await expect
      .poll(
        async () => {
          const value = await get(root + '/operations/' + state.accepted.operation.id);
          if (['failed', 'cancelled'].includes(value.state))
            throw Error('Retain original Operation ' + value.id);
          return value.state;
        },
        { timeout: 180000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
    const child = await get(root + '/branches/' + state.accepted.resource.id);
    expect(child.parent_branch_id).toBe(state.branch);
    expect(child.parent_lsn).toBe(state.preview.lsn);
    expect(child.restore_source).toBe('lsn');
    await save('real_storage_fork_uses_exact_preview_LSN');
    expect(await endpointIdentities()).toEqual(state.before);
    await replay();
    await save('original_request_replay_retains_source_and_no_new_Compute');
    for (const name of [
      'real_page_read_only_preview',
      'lost_POST_ui_recovers_same_original_point_and_operation',
      'real_storage_fork_uses_exact_preview_LSN',
      'original_request_replay_retains_source_and_no_new_Compute',
    ])
      expect(state.checks).toContain(name);
    result = 'pass';
    await save();
  } finally {
    await save();
  }
});
