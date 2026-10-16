import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';

test('UI private history names recover original commits and require explicit AI adoption', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !privateDir)
    throw new Error('Original protected SQL history/admin inputs required');
  const protectedDir: string = privateDir;
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    naming_history_id: string;
    naming_source_sha256: string;
  };
  if (
    !/^qh_[a-f0-9]{24}$/.test(f.naming_history_id) ||
    !/^[a-f0-9]{64}$/.test(f.naming_source_sha256)
  )
    throw new Error('Original source history identity/hash required');
  const base = `/api/v1/projects/${f.project_id}/query-history/${f.naming_history_id}`,
    namePath = base + '/name',
    aiPath = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}/ai-gateway/request`,
    queryPath = `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}/query`;
  let result = 'running',
    queries = 0,
    inferences = 0,
    mutations = 0,
    lost = false;
  const checks: string[] = [],
    requestIds: string[] = [];
  let original: { key: string; version: string; body: string; receipt: unknown } | null = null;
  context.on('request', (r) => {
    const p = new URL(r.url()).pathname;
    if (r.method() === 'POST' && p === queryPath) queries++;
    if (r.method() === 'POST' && p === aiPath) inferences++;
    if (r.method() === 'PUT' && p === namePath) mutations++;
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        request_ids: requestIds,
        history_id: f.naming_history_id,
        project_id: f.project_id,
        name_put_requests: mutations,
        sql_requests: queries,
        ai_requests: inferences,
        production_qualified: false,
        secrets_in_evidence: false,
      }) + '\n',
    );
    if (original)
      await writeFile(
        path.join(protectedDir, 'history-name-original.private.json'),
        JSON.stringify(original),
        { mode: 0o600 },
      );
  }
  async function open(p: typeof page) {
    await p.goto(`/#/projects/${f.project_id}/query`);
    await p
      .getByRole('button', { name: '管理历史名称 ' + f.naming_history_id, exact: true })
      .click();
    const panel = p.getByTestId('history-name-panel');
    await expect(panel.getByTestId('history-name-current')).toBeVisible();
    return panel;
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const panel = await open(page);
    await expect(panel.getByTestId('history-name-current')).toHaveText(/未命名 · 版本 0$/);
    const second = await context.newPage(),
      other = await open(second);
    await expect(other.getByTestId('history-name-current')).toHaveText(/版本 0$/);
    await other.getByLabel('本人历史名称', { exact: true }).fill('Stale panel must not overwrite');
    await page.route('**' + namePath, async (route) => {
      if (route.request().method() === 'PUT' && !lost) {
        lost = true;
        const headers = route.request().headers(),
          upstream = await route.fetch();
        expect(upstream.status()).toBe(200);
        original = {
          key: headers['idempotency-key'],
          version: headers['if-match'],
          body: route.request().postData() || '',
          receipt: await upstream.json(),
        };
        await save();
        await route.abort('failed');
      } else await route.continue();
    });
    await panel.getByLabel('本人历史名称', { exact: true }).fill('UI personal history label');
    await panel.getByRole('button', { name: '保存个人名称', exact: true }).click();
    await expect(panel.getByRole('button', { name: '重试原名称提交', exact: true })).toBeVisible();
    await expect(panel.getByLabel('本人历史名称', { exact: true })).toBeDisabled();
    expect(mutations).toBe(1);
    expect(queries).toBe(0);
    expect(inferences).toBe(0);
    await save(
      'actual_server_commit_response_lost_original_key_body_version_and_receipt_preserved',
    );
    const retry = page.waitForResponse(
      (r) => new URL(r.url()).pathname === namePath && r.request().method() === 'PUT',
    );
    await panel.getByRole('button', { name: '重试原名称提交', exact: true }).click();
    const replay = await retry;
    expect(replay.status()).toBe(200);
    const retained = original as {
      key: string;
      version: string;
      body: string;
      receipt: unknown;
    } | null;
    if (!retained) throw new Error('Original committed intent missing');
    expect(replay.request().headers()['idempotency-key']).toBe(retained.key);
    expect(replay.request().headers()['if-match']).toBe(retained.version);
    expect(replay.request().postData()).toBe(retained.body);
    expect(await replay.json()).toEqual(retained.receipt);
    await expect(panel.getByTestId('history-name-current')).toHaveText(
      /UI personal history label · 版本 1$/,
    );
    await save(
      'UI_retries_exact_original_annotation_and_reads_current_version_without_replaying_SQL',
    );
    const conflict = second.waitForResponse(
      (r) => new URL(r.url()).pathname === namePath && r.request().method() === 'PUT',
    );
    await other.getByRole('button', { name: '保存个人名称', exact: true }).click();
    expect((await conflict).status()).toBe(412);
    await expect(other.getByLabel('本人历史名称', { exact: true })).toBeDisabled();
    await other.getByRole('button', { name: '读取当前名称', exact: true }).click();
    await expect(other.getByTestId('history-name-current')).toHaveText(
      /UI personal history label · 版本 1$/,
    );
    await second.close();
    await save('second_UI_panel_stale_generation_is_rejected_and_reload_preserves_winning_name');
    await panel.getByLabel('本人历史名称', { exact: true }).fill('');
    await panel.getByRole('button', { name: '清除个人名称', exact: true }).click();
    await expect(panel.getByTestId('history-name-current')).toHaveText(/未命名 · 版本 2$/);
    await save('UI_clear_only_removes_private_annotation_and_retains_execution_history');
    await page
      .getByLabel('SQL 查询', { exact: true })
      .fill("SELECT 'unsaved_editor_not_for_history_naming'");
    await panel.getByText('AI 建议名称 · 需单独确认发送历史 SQL', { exact: true }).click();
    await expect(
      panel.getByRole('button', { name: '生成历史名称建议', exact: true }),
    ).toBeDisabled();
    await panel.getByRole('button', { name: '读取这条本人历史 SQL', exact: true }).click();
    await panel
      .getByRole('combobox', { name: '历史命名模型', exact: true })
      .selectOption('local-sql');
    const preview = panel.getByLabel('历史命名发送内容', { exact: true });
    await expect(preview).toHaveValue(/reference_sql/);
    const encoded = await preview.inputValue(),
      source = JSON.parse(JSON.parse(encoded).messages[1].content).reference_sql as string;
    expect(createHash('sha256').update(source).digest('hex')).toBe(f.naming_source_sha256);
    expect(encoded).not.toContain('unsaved_editor_not_for_history_naming');
    expect(queries).toBe(0);
    expect(inferences).toBe(0);
    await save(
      'explicit_source_is_the_original_personal_history_not_unsaved_editor_or_new_database_query',
    );
    await panel.getByRole('checkbox', { name: '确认发送这条历史 SQL', exact: true }).check();
    const generated = page.waitForResponse(
      (r) => new URL(r.url()).pathname === aiPath && r.request().method() === 'POST',
      { timeout: 200000 },
    );
    await panel.getByRole('button', { name: '生成历史名称建议', exact: true }).click();
    const response = await generated;
    requestIds.push(response.headers()['x-request-id'] || '');
    await writeFile(
      path.join(privateDir, 'history-name-model.private.json'),
      JSON.stringify(await response.json()),
      { mode: 0o600 },
    );
    expect(response.status()).toBe(200);
    await expect(panel.getByTestId('history-name-ai-draft')).toBeVisible({ timeout: 200000 });
    const candidate = await panel.getByTestId('history-name-ai-draft').innerText();
    expect(candidate.length).toBeGreaterThan(0);
    expect(mutations).toBe(4);
    expect(queries).toBe(0);
    await save('real_model_name_is_a_review_draft_and_does_not_save_or_execute_SQL');
    await panel.getByRole('button', { name: '采用名称建议', exact: true }).click();
    await expect(panel.getByLabel('本人历史名称', { exact: true })).toHaveValue(candidate);
    expect(mutations).toBe(4);
    await panel.getByRole('button', { name: '保存个人名称', exact: true }).click();
    await expect(panel.getByTestId('history-name-current')).toHaveText(
      `当前名称：${candidate} · 版本 3`,
    );
    await page.reload();
    await expect(page.getByTestId('history-name-' + f.naming_history_id)).toHaveText(candidate);
    expect(queries).toBe(0);
    expect(inferences).toBe(1);
    expect(mutations).toBe(5);
    await save(
      'explicit_adoption_and_save_persist_real_AI_name_across_reload_on_the_original_history',
    );
    await page.screenshot({ path: info.outputPath('history-names.png'), fullPage: true });
    await page.goto(`/#/projects/${f.project_id}/compute`);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠');
    await save(
      'all_naming_paths_metadata_only_compute_stays_zero_original_SQL_and_failures_retained',
    );
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
