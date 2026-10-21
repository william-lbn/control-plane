import { expect, test } from '@playwright/test';
import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

test('Failed independent Compute: native create failure, original cleanup replay and retained parents', async ({
  page,
  context,
  baseURL,
}, info) => {
  test.setTimeout(720000);
  const file = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !file || !admin || !privateDir || !baseURL)
    throw new Error('Protected Linux scope and separate native operator required');
  const root = privateDir,
    fixture = JSON.parse(await readFile(file, 'utf8')) as {
      project_id: string;
      branch_id: string;
      writer_id: string;
      failed_endpoint_resume?: {
        endpoint_id: string;
        original_create_operation_id: string;
        cleanup_operation_id: string;
        cleanup_request_key: string;
        endpoint_version: number;
        checks: string[];
        original_receipt_hash: string;
        original_failure_job: string;
      };
    };
  const project = fixture.project_id,
    branch = fixture.branch_id,
    writer = fixture.writer_id,
    resume = fixture.failed_endpoint_resume;
  for (const [value, prefix] of [
    [project, 'prj'],
    [branch, 'br'],
    [writer, 'ep'],
  ])
    if (!new RegExp('^' + prefix + '_[a-f0-9]{16}$').test(value))
      throw new Error('Original retained scope required');
  if (
    resume &&
    (!/^ep_[a-f0-9]{16}$/.test(resume.endpoint_id) ||
      !/^op_[a-f0-9]{24}$/.test(resume.original_create_operation_id) ||
      !resume.checks.includes('original_native_create_failed_guard_removed') ||
      !/^publication-ui-[0-9]{14}$/.test(resume.original_failure_job))
  )
    throw new Error('Reviewed original native failure checkpoint required');
  const base = `/api/v1/projects/${project}`,
    checks = [...(resume?.checks ?? [])];
  let result = 'running',
    endpoint = resume?.endpoint_id ?? '',
    origin = resume?.original_create_operation_id ?? '',
    cleanup = resume?.cleanup_operation_id ?? '',
    requestKey = resume?.cleanup_request_key ?? '',
    version = resume?.endpoint_version ?? 0,
    originalHash = resume?.original_receipt_hash ?? '';
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    const value = {
      result,
      project_id: project,
      branch_id: branch,
      writer_id: writer,
      endpoint_id: endpoint,
      original_create_operation_id: origin,
      cleanup_operation_id: cleanup,
      endpoint_version: version,
      checks,
      original_receipt_hash: originalHash,
      cleanup_request_key_sha256: requestKey
        ? createHash('sha256').update(requestKey).digest('hex')
        : '',
      data_retained: true,
      credentials_in_evidence: false,
      production_qualified: false,
      ...(resume ? { original_failure_job: resume.original_failure_job } : {}),
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(value) + '\n');
    await writeFile(
      root + '/failed-endpoint-checkpoint.json',
      JSON.stringify({ ...value, cleanup_request_key: requestKey }) + '\n',
      { mode: 0o600 },
    );
  }
  async function marker(name: string, value: unknown) {
    await writeFile(root + '/' + name, JSON.stringify(value) + '\n', { mode: 0o600, flag: 'wx' });
  }
  async function waitMarker(name: string) {
    const deadline = Date.now() + 200000;
    while (Date.now() < deadline) {
      try {
        return JSON.parse(await readFile(root + '/' + name, 'utf8'));
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
    throw new Error('Original operator marker deadline: ' + name);
  }
  async function get(url: string) {
    const r = await context.request.get(url);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function operation(id: string, want: string) {
    await expect
      .poll(
        async () => {
          const v = await get(base + '/operations/' + id);
          if (['failed', 'succeeded', 'cancelled'].includes(v.state) && v.state !== want)
            throw new Error(`Original operation ${id} ${v.state} ${v.error_code}`);
          return v.state;
        },
        { timeout: 240000, intervals: [1000, 2500] },
      )
      .toBe(want);
    return get(base + '/operations/' + id);
  }
  function receiptHash(value: Record<string, unknown>) {
    return createHash('sha256')
      .update(
        JSON.stringify(
          Object.fromEntries(
            ['id', 'action', 'resource_id', 'state', 'attempts', 'finished_at', 'error_code'].map(
              (k) => [k, value[k]],
            ),
          ),
        ),
      )
      .digest('hex');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const originalWriter = await get(base + '/endpoints/' + writer);
    expect(originalWriter.branch_id).toBe(branch);
    expect(originalWriter.observed_state).toBe('suspended');
    await marker('failed-endpoint-prepare.json', {
      project_id: project,
      branch_id: branch,
      writer_id: writer,
    });
    if (!resume) {
      const paused = await waitMarker('failed-endpoint-worker-paused.json');
      expect(paused.project_id).toBe(project);
      await save('trusted_original_scope_ready');
      await page.goto(`/#/projects/${project}/branches/${branch}`);
      await page.getByRole('button', { name: /^＋ (添加 Compute|创建 Endpoint)$/ }).click();
      await page.getByLabel('Compute 类型').selectOption('read_only');
      await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
      await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/endpoints' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建并验证', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(202);
      const accepted = await response.json();
      endpoint = accepted.resource.id;
      origin = accepted.operation.id;
      // Persist the accepted identities before assertions or operator handoff.
      // An interrupted harness must be able to recover the original resource.
      await save();
      expect(/^ep_[a-f0-9]{16}$/.test(endpoint)).toBe(true);
      expect(/^op_[a-f0-9]{24}$/.test(origin)).toBe(true);
      expect(accepted.operation.resource_id).toBe(endpoint);
      await save('original_reader_create_accepted_while_worker_paused');
      await marker('failed-endpoint-queued.json', {
        project_id: project,
        branch_id: branch,
        endpoint_id: endpoint,
        operation_id: origin,
      });
    }
    const failed = await operation(origin, 'failed');
    expect(failed.action).toBe('create_endpoint');
    expect(failed.resource_id).toBe(endpoint);
    const proof = await waitMarker('failed-endpoint-original-failed.json');
    expect(proof.endpoint_id).toBe(endpoint);
    expect(proof.native_admission_fault_removed).toBe(true);
    if (originalHash) expect(receiptHash(failed)).toBe(originalHash);
    else originalHash = receiptHash(failed);
    await save('original_native_create_failed_guard_removed');
    if (!cleanup) {
      await page.goto(`/#/projects/${project}/compute`);
      await page.locator('select').first().selectOption(endpoint);
      const current = await get(base + '/endpoints/' + endpoint);
      expect(current.state).toBe('error');
      version = current.version;
      await page.getByRole('button', { name: '删除此 Compute', exact: true }).click();
      await expect(page.getByRole('dialog', { name: '删除 Compute Endpoint' })).toBeVisible();
      await page.getByLabel('Endpoint Selector 确认').fill(current.selector);
      const url = base + '/endpoints/' + endpoint;
      await page.route('**' + url, async (route) => {
        if (route.request().method() !== 'DELETE') {
          await route.continue();
          return;
        }
        requestKey = route.request().headers()['idempotency-key'];
        expect(requestKey).toBeTruthy();
        const accepted = await route.fetch();
        expect(accepted.status()).toBe(202);
        const value = await accepted.json();
        expect(value.operation.action).toBe('cleanup_failed_endpoint');
        cleanup = value.operation.id;
        await save('lost_reply_original_cleanup_id_recorded');
        await route.abort('failed');
      });
      await page.getByRole('button', { name: '确认删除 Compute', exact: true }).click();
      await expect(page.getByRole('alert')).toBeVisible();
      await page.unroute('**' + url);
      const replay = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'DELETE',
      );
      await page.getByRole('button', { name: '确认删除 Compute', exact: true }).click();
      const recovered = await replay;
      expect(recovered.status()).toBe(202);
      expect(recovered.request().headers()['idempotency-key']).toBe(requestKey);
      expect((await recovered.json()).operation.id).toBe(cleanup);
      await save('same_key_replay_same_cleanup_operation');
    }
    const finished = await operation(cleanup, 'succeeded');
    expect(finished.resource_id).toBe(endpoint);
    expect(finished.action).toBe('cleanup_failed_endpoint');
    expect((await context.request.get(base + '/endpoints/' + endpoint)).status()).toBe(404);
    expect(receiptHash(await get(base + '/operations/' + origin))).toBe(originalHash);
    const csrf = (await context.cookies(baseURL)).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('CSRF cookie required');
    const denied = await context.request.post(base + '/operations/' + origin + '/retry', {
      headers: { 'X-CSRF-Token': csrf, Origin: new URL(baseURL).origin },
    });
    expect(denied.status()).toBe(409);
    expect(receiptHash(await get(base + '/operations/' + origin))).toBe(originalHash);
    await save('cleanup_succeeded_original_failure_preserved_and_retry_denied');
    expect((await get(base + '/branches/' + branch)).state).toBe('ready');
    const retained = await get(base + '/endpoints/' + writer);
    expect(retained.id).toBe(writer);
    expect(retained.branch_id).toBe(branch);
    expect(retained.state).toBe('active');
    expect(retained.observed_state).toBe('suspended');
    await marker('failed-endpoint-cleanup-finished.json', {
      endpoint_id: endpoint,
      cleanup_operation_id: cleanup,
    });
    await save('original_branch_writer_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
