import { expect, test, type APIResponse, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('UI preserves original successful schedule and SQL evidence then retires its exact owned fixture without re-dispatch', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!file || !admin)
    throw Error('Protected original owner and prior acceptance receipt required');
  const f = JSON.parse(await readFile(file, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    schedule_resume: {
      function_id: string;
      slug: string;
      name: string;
      trigger_id: string;
      failed_invocation_id: string;
      accepted: {
        job: string;
        result: string;
        checks: string[];
        operations: string[];
        project_id: string;
        branch_id: string;
        function_id: string;
        trigger_id: string;
        invocation_id: string;
        acceptance_bundle_digest: string;
      };
    };
  };
  const r = f.schedule_resume,
    a = r.accepted;
  if (
    !/^prj_[a-f0-9]{16}$/.test(f.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(f.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(f.writer_id) ||
    !/^fnc_[a-f0-9]{16}$/.test(r.function_id) ||
    !/^ftr_[a-f0-9]{24}$/.test(r.trigger_id) ||
    !/^fti_[a-f0-9]{64}$/.test(a.invocation_id) ||
    !/^publication-ui-[0-9]{14}$/.test(a.job) ||
    a.result !== 'fail' ||
    a.project_id !== f.project_id ||
    a.branch_id !== f.branch_id ||
    a.function_id !== r.function_id ||
    a.trigger_id !== r.trigger_id ||
    !a.checks.includes(
      'actual_minute_controller_woke_zero_function_and_compute_and_committed_one_restricted_sql_receipt',
    ) ||
    !a.checks.includes(
      'ordinary_public_caller_cannot_spoof_platform_trigger_header_and_journal_matches_original_id',
    ) ||
    !/^[a-f0-9]{64}$/.test(a.acceptance_bundle_digest)
  )
    throw Error(
      'Exact original completed business acceptance and failed final-observation checkpoint required',
    );
  const base = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}/functions`,
    fn = base + '/' + r.slug,
    triggers = base.replace(/\/functions$/, '/functions-triggers'),
    endpoint = `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}`;
  let result = 'running';
  const checks: string[] = [],
    operations: string[] = [];
  const save = async (check?: string) => {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        project_id: f.project_id,
        branch_id: f.branch_id,
        function_id: r.function_id,
        trigger_id: r.trigger_id,
        invocation_id: a.invocation_id,
        acceptance_bundle_digest: a.acceptance_bundle_digest,
        continued_from: a.job,
        original_result: a.result,
        prior_business_checks: a.checks,
        customer_requests_sent: 0,
        new_definitions_created: 0,
        new_deployments_created: 0,
        production_qualified: false,
      }) + '\n',
    );
  };
  const get = async (url: string) => {
    const response = await context.request.get(url);
    expect(response.status()).toBe(200);
    return response.json();
  };
  const journal = async () =>
    (await get(triggers + '/' + r.trigger_id + '/invocations?limit=20')).items;
  const open = async () => {
    await page.goto(`/#/projects/${f.project_id}/functions`);
    const refreshed = page.waitForResponse(
      (v) => new URL(v.url()).pathname === base && v.request().method() === 'GET',
    );
    await page.getByRole('button', { name: '刷新函数状态', exact: true }).click();
    expect((await refreshed).status()).toBe(200);
    await page.getByRole('button', { name: '查看函数 ' + r.slug, exact: true }).click();
    return page.getByRole('region', { name: '定时触发器', exact: true });
  };
  const wait = async (response: Response | APIResponse) => {
    expect(response.status()).toBe(202);
    const op = (await response.json()).operation;
    operations.push(op.id);
    await save();
    await expect
      .poll(
        async () => {
          const current = await get(`/api/v1/projects/${f.project_id}/operations/${op.id}`);
          expect(current.resource_id === r.function_id || current.resource_id === f.writer_id).toBe(
            true,
          );
          if (['failed', 'cancelled'].includes(current.state))
            throw Error('Original retirement operation failed ' + current.id);
          return current.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  };
  try {
    await save();
    page.on('dialog', (d) => void d.accept());
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const current = await get(fn);
    expect(current.id).toBe(r.function_id);
    expect(['active', 'suspended']).toContain(current.state);
    const versions = await get(fn + '/deployments');
    expect(
      versions.items.find((v: { id: string }) => v.id === current.active_deployment_id)
        ?.bundle_digest,
    ).toBe(a.acceptance_bundle_digest);
    const before = await journal(),
      original = before.find((v: { id: string }) => v.id === a.invocation_id);
    expect(original.state).toBe('succeeded');
    expect(original.application_status).toBe(200);
    expect(original.request_event_id).toMatch(/^fre_[a-f0-9]{24}$/);
    expect(before.find((v: { id: string }) => v.id === r.failed_invocation_id).state).toBe(
      'failed',
    );
    const plan = await get(triggers + '/' + r.trigger_id);
    expect(plan.trigger.enabled).toBe(false);
    const logs = await get(fn + '/request-logs?limit=20');
    expect(
      logs.items.filter(
        (v: { request_id: string; channel: string }) =>
          v.request_id === a.invocation_id && v.channel === 'schedule',
      ),
    ).toHaveLength(1);
    await save(
      'original_successful_occurrence_bundle_and_journal_verified_without_new_customer_call',
    );
    const panel = await open();
    await panel.getByRole('button', { name: '管理计划 ' + r.name, exact: true }).click();
    await panel.getByRole('button', { name: '刷新定时调用', exact: true }).click();
    await expect(
      panel
        .getByRole('row')
        .filter({ hasText: a.invocation_id })
        .getByRole('cell', { name: 'succeeded', exact: true }),
    ).toBeVisible();
    await save(
      'ui_exact_original_invocation_row_observed_with_other_success_and_failure_history_retained',
    );
    let response = page.waitForResponse(
      (v) =>
        new URL(v.url()).pathname === triggers + '/' + r.trigger_id &&
        v.request().method() === 'DELETE',
    );
    await panel.getByRole('button', { name: '删除定时计划', exact: true }).click();
    expect((await response).status()).toBe(204);
    expect(await journal()).toEqual(before);
    await save('ui_deleted_same_paused_plan_all_three_original_terminal_receipts_unchanged');
    await expect
      .poll(
        async () => {
          const value = await get(fn);
          if (value.state !== 'suspended' || !value.idle_suspended) return false;
          const entries = await get(`/api/v1/projects/${f.project_id}/operations`);
          const stopped = entries.items.find(
            (v: { resource_id: string; action: string }) =>
              v.resource_id === r.function_id && v.action === 'suspend_function',
          );
          if (stopped?.state === 'failed') throw Error('Original automatic retirement failed');
          if (stopped?.state !== 'succeeded') return false;
          if (!operations.includes(stopped.id)) operations.push(stopped.id);
          return true;
        },
        { timeout: 210000, intervals: [2500, 5000] },
      )
      .toBe(true);
    await save('corrected_schedule_application_automatically_retired_after_actual_call');
    await open();
    response = page.waitForResponse(
      (v) => new URL(v.url()).pathname === fn && v.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '删除函数', exact: true }).click();
    await wait(await response);
    await page.goto(`/#/projects/${f.project_id}/compute`);
    const writer = await get(endpoint);
    if (writer.runtime.observed_state === 'active') {
      response = page.waitForResponse(
        (v) =>
          new URL(v.url()).pathname === endpoint + '/suspend' && v.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await response);
    }
    const zero = await get(endpoint);
    expect(zero.runtime.observed_state).toBe('suspended');
    expect(zero.scale_to_zero).toBe(writer.scale_to_zero);
    expect(zero.idle_timeout_seconds).toBe(writer.idle_timeout_seconds);
    expect(await journal()).toEqual(before);
    await save(
      'ui_owned_function_and_compute_normally_retired_all_source_sql_and_trigger_evidence_retained',
    );
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
