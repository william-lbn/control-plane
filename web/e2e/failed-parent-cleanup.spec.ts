import { expect, test } from '@playwright/test';
import { createHash, randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

test('Failed parent creation: exact native failure, retained cleanup and original replay', async ({
  page,
  context,
  baseURL,
}, info) => {
  test.setTimeout(720000);
  const file = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !file || !admin || !privateDir || !baseURL)
    throw new Error('Protected Linux scope and independent native operator required');
  const fixture = JSON.parse(await readFile(file, 'utf8'));
  const kind = (fixture.failed_parent_kind ?? process.env.NEON_E2E_FAILED_PARENT_KIND) as
      | 'project'
      | 'branch',
    ownerProject = fixture.project_id as string,
    ownerBranch = fixture.branch_id as string,
    writer = fixture.writer_id as string;
  if (
    !['project', 'branch'].includes(kind) ||
    !/^prj_[a-f0-9]{16}$/.test(ownerProject) ||
    !/^br_[a-f0-9]{16}$/.test(ownerBranch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw new Error('Reviewed original scope required');
  const resume = fixture.failed_endpoint_resume;
  if (
    resume &&
    (resume.resource_kind !== kind ||
      !resume.checks?.includes('original_native_create_failed_guard_removed') ||
      !/^publication-ui-[0-9]{14}$/.test(resume.original_failure_job))
  )
    throw new Error('Reviewed original parent failure checkpoint required');
  let project: string = resume?.project_id ?? ownerProject,
    branch: string = resume?.branch_id ?? '',
    endpoint: string = resume?.endpoint_id ?? '',
    origin: string = resume?.original_create_operation_id ?? '',
    cleanup: string = resume?.cleanup_operation_id ?? '',
    requestKey: string = resume?.cleanup_request_key ?? '',
    originalHash: string = resume?.original_receipt_hash ?? '',
    resourceName: string = resume?.resource_name ?? '',
    version: number = resume?.resource_version ?? 0,
    result = 'running';
  const checks: string[] = [...(resume?.checks ?? [])],
    base = () => `/api/v1/projects/${project}`,
    ownerBase = `/api/v1/projects/${ownerProject}`;
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    const value = {
      result,
      resource_kind: kind,
      project_id: project,
      branch_id: branch,
      endpoint_id: endpoint,
      original_create_operation_id: origin,
      cleanup_operation_id: cleanup,
      resource_name: resourceName,
      resource_version: version,
      original_receipt_hash: originalHash,
      checks,
      owner_project_id: ownerProject,
      owner_branch_id: ownerBranch,
      writer_id: writer,
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
      privateDir + '/failed-endpoint-checkpoint.json',
      JSON.stringify({ ...value, cleanup_request_key: requestKey }) + '\n',
      { mode: 0o600 },
    );
  }
  async function marker(name: string, value: unknown) {
    await writeFile(privateDir + '/' + name, JSON.stringify(value) + '\n', {
      mode: 0o600,
      flag: 'wx',
    });
  }
  async function waitMarker(name: string) {
    const deadline = Date.now() + 200000;
    while (Date.now() < deadline) {
      try {
        return JSON.parse(await readFile(privateDir + '/' + name, 'utf8'));
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
      }
      await new Promise((resolve) => setTimeout(resolve, 500));
    }
    throw new Error('Original operator marker deadline: ' + name);
  }
  async function get(url: string) {
    const response = await context.request.get(url);
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function operation(id: string, want: string) {
    await expect
      .poll(
        async () => {
          const value = await get(base() + '/operations/' + id);
          if (['failed', 'succeeded', 'cancelled'].includes(value.state) && value.state !== want)
            throw new Error(`Original Operation ${id} ${value.state} ${value.error_code}`);
          return value.state;
        },
        { timeout: 240000, intervals: [1000, 2500] },
      )
      .toBe(want);
    return get(base() + '/operations/' + id);
  }
  function receiptHash(value: Record<string, unknown>) {
    return createHash('sha256')
      .update(
        JSON.stringify(
          Object.fromEntries(
            ['id', 'action', 'resource_id', 'state', 'attempts', 'finished_at', 'error_code'].map(
              (key) => [key, value[key]],
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
    expect((await get(ownerBase + '/endpoints/' + writer)).observed_state).toBe('suspended');
    await marker('failed-endpoint-prepare.json', {
      project_id: ownerProject,
      branch_id: ownerBranch,
      writer_id: writer,
    });
    if (!resume) {
      const paused = await waitMarker('failed-endpoint-worker-paused.json');
      expect(paused.project_id).toBe(ownerProject);
      await save('trusted_original_scope_ready');
      resourceName = `ci-failed-${kind}-${process.env.NEON_E2E_ATTEMPT || Date.now()}`;
      if (kind === 'project') {
        await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
        await page.getByLabel('项目名称').fill(resourceName);
      } else {
        await page.goto(`/#/projects/${ownerProject}/branches`);
        await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
        await page.getByLabel('分支名称').fill(resourceName);
        await page.getByLabel('父分支').selectOption(ownerBranch);
        await page.getByLabel('同时创建读写 Compute Endpoint').check();
      }
      const password = randomBytes(30).toString('base64url');
      await writeFile(privateDir + '/failed-parent-database-password', password, {
        mode: 0o600,
        flag: 'wx',
      });
      await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
      await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
      await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
      const url =
        kind === 'project' ? '/api/v1/organizations/local/projects' : ownerBase + '/branches';
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建并验证', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(202);
      const accepted = await response.json();
      origin = accepted.operation.id;
      if (kind === 'project') {
        project = accepted.resource.id;
        branch = accepted.resource.default_branch_id;
      } else branch = accepted.resource.id;
      await save();
      expect(accepted.operation.action).toBe('create_' + kind);
      expect(/^prj_[a-f0-9]{16}$/.test(project)).toBe(true);
      expect(/^br_[a-f0-9]{16}$/.test(branch)).toBe(true);
      const endpoints = (await get(base() + '/endpoints')).items.filter(
        (value: { branch_id: string }) => value.branch_id === branch,
      );
      expect(endpoints).toHaveLength(1);
      endpoint = endpoints[0].id;
      await save('original_parent_create_accepted_while_worker_paused');
      await marker('failed-endpoint-queued.json', {
        project_id: project,
        branch_id: branch,
        endpoint_id: endpoint,
        operation_id: origin,
      });
    }
    const failed = await operation(origin, 'failed');
    expect(failed.action).toBe('create_' + kind);
    expect(failed.resource_id).toBe(kind === 'project' ? project : branch);
    const proof = await waitMarker('failed-endpoint-original-failed.json');
    expect(proof.endpoint_id).toBe(endpoint);
    expect(proof.native_admission_fault_removed).toBe(true);
    if (originalHash) expect(receiptHash(failed)).toBe(originalHash);
    else originalHash = receiptHash(failed);
    await save('original_native_create_failed_guard_removed');
    if (!cleanup) {
      const snapshot = await get(base() + '/lifecycle'),
        resource =
          kind === 'project'
            ? snapshot.project
            : snapshot.branches.find((value: { id: string }) => value.id === branch);
      expect(resource.state).toBe('error');
      expect(resource.name).toBe(resourceName);
      version = resource.version;
      await page.goto(`/#/projects/${project}/lifecycle`);
      await page
        .getByTestId(kind === 'project' ? 'project-lifecycle' : 'lifecycle-' + branch)
        .getByRole('button', {
          name: kind === 'project' ? '清理失败项目' : '清理失败分支',
          exact: true,
        })
        .click();
      await expect(page.getByRole('dialog', { name: '确认生命周期操作' })).toBeVisible();
      await page.getByLabel('资源名称确认').fill(resourceName);
      const url = base() + (kind === 'project' ? '' : '/branches/' + branch);
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
        cleanup = value.operation.id;
        await save('lost_reply_original_cleanup_id_recorded');
        expect(value.operation.action).toBe('cleanup_failed_' + kind);
        await route.abort('failed');
      });
      await page.getByRole('button', { name: '确认执行', exact: true }).click();
      await expect(page.getByRole('alert')).toBeVisible();
      await page.unroute('**' + url);
      const replay = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'DELETE',
      );
      await page.getByRole('button', { name: '确认执行', exact: true }).click();
      const recovered = await replay;
      expect(recovered.status()).toBe(202);
      expect(recovered.request().headers()['idempotency-key']).toBe(requestKey);
      expect((await recovered.json()).operation.id).toBe(cleanup);
      await save('same_key_replay_same_cleanup_operation');
    }
    const finished = await operation(cleanup, 'succeeded');
    expect(finished.action).toBe('cleanup_failed_' + kind);
    expect(finished.steps).toHaveLength(5);
    expect(finished.steps.every((step: { state: string }) => step.state === 'succeeded')).toBe(
      true,
    );
    expect(receiptHash(await get(base() + '/operations/' + origin))).toBe(originalHash);
    const csrf = (await context.cookies(baseURL)).find(
      (cookie) => cookie.name === 'neon_v2_csrf',
    )?.value;
    if (!csrf) throw new Error('CSRF cookie required');
    expect(
      (
        await context.request.post(base() + '/operations/' + origin + '/retry', {
          headers: { 'X-CSRF-Token': csrf, Origin: new URL(baseURL).origin },
        })
      ).status(),
    ).toBe(409);
    expect(receiptHash(await get(base() + '/operations/' + origin))).toBe(originalHash);
    await save('cleanup_succeeded_original_failure_preserved_and_retry_denied');
    const snapshot = await get(base() + '/lifecycle');
    expect(snapshot.branches.find((value: { id: string }) => value.id === branch).state).toBe(
      'deleted',
    );
    if (kind === 'project') {
      expect(snapshot.project.state).toBe('deleted');
      expect(snapshot.project.recover_until).toBeNull();
    } else expect(snapshot.project.state).toBe('ready');
    const tombstone = snapshot.tombstones.find(
      (value: { operation_id: string }) => value.operation_id === cleanup,
    );
    expect(tombstone.resource_type).toBe(kind);
    expect(tombstone.physical_gc_state).toBe('held');
    expect((await get(ownerBase + '/branches/' + ownerBranch)).state).toBe('ready');
    const retained = await get(ownerBase + '/endpoints/' + writer);
    expect(retained.state).toBe('active');
    expect(retained.observed_state).toBe('suspended');
    await marker('failed-endpoint-cleanup-finished.json', {
      endpoint_id: endpoint,
      cleanup_operation_id: cleanup,
    });
    await save('original_owner_branch_writer_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
