import { expect, test, type APIResponse, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('Public Functions HTTP/SSE preserves SQL and automatically sleeps/wakes without request replay', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE;
  const passwordFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!fixtureFile || !passwordFile || !zip)
    throw new Error('Protected Linux owner fixture and HTTP bundle required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(fixture.writer_id)
  )
    throw new Error('Exact owner scope required');
  const { project_id: project, branch_id: branch } = fixture;
  const base = `/api/v1/projects/${project}/branches/${branch}/functions`,
    path = base + '/edge';
  const checks: string[] = [],
    operations: string[] = [];
  let result = 'running',
    functionID = '',
    publicURL = '';
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        project_id: project,
        branch_id: branch,
        function_id: functionID,
        public_url: publicURL,
        production_qualified: false,
        credentials_in_evidence: false,
      }) + '\n',
    );
  }
  async function metadata() {
    const r = await context.request.get(path);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function wait(r: APIResponse | Response) {
    expect(r.status()).toBe(202);
    const accepted = await r.json();
    operations.push(accepted.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const reply = await context.request.get(
            `/api/v1/projects/${project}/operations/${accepted.operation.id}`,
          );
          expect(reply.status()).toBe(200);
          const op = await reply.json();
          if (op.state === 'failed' || op.state === 'cancelled')
            throw new Error(`Operation failed ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
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
    await page.getByRole('button', { name: '查看函数 edge', exact: true }).click();
  }
  async function runtime() {
    const r = await context.request.get(path + '/runtime');
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function zero() {
    await page.goto(`/#/projects/${project}/compute`);
    const state = await context.request.get(
      `/api/v1/projects/${project}/endpoints/${fixture.writer_id}`,
    );
    expect(state.status()).toBe(200);
    if ((await state.json()).state === 'active') {
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname.endsWith('/suspend') && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await pending);
    }
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page
      .getByLabel('密码', { exact: true })
      .fill((await readFile(passwordFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByLabel('函数名称', { exact: true }).fill('edge');
    await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
    await page.getByLabel('授权 Schema', { exact: true }).fill('app');
    const created = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
    functionID = (await wait(await created)).function_id;
    await page.getByRole('button', { name: '查看函数 edge', exact: true }).click();
    await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
    await page
      .getByLabel('环境变量 JSON', { exact: true })
      .fill('{"APP_MESSAGE":"public-retained"}');
    await wait(await action('部署函数版本', '/deployments'));
    await refresh();
    const original = await runtime();
    expect(original.node_ready).toBe(true);
    expect((await metadata()).invocation_url).toBeNull();
    await save('ui_native_deployment_private_by_default_original_runtime_observed');
    await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
    await page.getByLabel('自动休眠唤醒', { exact: true }).check();
    await page.getByLabel('函数空闲秒数', { exact: true }).fill('30');
    await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
    const policyPending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path + '/http-policy',
    );
    await page.getByRole('button', { name: '保存入口策略', exact: true }).click();
    const policy = await policyPending;
    expect(policy.status()).toBe(200);
    const headers = await policy.request().allHeaders(),
      input = policy.request().postDataJSON();
    const replay = await context.request.patch(path + '/http-policy', {
      headers: {
        'X-CSRF-Token': headers['x-csrf-token'],
        'Idempotency-Key': headers['idempotency-key'],
        'If-Match': headers['if-match'],
      },
      data: input,
    });
    expect(replay.status()).toBe(200);
    expect((await replay.json()).version_token).toBe((await policy.json()).version_token);
    publicURL = (await metadata()).invocation_url;
    expect(new URL(publicURL).origin).not.toBe(new URL(page.url()).origin);
    await save('ui_public_idle_opt_in_policy_atomic_replay_and_separate_origin');
    const health = await context.request.get(publicURL + '/health');
    expect(health.status()).toBe(200);
    expect((await health.json()).message).toBe('public-retained');
    const echo = await context.request.post(publicURL + '/echo', {
      headers: {
        Cookie: 'neon_v2_session=private',
        'X-Forwarded-Host': 'console.invalid',
        Authorization: 'Bearer app-only',
      },
      data: 'unicode 测试',
    });
    expect(echo.status()).toBe(200);
    expect(await echo.json()).toEqual({
      body: 'unicode 测试',
      cookie: null,
      authorization: 'Bearer app-only',
      forwarded: null,
    });
    const html = await context.request.get(publicURL + '/html');
    expect(html.status()).toBe(200);
    expect(html.headers()['set-cookie']).toBeUndefined();
    expect(await html.text()).toBe('<b>application HTML</b>');
    const console = await context.request.get(new URL('/api/v1/projects', publicURL).toString());
    expect(console.status()).toBe(404);
    const redirect = await context.request.get(publicURL + '/redirect', { maxRedirects: 0 });
    expect(redirect.status()).toBe(307);
    await save(
      'actual_public_http_raw_html_cookie_isolation_application_authorization_redirect_and_no_console_routes',
    );
    const before = await context.request.get(publicURL + '/');
    expect(before.status()).toBe(200);
    const data = await before.json();
    const marker = 'edge-' + functionID;
    const write = await context.request.post(publicURL + '/write', { data: { marker } });
    expect(write.status()).toBe(200);
    expect((await write.json()).rows).toBe(data.rows + 1);
    await save('public_invocation_actual_restricted_neon_sql_single_write');
    const streamPromise = fetch(publicURL + '/stream');
    const stream = await streamPromise;
    expect(stream.status).toBe(200);
    expect(stream.headers.get('content-type')).toContain('text/event-stream');
    const reader = stream.body!.getReader();
    const initial = await reader.read();
    expect(new TextDecoder().decode(initial.value)).toContain('started');
    await new Promise((resolve) => setTimeout(resolve, 34000));
    const during = await runtime();
    expect(during.instance_id).toBe(original.instance_id);
    expect(during.active_invocations).toBeGreaterThan(0);
    let text = '';
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) break;
      text += new TextDecoder().decode(chunk.value);
    }
    expect(text).toContain('finished');
    await save('actual_sse_flush_and_active_stream_prevents_idle_retirement');
    await expect
      .poll(async () => (await metadata()).state, { timeout: 180000, intervals: [2500, 5000] })
      .toBe('suspended');
    expect((await metadata()).idle_suspended).toBe(true);
    await save('no_request_automatic_suspend_durable_idle_marker');
    // A first public read waits for the retained VM to cold start. It is sent
    // once; writes are never automatically retried by this test or the ingress.
    const woke = await context.request.get(publicURL + '/', { timeout: 210000 });
    expect(woke.status()).toBe(200);
    const after = await woke.json();
    expect(after.rows).toBe(data.rows + 1);
    expect(after.role).toBe('fn_' + functionID.slice(4));
    expect(after.message).toBe('public-retained');
    const next = await runtime();
    expect(next.instance_id).not.toBe(original.instance_id);
    await save(
      'first_public_request_cold_wakes_new_boot_retained_code_environment_sql_identity_and_data',
    );
    await refresh();
    await wait(await action('停止函数实例', '/suspend'));
    const stopped = await context.request.get(publicURL + '/health');
    expect(stopped.status()).toBe(503);
    expect((await metadata()).idle_suspended).toBe(false);
    await save('manual_suspend_does_not_allow_automatic_public_wake');
    await refresh();
    await wait(await action('删除函数', '', 'DELETE'));
    await zero();
    await save('owned_function_runtime_retired_sql_data_and_all_evidence_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
