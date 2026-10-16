import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('UI retires the exact failed unpublished Function instance and preserves original invocation evidence', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !adminFile)
    throw Error('Protected original recovery and admin inputs required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    schedule_resume: {
      function_id: string;
      slug: string;
      failed_deploy_operation_id: string;
      trigger_id: string;
      failed_invocation_id: string;
    };
  };
  const original = f.schedule_resume;
  if (
    !/^fnc_[a-f0-9]{16}$/.test(original.function_id) ||
    !/^[a-z0-9]{1,20}$/.test(original.slug) ||
    !/^op_[a-f0-9]{16,24}$/.test(original.failed_deploy_operation_id) ||
    !/^ftr_[a-f0-9]{24}$/.test(original.trigger_id) ||
    !/^fti_[a-f0-9]{64}$/.test(original.failed_invocation_id)
  )
    throw Error('Exact original failed ownership required');
  const fn = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}/functions/${original.slug}`,
    endpoint = `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}`,
    journal = fn.replace(
      /\/functions\/[^/]+$/,
      '/functions-triggers/' + original.trigger_id + '/invocations',
    );
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
        function_id: original.function_id,
        trigger_id: original.trigger_id,
        original_failed_invocation_id: original.failed_invocation_id,
        original_failed_operation_id: original.failed_deploy_operation_id,
        new_function_created: false,
        customer_invocations_sent: 0,
        production_qualified: false,
      }) + '\n',
    );
  };
  const wait = async (response: Response) => {
    expect(response.status()).toBe(202);
    const accepted = await response.json();
    operations.push(accepted.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${f.project_id}/operations/${accepted.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (op.state === 'failed')
            throw Error('Original retirement failed ' + op.id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 240000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  };
  try {
    await save();
    page.on('dialog', (d) => void d.accept());
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const before = await context.request.get(fn);
    expect(before.status()).toBe(200);
    const definition = await before.json();
    expect(definition.id).toBe(original.function_id);
    expect(definition.state).toBe('degraded');
    const source = definition.active_deployment_id;
    expect(source).toMatch(/^fdp_[a-f0-9]{16}$/);
    const failed = await context.request.get(
      `/api/v1/projects/${f.project_id}/operations/${original.failed_deploy_operation_id}`,
    );
    expect(failed.status()).toBe(200);
    const operation = await failed.json();
    expect(operation.resource_id).toBe(original.function_id);
    expect(operation.state).toBe('failed');
    const beforeJournal = await context.request.get(journal);
    expect(beforeJournal.status()).toBe(200);
    const receipt = (await beforeJournal.json()).items.find(
      (v: { id: string }) => v.id === original.failed_invocation_id,
    );
    expect(receipt.state).toBe('failed');
    expect(receipt.request_event_id).toBeNull();
    await save('original_failed_boot_and_undispatched_occurrence_authoritatively_identified');
    await page.goto(`/#/projects/${f.project_id}/functions`);
    await page.getByRole('button', { name: '查看函数 ' + original.slug, exact: true }).click();
    let pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === fn + '/suspend' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '停止函数实例', exact: true }).click();
    await wait(await pending);
    const stopped = await context.request.get(fn);
    expect(stopped.status()).toBe(200);
    const retained = await stopped.json();
    expect(retained.id).toBe(original.function_id);
    expect(retained.state).toBe('suspended');
    expect(retained.active_deployment_id).toBe(source);
    await save('ui_original_failed_instance_normal_uid_retirement_and_source_retained');
    await page.goto(`/#/projects/${f.project_id}/compute`);
    const writer = await context.request.get(endpoint);
    expect(writer.status()).toBe(200);
    const policy = await writer.json();
    if (policy.runtime.observed_state === 'active') {
      pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === endpoint + '/suspend' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await pending);
    }
    const zero = await context.request.get(endpoint);
    expect(zero.status()).toBe(200);
    const actual = await zero.json();
    expect(actual.runtime.observed_state).toBe('suspended');
    expect(actual.scale_to_zero).toBe(policy.scale_to_zero);
    expect(actual.idle_timeout_seconds).toBe(policy.idle_timeout_seconds);
    await save('ui_compute_zero_with_original_policy_preserved');
    const after = await context.request.get(journal);
    expect(after.status()).toBe(200);
    expect((await after.json()).items.find((v: { id: string }) => v.id === receipt.id)).toEqual(
      receipt,
    );
    await save('original_terminal_failed_receipt_preserved_without_relabel_or_customer_replay');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
