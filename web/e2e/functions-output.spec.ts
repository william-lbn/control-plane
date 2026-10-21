import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('Explicit customer output consent preserves redacted levels encrypted history and retired instance ownership', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!input || !admin || !zip)
    throw Error('Protected retained owner and exact Linux output bundle required');
  const f = JSON.parse(await readFile(input, 'utf8'));
  const { project_id: project, branch_id: branch, writer_id: writer } = f;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw Error('Exact owner required');
  const base = `/api/v1/projects/${project}/branches/${branch}/functions`,
    slug = 'logprobe55',
    fn = base + '/' + slug,
    secret = 'acceptance-secret55-' + Date.now();
  const resume = f.output_resume;
  const checks: string[] = [...(resume?.checks || [])],
    operations: string[] = [...(resume?.operations || [])];
  let result = 'running',
    functionID = resume?.function_id || '',
    url = resume?.public_url || '';
  let runtimeRecovery = resume?.runtime_recovery || null;
  let phase = 'resuming_original_checkpoints';
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
        public_url: url,
        runtime_recovery: runtimeRecovery,
        phase,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function get(path: string) {
    const r = await context.request.get(path);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function operation(r: Response) {
    expect(r.status()).toBe(202);
    const body = await r.json();
    operations.push(body.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const op = await get(`/api/v1/projects/${project}/operations/${body.operation.id}`);
          if (['failed', 'cancelled'].includes(op.state))
            throw Error(`Original operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function action(path: string, button: string, method = 'POST') {
    const p = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path && r.request().method() === method,
      { timeout: 210000 },
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return p;
  }
  async function refresh() {
    await page.goto(`/#/projects/${project}/functions`);
    await action(base, '刷新函数状态', 'GET');
    await page.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
    await expect(page.getByRole('region', { name: '客户输出日志', exact: true })).toHaveCount(1);
  }
  async function emit(marker: string) {
    const r = await context.request.get(url + '?marker=' + marker, { timeout: 210000 });
    expect(r.status()).toBe(200);
    expect((await r.json()).marker).toBe(marker);
  }
  async function logs() {
    return get(fn + '/output-logs?limit=100');
  }
  async function configure(enabled: boolean) {
    await page.getByLabel('采集客户输出', { exact: true }).setChecked(enabled);
    await page.getByLabel('确认输出可能包含敏感数据', { exact: true }).setChecked(enabled);
    expect((await action(fn + '/output-policy', '保存日志采集设置', 'PATCH')).status()).toBe(200);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!functionID) {
      await page.goto(`/#/projects/${project}/functions`);
      await page.getByLabel('函数名称', { exact: true }).fill(slug);
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      const created = await action(base, '创建函数定义');
      functionID = (await created.json()).function_id;
      await save();
      await operation(created);
      await refresh();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page
        .getByLabel('环境变量 JSON', { exact: true })
        .fill(JSON.stringify({ LOG_SECRET: secret }));
      await operation(await action(fn + '/deployments', '部署函数版本'));
      await refresh();
      await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
      await page.getByLabel('自动休眠唤醒', { exact: true }).uncheck();
      await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
      expect((await action(fn + '/http-policy', '保存入口策略', 'PATCH')).status()).toBe(200);
      url = (await get(fn)).invocation_url;
      await save('ui_original_native_function_deployed_with_explicit_test_environment');
    } else expect((await get(fn)).id).toBe(functionID);
    // An explicitly identified expired test instance can retire normally and
    // resume its retained source. Never extend certificates or replace the
    // original Function definition to make a failed acceptance disappear.
    if (f.output_recover_expired && runtimeRecovery?.stage !== 'completed') {
      const before = await get(fn);
      const original = runtimeRecovery?.original_deployment_id || before.active_deployment_id;
      runtimeRecovery = {
        ...runtimeRecovery,
        original_deployment_id: original,
        original_instance_id: f.output_recover_expired.instance_id,
        stage: 'pending',
      };
      await save();
      if (before.state === 'active' && before.active_deployment_id === original) {
        await refresh();
        await operation(await action(fn + '/suspend', '停止函数实例'));
        runtimeRecovery.stage = 'stopped';
        await save();
      }
      await refresh();
      if ((await get(fn)).state === 'suspended') {
        await operation(await action(fn + '/resume', '唤醒保留版本'));
      }
      const afterRecovery = await get(fn);
      expect(afterRecovery.id).toBe(functionID);
      expect(afterRecovery.state).toBe('active');
      expect(afterRecovery.active_deployment_id).not.toBe(original);
      const deployments = (await get(fn + '/deployments?limit=20')).items;
      const previous = deployments.find((d: { id: string }) => d.id === original);
      const restored = deployments.find(
        (d: { id: string }) => d.id === afterRecovery.active_deployment_id,
      );
      expect(previous.state).toBe('completed');
      expect(restored.state).toBe('completed');
      for (const field of ['bundle_digest', 'bundle_bytes', 'runtime', 'environment_names'])
        expect(restored[field]).toEqual(previous[field]);
      expect(restored.source_deployment_id).toBeNull();
      runtimeRecovery.resumed_deployment_id = restored.id;
      runtimeRecovery.stage = 'completed';
      await save();
    }
    if (
      !checks.includes('default_capture_off_and_sensitive_output_consent_lost_reply_exact_replay')
    ) {
      await emit('before-consent');
      expect((await logs()).items).toHaveLength(0);
      await refresh();
      await page.getByLabel('采集客户输出', { exact: true }).check();
      await expect(
        page.getByRole('button', { name: '保存日志采集设置', exact: true }),
      ).toBeDisabled();
      await page.getByLabel('确认输出可能包含敏感数据', { exact: true }).check();
      let saved: { key: string; version: string; body: string; receipt: unknown } | null = null;
      const policyPath = fn + '/output-policy';
      await page.route(
        '**' + policyPath,
        async (route) => {
          if (route.request().method() !== 'PATCH') {
            await route.continue();
            return;
          }
          const r = await route.fetch();
          expect(r.status()).toBe(200);
          saved = {
            key: route.request().headers()['idempotency-key'],
            version: route.request().headers()['if-match'],
            body: route.request().postData()!,
            receipt: await r.json(),
          };
          await route.abort('failed');
        },
        { times: 1 },
      );
      await page.getByRole('button', { name: '保存日志采集设置', exact: true }).click();
      await expect(page.getByRole('button', { name: '重试原日志设置', exact: true })).toBeVisible();
      if (!saved) throw Error('Original consent receipt required');
      const original = saved as { key: string; version: string; body: string; receipt: unknown };
      const request = page.waitForRequest(
        (r) => new URL(r.url()).pathname === policyPath && r.method() === 'PATCH',
      );
      const response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === policyPath && r.request().method() === 'PATCH',
      );
      await page.getByRole('button', { name: '重试原日志设置', exact: true }).click();
      expect((await response).status()).toBe(200);
      expect(await (await response).json()).toEqual(original.receipt);
      const replay = await request;
      expect(replay.headers()['idempotency-key']).toBe(original.key);
      expect(replay.headers()['if-match']).toBe(original.version);
      expect(replay.postData()).toBe(original.body);
      await save('default_capture_off_and_sensitive_output_consent_lost_reply_exact_replay');
    }
    if (!checks.includes('real_stdout_stderr_levels_scope_redaction_bounds_and_ui_download')) {
      await emit('after-consent');
      await expect
        .poll(async () => JSON.stringify((await logs()).items), {
          timeout: 45000,
          intervals: [2000],
        })
        .toContain('info after-consent');
      const data = await logs(),
        text = JSON.stringify(data.items);
      expect(text).not.toContain('acceptance-secret55-');
      expect(text).not.toContain('postgres://');
      expect(text).not.toContain('before-consent');
      expect(text).toContain('[REDACTED]');
      expect(text).toContain('[output line exceeded 4096 bytes]');
      for (const [severity, marker] of [
        ['DEBUG', 'debug'],
        ['INFO', 'info'],
        ['WARN', 'warn'],
        ['ERROR', 'error'],
      ])
        expect(
          data.items.some(
            (v: { severity: string; message: string }) =>
              v.severity === severity && v.message === marker + ' after-consent',
          ),
        ).toBe(true);
      const runtime = await get(fn + '/runtime');
      for (const v of data.items) {
        expect(v.instance_id).toBe(runtime.instance_id);
        expect(v.deployment_id).toBe(runtime.deployment_id);
        if (v.message.includes('forged platform')) expect(v.scope_name).toBe('neon.function.app');
      }
      expect(
        data.items.some(
          (v: { scope_name: string; message: string }) =>
            v.scope_name === 'neon.function.request' && v.message === 'invoke begin',
        ),
      ).toBe(true);
      expect(
        data.items.some(
          (v: { scope_name: string; message: string }) =>
            v.scope_name === 'neon.function.request' && v.message === 'invoke end',
        ),
      ).toBe(true);
      await refresh();
      await page.getByRole('button', { name: '刷新客户日志', exact: true }).click();
      await expect(page.getByTestId('function-output-lines')).toContainText('info after-consent');
      // Hold an older all-level response until the new WARN response has
      // completed. A stale refresh must never replace the chosen view.
      let releaseOld!: () => void;
      let oldStarted!: () => void;
      let oldDelivered!: () => void;
      const release = new Promise<void>((resolve) => {
        releaseOld = resolve;
      });
      const started = new Promise<void>((resolve) => {
        oldStarted = resolve;
      });
      const delivered = new Promise<void>((resolve) => {
        oldDelivered = resolve;
      });
      await page.route(
        (url) =>
          url.pathname === fn + '/output-logs' &&
          url.searchParams.get('limit') === '50' &&
          !url.searchParams.has('severity'),
        async (route) => {
          const response = await route.fetch();
          oldStarted();
          await release;
          await route.fulfill({ response });
          oldDelivered();
        },
        { times: 1 },
      );
      const refreshing = page.getByRole('button', { name: '刷新客户日志', exact: true }).click();
      phase = 'waiting_for_original_all_level_response';
      await save();
      await Promise.race([
        started,
        new Promise((_, reject) =>
          setTimeout(() => reject(Error('Original all-level response was not held')), 15000),
        ),
      ]);
      phase = 'original_all_level_response_held';
      await save();
      const filtered = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === fn + '/output-logs' &&
          new URL(r.url()).searchParams.get('severity') === 'WARN',
      );
      await page.getByLabel('客户日志级别', { exact: true }).selectOption('WARN');
      const filteredResponse = await filtered;
      phase = 'warn_response_received';
      await save();
      expect(filteredResponse.status()).toBe(200);
      expect(
        (await filteredResponse.json()).items.every(
          (v: { severity: string }) => v.severity === 'WARN',
        ),
      ).toBe(true);
      releaseOld();
      await Promise.race([
        delivered,
        new Promise((_, reject) =>
          setTimeout(() => reject(Error('Held all-level response was not delivered')), 15000),
        ),
      ]);
      await refreshing;
      phase = 'late_all_level_response_delivered';
      await save();
      await expect(page.getByTestId('function-output-lines')).toContainText('warn after-consent');
      await expect(page.getByTestId('function-output-lines')).not.toContainText(
        'debug after-consent',
      );
      await page.getByLabel('客户日志级别', { exact: true }).selectOption('');
      await page.getByLabel('筛选已加载日志', { exact: true }).fill('raw stdout');
      await expect(page.getByTestId('function-output-lines')).toContainText(
        'raw stdout after-consent',
      );
      await page.getByLabel('筛选已加载日志', { exact: true }).clear();
      const download = page.waitForEvent('download');
      await page.getByRole('button', { name: '导出已加载日志', exact: true }).click();
      const file = await (await download).path();
      if (!file) throw Error('Actual browser download required');
      const exported = await readFile(file, 'utf8');
      expect(exported).toContain('info after-consent');
      expect(exported).not.toContain('acceptance-secret55-');
      await save('real_stdout_stderr_levels_scope_redaction_bounds_and_ui_download');
    }
    if (!checks.includes('explicit_live_tail_and_disabled_interval_excluded_from_new_consent')) {
      await page.getByLabel('跟随最新客户日志', { exact: true }).check();
      await emit('live-tail');
      await expect(page.getByTestId('function-output-lines')).toContainText('info live-tail', {
        timeout: 45000,
      });
      await page.getByLabel('跟随最新客户日志', { exact: true }).uncheck();
      await configure(false);
      await emit('while-disabled');
      await page.waitForTimeout(6000);
      expect(JSON.stringify((await logs()).items)).not.toContain('while-disabled');
      await configure(true);
      await emit('new-consent');
      await expect
        .poll(async () => JSON.stringify((await logs()).items), {
          timeout: 45000,
          intervals: [2000],
        })
        .toContain('info new-consent');
      expect(JSON.stringify((await logs()).items)).not.toContain('while-disabled');
      await save('explicit_live_tail_and_disabled_interval_excluded_from_new_consent');
    }
    await refresh();
    await operation(await action(fn + '/suspend', '停止函数实例'));
    await expect
      .poll(async () => JSON.stringify((await logs()).items), { timeout: 30000, intervals: [2000] })
      .toContain('customer output shutdown');
    await refresh();
    await page.getByRole('button', { name: '刷新客户日志', exact: true }).click();
    await expect(page.getByTestId('function-output-lines')).toContainText('info after-consent');
    expect((await get(fn)).state).toBe('suspended');
    await configure(false);
    await save(
      'normal_instance_retirement_flushes_final_output_and_encrypted_history_remains_readable',
    );
    await operation(await action(fn, '删除函数', 'DELETE'));
    await page.goto(`/#/projects/${project}/compute`);
    if ((await get(`/api/v1/projects/${project}/endpoints/${writer}`)).state === 'active')
      await operation(
        await action(`/api/v1/projects/${project}/endpoints/${writer}/suspend`, '现在缩到 0'),
      );
    await save('normal_original_function_deletion_and_writer_zero_data_history_retained');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
