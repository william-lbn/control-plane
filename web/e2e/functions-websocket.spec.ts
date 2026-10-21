import { expect, test, type APIResponse, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('Functions WebSocket native handshake messages active idle and retained cold wake', async ({
  page,
  context,
}, info) => {
  const fixturePath = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE;
  const passwordPath = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!fixturePath || !passwordPath || !zip)
    throw new Error('Protected owner and native bundle required');
  const fixture = JSON.parse(await readFile(fixturePath, 'utf8'));
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw new Error('Exact retained owner required');
  const slug = 'wsprobe18',
    base = `/api/v1/projects/${project}/branches/${branch}/functions`,
    path = base + '/' + slug;
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
        slug,
        public_url: publicURL,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function metadata() {
    const r = await context.request.get(path);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function runtime() {
    const r = await context.request.get(path + '/runtime');
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
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${accepted.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Original operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
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
  async function clickAction(button: string, suffix: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path + suffix && r.request().method() === method,
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function connect() {
    await page.getByLabel('WebSocket 子协议', { exact: true }).fill('echo.v1');
    await page.getByRole('button', { name: '连接 WebSocket', exact: true }).click();
    await expect(page.getByTestId('function-websocket-state')).toContainText('已连接 · echo.v1', {
      timeout: 210000,
    });
    await expect(page.getByTestId('function-websocket-messages')).toContainText('ready');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page
      .getByLabel('密码', { exact: true })
      .fill((await readFile(passwordPath, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    const resume = fixture.websocket_resume;
    if (resume) {
      if (!/^fnc_[a-f0-9]{16}$/.test(resume.function_id) || resume.slug !== slug)
        throw new Error('Original resume scope required');
      functionID = resume.function_id;
      operations.push(...resume.operations);
      checks.push(...resume.checks);
      expect((await metadata()).id).toBe(functionID);
    } else {
      await page.goto(`/#/projects/${project}/functions`);
      await page.getByLabel('函数名称', { exact: true }).fill(slug);
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(202);
      const accepted = await response.json();
      functionID = accepted.function_id;
      await save();
      await wait(response);
      await page.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await wait(await clickAction('部署函数版本', '/deployments'));
      await refresh();
      await save('original_native_function_deployed_from_pinned_websocket_guest');
    }
    if (!(await metadata()).public_enabled) {
      await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
      await page.getByLabel('自动休眠唤醒', { exact: true }).check();
      await page.getByLabel('函数空闲秒数', { exact: true }).fill('30');
      await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === path + '/http-policy',
      );
      await page.getByRole('button', { name: '保存入口策略', exact: true }).click();
      expect((await pending).status()).toBe(200);
    }
    publicURL = (await metadata()).invocation_url;
    let original: { instance_id: string; deployment_id: string };
    if (resume?.cold_wake_proof) {
      const proof = resume.cold_wake_proof;
      expect(proof.function_id).toBe(functionID);
      expect(checks).toContain('normal_client_close_and_original_automatic_idle_retirement');
      original = {
        instance_id: proof.original_instance_id,
        deployment_id: proof.original_deployment_id,
      };
      const previous = await context.request.get(path + '/deployments?limit=100');
      expect(previous.status()).toBe(200);
      const records = await previous.json();
      const accepted = records.items.find(
        (d: { id: string }) => d.id === proof.woken_deployment_id,
      );
      expect(accepted?.state).toBe('completed');
      expect(proof.original_automatic_retained_resume_succeeded).toBe(true);
      await refresh();
      await connect();
    } else {
      await refresh();
      await connect();
      const observed = await runtime();
      original = observed;
      expect(observed.node_ready).toBe(true);
      expect(observed.active_invocations).toBeGreaterThan(0);
      await page.getByLabel('WebSocket 消息', { exact: true }).fill('中文 echo');
      await page.getByRole('button', { name: '发送 WebSocket 消息', exact: true }).click();
      await expect(page.getByTestId('function-websocket-messages')).toContainText('中文 echo');
      await save('ui_handshake_selected_subprotocol_and_real_text_echo');
      const binary = await page.evaluate(async (url) => {
        const target = new URL(url + '/ws');
        target.protocol = target.protocol === 'https:' ? 'wss:' : 'ws:';
        return new Promise<number[]>((resolve, reject) => {
          const ws = new WebSocket(target, ['echo.v1']);
          ws.binaryType = 'arraybuffer';
          const timer = setTimeout(() => {
            ws.close();
            reject(new Error('Binary echo timeout'));
          }, 15000);
          ws.onerror = () => reject(new Error('Binary connection failed'));
          ws.onopen = () => ws.send(new Uint8Array([0, 255, 7]));
          ws.onmessage = (e) => {
            if (typeof e.data !== 'string') {
              clearTimeout(timer);
              ws.close(1000, 'done');
              resolve([...new Uint8Array(e.data)]);
            }
          };
        });
      }, publicURL);
      expect(binary).toEqual([0, 255, 7]);
      const denied = await context.request.get(publicURL + '/deny', {
        headers: {
          Connection: 'Upgrade',
          Upgrade: 'websocket',
          'Sec-WebSocket-Version': '13',
          'Sec-WebSocket-Key': 'dGhlIHNhbXBsZSBub25jZQ==',
        },
      });
      expect(denied.status()).toBe(401);
      await save('binary_bytes_and_application_handshake_authorization');
      await page.waitForTimeout(34000);
      const during = await runtime();
      expect(during.instance_id).toBe(original.instance_id);
      expect(during.active_invocations).toBeGreaterThan(0);
      expect((await metadata()).state).toBe('active');
      await save('open_connection_counts_as_activity_and_prevents_automatic_idle');
      await page.getByRole('button', { name: '关闭 WebSocket', exact: true }).click();
      await expect(page.getByTestId('function-websocket-state')).toContainText('1000');
      await expect
        .poll(async () => (await metadata()).state, { timeout: 180000, intervals: [2500, 5000] })
        .toBe('suspended');
      await save('normal_client_close_and_original_automatic_idle_retirement');
      await refresh();
      await connect();
    }
    const awakened = await runtime();
    expect(awakened.node_ready).toBe(true);
    expect(awakened.instance_id).not.toBe(original.instance_id);
    const historyResponse = await context.request.get(path + '/deployments?limit=100');
    expect(historyResponse.status()).toBe(200);
    const history = await historyResponse.json();
    const baseline = history.items.find((d: { id: string }) => d.id === original.deployment_id);
    expect(baseline?.state).toBe('completed');
    const current = history.items.find((d: { id: string }) => d.id === awakened.deployment_id);
    expect(current).toBeTruthy();
    expect(current.function_id).toBe(functionID);
    expect(current.project_id).toBe(project);
    expect(current.branch_id).toBe(branch);
    expect(current.state).toBe('completed');
    expect(current.bundle_digest).toBe(baseline.bundle_digest);
    expect(current.bundle_bytes).toBe(baseline.bundle_bytes);
    expect(current.entry).toBe(baseline.entry);
    expect(current.environment_names).toEqual(baseline.environment_names);
    expect((await metadata()).id).toBe(functionID);
    await page.getByLabel('WebSocket 消息', { exact: true }).fill('server-close');
    await page.getByRole('button', { name: '发送 WebSocket 消息', exact: true }).click();
    await expect(page.getByTestId('function-websocket-state')).toContainText('4001');
    await save(
      'explicit_websocket_reconnect_cold_wakes_same_bundle_and_preserves_server_close_code',
    );
    const logs = await context.request.get(path + '/request-logs');
    expect(logs.status()).toBe(200);
    expect(
      (await logs.json()).items.some(
        (v: { application_status: number }) => v.application_status === 101,
      ),
    ).toBe(true);
    await refresh();
    await wait(await clickAction('停止函数实例', '/suspend'));
    await refresh();
    await wait(await clickAction('删除函数', '', 'DELETE'));
    await page.goto(`/#/projects/${project}/compute`);
    const state = await context.request.get(`/api/v1/projects/${project}/endpoints/${writer}`);
    expect(state.status()).toBe(200);
    if ((await state.json()).state === 'active') {
      const stop = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/suspend` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await stop);
    }
    await save('request_status_101_journal_and_normal_function_compute_retirement');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
