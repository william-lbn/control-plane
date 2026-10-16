import { expect, test, type APIResponse, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { unzipSync } from 'fflate';

test('UI schedule wakes original sleeping Function and records one real restricted SQL effect', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !zip || !privateDir)
    throw Error('Protected original owner, Linux bundle and private evidence directory required');
  const protectedDir: string = privateDir;
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    schedule_resume?: {
      function_id: string;
      slug: string;
      name: string;
      checks: string[];
      operations: string[];
      original_failure_job: string;
      corrected_bundle_required?: boolean;
      prior_invocation_ids?: string[];
      trigger_id?: string;
      failed_invocation_id?: string;
      failed_deploy_operation_id?: string;
      recovery_stop_operation_id?: string;
      retired_operation_id?: string;
      retirement_evidence_job?: string;
    };
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(f.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(f.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(f.writer_id)
  )
    throw Error('Original owner scope required');
  const archive = await readFile(zip),
    entry = unzipSync(archive)['index.mjs'];
  if (
    !entry ||
    !Buffer.from(entry).equals(
      await readFile(new URL('../../examples/functions/schedule/index.mjs', import.meta.url)),
    )
  )
    throw Error('Linux schedule bundle must contain the exact sealed acceptance entry');
  const bundleDigest = createHash('sha256').update(archive).digest('hex');
  const resume = f.schedule_resume;
  if (
    resume &&
    (!/^fnc_[a-f0-9]{16}$/.test(resume.function_id) ||
      !/^[a-z0-9]{1,20}$/.test(resume.slug) ||
      !resume.name ||
      resume.checks.length !== 1 ||
      resume.checks[0] !== 'ui_native_definition_in_retained_owner_project' ||
      resume.operations.length !== 2 ||
      !resume.operations.every((id) => /^op_[a-f0-9]{16,24}$/.test(id)) ||
      !/^publication-ui-[0-9]{14}$/.test(resume.original_failure_job))
  )
    throw Error('Exact original deployment checkpoint required');
  const slug = resume?.slug || 'cron' + Date.now(),
    name = resume?.name || 'minute-' + Date.now(),
    base = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}/functions`,
    fn = base + '/' + slug,
    triggers = base.replace(/\/functions$/, '/functions-triggers');
  let result = 'running',
    functionID = resume?.function_id || '',
    triggerID = resume?.trigger_id || '',
    invocationID = '',
    publicURL = '';
  const checks: string[] = [...(resume?.checks || [])],
    operations: string[] = [...(resume?.operations || [])],
    requestIDs: string[] = [];
  let originalFailedReceipt: unknown = null;
  let priorReceipts: unknown[] = [];
  const authenticationObservations: unknown[] = [];
  let computeIdlePolicy: { scale_to_zero: boolean; idle_timeout_seconds: number } | null = null;
  let original: { key: string; body: string; receipt: unknown } | null = null;
  const save = async (check?: string) => {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        request_ids: requestIDs,
        project_id: f.project_id,
        branch_id: f.branch_id,
        function_id: functionID,
        slug,
        trigger_id: triggerID,
        invocation_id: invocationID,
        original_failed_invocation_id: resume?.failed_invocation_id,
        acceptance_bundle_digest: bundleDigest,
        prior_invocation_ids: resume?.prior_invocation_ids,
        production_qualified: false,
        customer_bodies_recorded: false,
        resumed_from: resume?.original_failure_job,
        authentication_observations: authenticationObservations,
        original_compute_idle_policy: computeIdlePolicy,
      }) + '\n',
    );
    await writeFile(
      path.join(protectedDir, 'schedule-original.private.json'),
      JSON.stringify({
        project_id: f.project_id,
        branch_id: f.branch_id,
        writer_id: f.writer_id,
        function_id: functionID,
        slug,
        trigger_id: triggerID,
        name,
        operations,
        checks,
        original,
      }) + '\n',
    );
  };
  const read = async (url: string): Promise<APIResponse> => {
    if (!url.startsWith('/api/v1/'))
      throw Error('Bounded authenticated read requires a Console API path');
    for (let attempt = 0; ; attempt++) {
      const response = await context.request.get(url);
      if (response.status() === 401) {
        const cookies = await context.cookies();
        authenticationObservations.push({
          path: url,
          status: 401,
          request_id: response.headers()['x-request-id'],
          session_cookie_present: cookies.some((c) => c.name === 'neon_v2_session'),
        });
        await context.storageState({ path: path.join(protectedDir, 'browser-auth.private.json') });
        await save();
        return response;
      }
      if (response.status() !== 503 || attempt >= 2) return response;
      const body = await response.json();
      if (body.code !== 'authentication_unavailable') return response;
      authenticationObservations.push({
        path: url,
        status: 503,
        code: body.code,
        request_id: body.request_id,
        attempt,
      });
      await save();
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
  };
  const metadata = async () => {
    const response = await read(fn);
    expect(response.status()).toBe(200);
    return response.json();
  };
  const waitRetired = async () => {
    await expect
      .poll(
        async () => {
          if ((await metadata()).state !== 'suspended') return false;
          const response = await read(`/api/v1/projects/${f.project_id}/operations`);
          expect(response.status()).toBe(200);
          const op = (await response.json()).items.find(
            (v: { resource_id: string; action: string }) =>
              v.resource_id === functionID && v.action === 'suspend_function',
          );
          if (op?.state === 'failed') throw Error(`Original retirement failed ${op.id}`);
          if (op?.state !== 'succeeded') return false;
          if (!operations.includes(op.id)) operations.push(op.id);
          return true;
        },
        { timeout: 210000, intervals: [2500, 5000] },
      )
      .toBe(true);
  };
  const wait = async (response: APIResponse | Response) => {
    expect(response.status()).toBe(202);
    const accepted = await response.json();
    operations.push(accepted.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const reply = await read(
            `/api/v1/projects/${f.project_id}/operations/${accepted.operation.id}`,
          );
          expect(reply.status()).toBe(200);
          const op = await reply.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw Error(`Operation ${op.id}: ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
  };
  const open = async (p = page) => {
    await p.goto(`/#/projects/${f.project_id}/functions`);
    const refreshed = p.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'GET',
    );
    await p.getByRole('button', { name: '刷新函数状态', exact: true }).click();
    expect((await refreshed).status()).toBe(200);
    await p.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
    return p.getByRole('region', { name: '定时触发器', exact: true });
  };
  const allCalls = async () => {
    const reply = await read(`${triggers}/${triggerID}/invocations?limit=20`);
    expect(reply.status()).toBe(200);
    return (await reply.json()).items as {
      id: string;
      state: string;
      application_status: number;
      request_event_id: string;
      error_code: string | null;
    }[];
  };
  const calls = async () =>
    (await allCalls()).filter(
      (v) => v.id !== resume?.failed_invocation_id && !resume?.prior_invocation_ids?.includes(v.id),
    );
  try {
    await save();
    page.on('dialog', (d) => void d.accept());
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await context.storageState({ path: path.join(protectedDir, 'browser-auth.private.json') });
    let response: Promise<Response>;
    if (resume) {
      const current = await metadata();
      expect(current.id).toBe(functionID);
      expect(current.slug).toBe(slug);
      expect(current.branch_id).toBe(f.branch_id);
      const original = await read(
        `/api/v1/projects/${f.project_id}/operations/${resume.operations[1]}`,
      );
      expect(original.status()).toBe(200);
      const operation = await original.json();
      expect(operation.resource_id).toBe(functionID);
      expect(operation.action).toBe('deploy_function');
      expect(operation.state).toBe('succeeded');
      await save('resumed_exact_original_successful_deployment_without_new_function_or_bundle');
    } else {
      await page.goto(`/#/projects/${f.project_id}/functions`);
      await page.getByLabel('函数名称', { exact: true }).fill(slug);
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
      functionID = (await wait(await response)).function_id;
      await save('ui_native_definition_in_retained_owner_project');
      await open();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page.getByLabel('环境变量 JSON', { exact: true }).fill('{}');
      response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === fn + '/deployments' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '部署函数版本', exact: true }).click();
      await wait(await response);
    }
    await open();
    if (resume?.corrected_bundle_required) {
      const ids = resume.prior_invocation_ids;
      if (
        !ids ||
        ids.length !== 2 ||
        new Set(ids).size !== 2 ||
        !ids.every((id) => /^fti_[a-f0-9]{64}$/.test(id)) ||
        !ids.includes(resume.failed_invocation_id || '')
      )
        throw Error('Explicit original failed and old transport-only receipt IDs required');
      const snapshot = (await allCalls()).filter((v) => ids.includes(v.id));
      expect(snapshot).toHaveLength(2);
      expect(snapshot.every((v) => ['failed', 'succeeded'].includes(v.state))).toBe(true);
      priorReceipts = snapshot;
      originalFailedReceipt = (await allCalls()).find((v) => v.id === resume.failed_invocation_id);
      if ((await metadata()).state !== 'suspended') {
        response = page.waitForResponse(
          (r) => new URL(r.url()).pathname === fn + '/suspend' && r.request().method() === 'POST',
        );
        await page.getByRole('button', { name: '停止函数实例', exact: true }).click();
        await wait(await response);
        await save(
          'ui_original_unhealthy_acceptance_guest_explicit_normal_retirement_before_bundle_correction',
        );
      }
      await waitRetired();
      await open();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page.getByLabel('环境变量 JSON', { exact: true }).fill('{}');
      response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === fn + '/deployments' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '部署函数版本', exact: true }).click();
      await wait(await response);
      await waitRetired();
      await save(
        'ui_same_function_explicit_corrected_schedule_bundle_old_deployments_and_transport_receipts_retained',
      );
    } else if (resume?.failed_invocation_id) {
      if (
        !/^ftr_[a-f0-9]{24}$/.test(triggerID) ||
        !/^fti_[a-f0-9]{64}$/.test(resume.failed_invocation_id) ||
        !/^op_[a-f0-9]{16,24}$/.test(resume.recovery_stop_operation_id || '')
      )
        throw Error('Exact recovery identities required');
      const failed = (await allCalls()).find((v) => v.id === resume.failed_invocation_id);
      expect(failed?.state).toBe('failed');
      expect(failed?.error_code).toBe('target_not_ready');
      expect(failed?.request_event_id).toBeNull();
      originalFailedReceipt = failed;
      const stopped = await read(
        `/api/v1/projects/${f.project_id}/operations/${resume.recovery_stop_operation_id}`,
      );
      expect(stopped.status()).toBe(200);
      const retired = await stopped.json();
      expect(retired.state).toBe('succeeded');
      expect(retired.resource_id).toBe(functionID);
      expect(retired.action).toBe('suspend_function');
      expect((await metadata()).state).toBe('suspended');
      response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === fn + '/resume' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '唤醒保留版本', exact: true }).click();
      await wait(await response);
      await waitRetired();
      expect((await metadata()).idle_suspended).toBe(true);
      await save(
        'ui_same_function_retained_source_new_boot_and_automatic_retirement_original_failure_immutable',
      );
    } else if (resume?.retired_operation_id) {
      if (!/^op_[a-f0-9]{16,24}$/.test(resume.retired_operation_id))
        throw Error('Original retirement identity required');
      const original = await read(
        `/api/v1/projects/${f.project_id}/operations/${resume.retired_operation_id}`,
      );
      expect(original.status()).toBe(200);
      const operation = await original.json();
      expect(operation.resource_id).toBe(functionID);
      expect(operation.action).toBe('suspend_function');
      expect(operation.state).toBe('succeeded');
      const current = await metadata();
      expect(current.state).toBe('suspended');
      expect(current.idle_suspended).toBe(true);
      expect(current.public_enabled).toBe(true);
      expect(current.idle_enabled).toBe(true);
      operations.push(operation.id);
      await save('retained_original_completed_automatic_retirement_confirmed_without_redeploy');
    } else {
      await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
      await page.getByLabel('自动休眠唤醒', { exact: true }).check();
      await page.getByLabel('函数空闲秒数', { exact: true }).fill('30');
      await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
      response = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === fn + '/http-policy' && r.request().method() === 'PATCH',
      );
      await page.getByRole('button', { name: '保存入口策略', exact: true }).click();
      expect((await response).status()).toBe(200);
      await waitRetired();
      expect((await metadata()).idle_suspended).toBe(true);
    }
    const current = await metadata(),
      versions = await read(fn + '/deployments');
    expect(versions.status()).toBe(200);
    const deployed = (await versions.json()).items.find(
      (v: { id: string }) => v.id === current.active_deployment_id,
    );
    expect(deployed?.bundle_digest).toBe(bundleDigest);
    await save(
      'actual_published_bundle_digest_matches_exact_sealed_schedule_entry_before_dispatch',
    );
    // The retained fixture deliberately has automatic Compute idle disabled.
    // Establish the zero precondition through UI; keep its policy unchanged.
    await page.goto(`/#/projects/${f.project_id}/compute`);
    const writer = await read(`/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}`);
    expect(writer.status()).toBe(200);
    const before = await writer.json();
    computeIdlePolicy = {
      scale_to_zero: before.scale_to_zero,
      idle_timeout_seconds: before.idle_timeout_seconds,
    };
    if (before.runtime.observed_state === 'active') {
      response = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname ===
            `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}/suspend` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await response);
    }
    const zero = await read(`/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}`);
    expect((await zero.json()).runtime.observed_state).toBe('suspended');
    await save('new_guest_deployed_then_function_and_compute_zero_before_schedule');
    let panel = await open();
    if (resume?.trigger_id) {
      const retained = await read(triggers + '/' + triggerID);
      expect(retained.status()).toBe(200);
      const config = (await retained.json()).trigger;
      expect(config.enabled).toBe(false);
      expect(config.name).toBe(name);
      expect(config.function_slug).toBe(slug);
      expect(await calls()).toHaveLength(0);
      await panel.getByRole('button', { name: '管理计划 ' + name, exact: true }).click();
      response = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === triggers + '/' + triggerID &&
          r.request().method() === 'PATCH',
      );
      await panel.getByRole('button', { name: '保存并启用计划', exact: true }).click();
      expect((await response).status()).toBe(200);
      await save(
        'ui_reenabled_exact_paused_plan_after_explicit_original_uid_recovery_without_fixture_replacement',
      );
    } else {
      await panel.getByLabel('定时计划名称', { exact: true }).fill(name);
      await panel.getByLabel('UTC Cron', { exact: true }).fill('* * * * *');
      await panel.getByLabel('定时函数路径', { exact: true }).fill('/scheduled');
      let lost = false;
      await context.route('**/functions-triggers', async (route) => {
        if (
          !lost &&
          route.request().method() === 'POST' &&
          new URL(route.request().url()).pathname === triggers
        ) {
          const reply = await route.fetch();
          expect(reply.status()).toBe(201);
          const headers = await route.request().allHeaders(),
            data = await reply.json();
          triggerID = data.trigger.trigger_id;
          original = {
            key: headers['idempotency-key'],
            body: route.request().postData()!,
            receipt: data,
          };
          lost = true;
          await save();
          await route.abort('failed');
          return;
        }
        await route.continue();
      });
      await panel.getByRole('button', { name: '保存暂停计划', exact: true }).click();
      await expect(
        panel.getByRole('button', { name: '重试原定时请求', exact: true }),
      ).toBeVisible();
      response = page.waitForResponse(
        (r) => new URL(r.url()).pathname === triggers && r.request().method() === 'POST',
      );
      await panel.getByRole('button', { name: '重试原定时请求', exact: true }).click();
      expect((await response).status()).toBe(201);
      expect(original).not.toBeNull();
      await expect(
        panel.getByRole('button', { name: '管理计划 ' + name, exact: true }),
      ).toBeVisible();
      expect((await metadata()).state).toBe('suspended');
      await save('lost_committed_create_replayed_original_key_and_input_without_wake_or_duplicate');
      await panel.getByRole('button', { name: '管理计划 ' + name, exact: true }).click();
      const stalePage = await context.newPage(),
        stale = await open(stalePage);
      await stale.getByRole('button', { name: '管理计划 ' + name, exact: true }).click();
      response = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === triggers + '/' + triggerID &&
          r.request().method() === 'PATCH',
      );
      await panel.getByRole('button', { name: '保存并启用计划', exact: true }).click();
      expect((await response).status()).toBe(200);
      const rejected = stalePage.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === triggers + '/' + triggerID &&
          r.request().method() === 'PATCH',
      );
      await stale.getByRole('button', { name: '保存并暂停计划', exact: true }).click();
      expect((await rejected).status()).toBe(412);
      await stalePage.close();
      await save('ui_enabled_utc_plan_and_stale_second_console_cannot_override_generation');
    }
    await expect
      .poll(
        async () => {
          const items = await calls();
          return items.length;
        },
        { timeout: 95000, intervals: [1000, 2000] },
      )
      .toBe(1);
    // Stop future queue admission immediately. The first immutable occurrence
    // must continue through cold wake despite this later configuration change.
    panel = await open();
    await panel.getByRole('button', { name: '管理计划 ' + name, exact: true }).click();
    response = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === triggers + '/' + triggerID &&
        r.request().method() === 'PATCH',
    );
    await panel.getByRole('button', { name: '保存并暂停计划', exact: true }).click();
    expect((await response).status()).toBe(200);
    await expect
      .poll(
        async () => {
          const item = (await calls())[0];
          if (['failed', 'outcome_unknown', 'skipped'].includes(item.state))
            throw Error(`Trigger ${item.id}: ${item.state}/${item.error_code}`);
          return item.state;
        },
        { timeout: 300000, intervals: [2000, 4000] },
      )
      .toBe('succeeded');
    const runs = await calls();
    expect(runs).toHaveLength(1);
    expect(runs[0].application_status).toBe(200);
    expect(runs[0].id).toMatch(/^fti_[a-f0-9]{64}$/);
    expect(runs[0].request_event_id).toMatch(/^fre_[a-f0-9]{24}$/);
    invocationID = runs[0].id;
    requestIDs.push(invocationID);
    publicURL = (await metadata()).invocation_url;
    const receipt = await context.request.get(publicURL + '/receipts?trigger=' + triggerID);
    expect(receipt.status()).toBe(200);
    expect((await receipt.json()).receipts).toEqual([triggerID + ':' + invocationID]);
    await save(
      'actual_minute_controller_woke_zero_function_and_compute_and_committed_one_restricted_sql_receipt',
    );
    const spoof = await context.request.post(publicURL + '/scheduled', {
      headers: { 'X-Neon-Trigger-Invocation-Id': invocationID },
      data: {
        version: 1,
        invocation_id: invocationID,
        trigger: { type: 'schedule', id: triggerID, name },
        data: { scheduled_at: new Date().toISOString() },
      },
    });
    expect(spoof.status()).toBe(403);
    const proof = await context.request.get(publicURL + '/receipts?trigger=' + triggerID);
    expect((await proof.json()).receipts).toEqual([triggerID + ':' + invocationID]);
    const logs = await context.request.get(fn + '/request-logs?limit=20');
    expect(logs.status()).toBe(200);
    expect(
      (await logs.json()).items.filter(
        (v: { request_id: string; channel: string }) =>
          v.request_id === invocationID && v.channel === 'schedule',
      ),
    ).toHaveLength(1);
    await save(
      'ordinary_public_caller_cannot_spoof_platform_trigger_header_and_journal_matches_original_id',
    );
    panel = await open();
    await panel.getByRole('button', { name: '管理计划 ' + name, exact: true }).click();
    await panel.getByRole('button', { name: '刷新定时调用', exact: true }).click();
    await expect(
      panel
        .getByRole('row')
        .filter({ hasText: invocationID })
        .getByRole('cell', { name: 'succeeded', exact: true }),
    ).toBeVisible();
    await expect(panel.getByText(invocationID, { exact: true })).toBeVisible();
    response = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === triggers + '/' + triggerID &&
        r.request().method() === 'DELETE',
    );
    await panel.getByRole('button', { name: '删除定时计划', exact: true }).click();
    expect((await response).status()).toBe(204);
    expect(await calls()).toHaveLength(1);
    if (resume?.prior_invocation_ids)
      expect((await allCalls()).filter((v) => resume.prior_invocation_ids!.includes(v.id))).toEqual(
        priorReceipts,
      );
    if (resume?.failed_invocation_id)
      expect((await allCalls()).find((v) => v.id === resume.failed_invocation_id)).toEqual(
        originalFailedReceipt,
      );
    await save('ui_execution_receipt_and_deleted_plan_retains_exact_occurrence_history');
    await waitRetired();
    await open();
    response = page.waitForResponse(
      (r) => new URL(r.url()).pathname === fn && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '删除函数', exact: true }).click();
    await wait(await response);
    await page.goto(`/#/projects/${f.project_id}/compute`);
    const endpoint = await context.request.get(
      `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}`,
    );
    expect(endpoint.status()).toBe(200);
    if ((await endpoint.json()).runtime.observed_state === 'active') {
      response = page.waitForResponse(
        (r) => new URL(r.url()).pathname.endsWith('/suspend') && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await response);
    }
    await save(
      'owned_function_retired_by_ui_original_source_environment_history_and_sql_bytes_retained_compute_zero',
    );
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
