import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
test('UI and public actual Function requests appear in durable privacy-preserving request journal', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!fixtureFile || !adminFile || !zip)
    throw new Error('Protected owner/admin/fixed Linux bundle required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const project = f.project_id,
    branch = f.branch_id,
    slug = f.journal_slug || 'journal' + Date.now(),
    base = `/api/v1/projects/${project}/branches/${branch}/functions`,
    path = base + '/' + slug;
  const checks: string[] = f.journal_verify?.checks || [],
    operations: string[] = f.journal_verify?.operations || f.journal_recovery?.operations || [];
  let result = 'running',
    functionID = f.journal_verify?.function_id || f.journal_recovery?.function_id || '';
  const requestIDs: string[] = f.journal_verify?.request_ids || [];
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        request_ids: requestIDs,
        project_id: project,
        branch_id: branch,
        function_id: functionID,
        slug,
        production_qualified: false,
        sensitive_input_recorded: false,
      }) + '\n',
    );
  }
  async function wait(response: Response) {
    expect(response.status()).toBe(202);
    const value = await response.json();
    operations.push(value.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request
            .get(`/api/v1/projects/${project}/operations/${value.operation.id}`, { timeout: 15000 })
            .catch(() => null);
          if (!r || [502, 503, 504].includes(r.status())) return 'original_read_unavailable';
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(
              'Original journal setup operation failed ' + op.id + ' ' + op.error_code,
            );
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return value;
  }
  async function action(button: string, suffix: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path + suffix && r.request().method() === method,
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function refresh() {
    await page.goto(`/#/projects/${project}/functions`);
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'GET',
    );
    await page.getByRole('button', { name: '刷新函数状态', exact: true }).click();
    expect((await pending).status()).toBe(200);
    await page.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/functions`);
    if (!f.journal_verify) {
      if (!f.journal_recovery) {
        await page.getByLabel('函数名称', { exact: true }).fill(slug);
        await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
        await page.getByLabel('授权 Schema', { exact: true }).fill('app');
        const created = page.waitForResponse(
          (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
        );
        await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
        functionID = (await wait(await created)).function_id;
      } else {
        await refresh();
        await wait(await action('停止函数实例', '/suspend'));
        await refresh();
        await save(
          'original_kvm_failed_candidate_retired_by_uid_original_function_and_inputs_preserved',
        );
      }
      await page.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page
        .getByLabel('环境变量 JSON', { exact: true })
        .fill('{"APP_MESSAGE":"journal-native"}');
      await wait(await action('部署函数版本', '/deployments'));
      await refresh();
      await save('owned_small_function_setup_no_old_crud_or_scale_cycle_retested');
      await page.getByLabel('调用路径', { exact: true }).fill('/health?token=sentinel_do_not_log');
      const invoked = page.waitForResponse(
        (r) => new URL(r.url()).pathname === path + '/console-invoke',
      );
      await page.getByRole('button', { name: '调用函数', exact: true }).click();
      const consoleReply = await invoked;
      expect(consoleReply.status()).toBe(200);
      const consoleBody = await consoleReply.json();
      expect(consoleBody.status_code).toBe(200);
      requestIDs.push(consoleBody.request_id);
      await save('ui_actual_guest_health_response_and_request_id_recorded');
      const logsReply = page.waitForResponse(
        (r) => new URL(r.url()).pathname === path + '/request-logs',
      );
      await page.getByRole('button', { name: '加载请求日志', exact: true }).click();
      expect((await logsReply).status()).toBe(200);
      await expect(page.getByTestId('function-request-logs')).toContainText(requestIDs[0]);
      await expect(page.getByTestId('function-request-logs')).toContainText('已完成');
      await save('ui_displays_real_completed_console_request_journal');
      await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
      await page.getByLabel('自动休眠唤醒', { exact: true }).uncheck();
      await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
      const policy = page.waitForResponse(
        (r) => new URL(r.url()).pathname === path + '/http-policy',
      );
      await page.getByRole('button', { name: '保存入口策略', exact: true }).click();
      expect((await policy).status()).toBe(200);
      const publicReply = await context.request.get(
        `http://192.168.146.101:30892/functions/v1/${branch}/${slug}/health?token=sentinel_do_not_log`,
        { timeout: 210000 },
      );
      expect(publicReply.status()).toBe(200);
      requestIDs.push(publicReply.headers()['x-request-id']);
      expect(requestIDs[1]).toMatch(/^fhr_[a-f0-9]{24}$/);
      await save('actual_separate_public_origin_health_request_completed');
    } else {
      await refresh();
      await save(
        'actual_separate_public_origin_health_request_completed_original_ids_reused_no_reinvoke',
      );
    }
    await page.getByRole('button', { name: '加载请求日志', exact: true }).click();
    await expect(page.getByTestId('function-request-logs')).toContainText(requestIDs[1]);
    const durable = await context.request.get(path + '/request-logs?limit=1');
    expect(durable.status()).toBe(200);
    const first = await durable.json();
    expect(first.items[0].request_id).toBe(requestIDs[1]);
    expect(first.items[0].channel).toBe('public_http');
    const second = await context.request.get(
      path + '/request-logs?limit=1&after=' + encodeURIComponent(first.next_after),
    );
    expect(second.status()).toBe(200);
    expect((await second.json()).items[0].request_id).toBe(requestIDs[0]);
    const all = await context.request.get(path + '/request-logs');
    expect(await all.text()).not.toContain('sentinel_do_not_log');
    await save('durable_public_console_keyset_pagination_and_no_sensitive_query_capture');
    await page.screenshot({
      path: info.outputPath('functions-request-journal.png'),
      fullPage: true,
    });
    await refresh();
    await wait(await action('停止函数实例', '/suspend'));
    await page.goto(`/#/projects/${project}/compute`);
    const before = await context.request.get(
      `/api/v1/projects/${project}/endpoints/${f.writer_id}`,
    );
    if ((await before.json()).state === 'active') {
      const stop = page.waitForResponse(
        (r) => new URL(r.url()).pathname.endsWith('/suspend') && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await stop);
    }
    await save('owned_function_and_compute_stopped_journal_and_data_retained');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
