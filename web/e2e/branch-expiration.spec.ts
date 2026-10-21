import { expect, test } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

test('Branch expiration UI: set, replay, update, clear and automatic retained retirement', async ({
  page,
  context,
  baseURL,
}, info) => {
  test.setTimeout(720000);
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !fixtureFile || !admin || !privateDir || !baseURL)
    throw new Error('Protected Linux inputs required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')),
    project = fixture.project_id as string,
    parent = fixture.branch_id as string,
    writer = fixture.writer_id as string;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(parent) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw new Error('Original retained scope required');
  const resume = fixture.branch_expiration_resume;
  if (
    resume &&
    (!/^br_[a-f0-9]{16}$/.test(resume.branch_id) ||
      !/^ep_[a-f0-9]{16}$/.test(resume.endpoint_id) ||
      !/^publication-ui-[0-9]{14}$/.test(resume.original_failure_job))
  )
    throw new Error('Reviewed original TTL fixture required');
  let branch: string = resume?.branch_id ?? '',
    endpoint: string = resume?.endpoint_id ?? '',
    creation: string = resume?.create_operation_id ?? '',
    expiration: string = resume?.expiration_operation_id ?? '',
    timeline: string = resume?.timeline_id ?? '',
    branchName: string = resume?.branch_name ?? '',
    result = 'running';
  const operations: string[] = [...(resume?.operations ?? [])],
    checks: string[] = [...(resume?.checks ?? [])],
    base = `/api/v1/projects/${project}`;
  const password = resume
    ? (await readFile(privateDir + '/branch-expiration-database-password', 'utf8')).trim()
    : randomBytes(30).toString('base64url');
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    const value = {
      result,
      project_id: project,
      parent_branch_id: parent,
      writer_id: writer,
      branch_id: branch,
      endpoint_id: endpoint,
      create_operation_id: creation,
      expiration_operation_id: expiration,
      timeline_id: timeline,
      branch_name: branchName,
      operations,
      checks,
      data_retained: true,
      credentials_in_evidence: false,
      performance_reliability_campaign: false,
      production_qualified: false,
      ...(resume ? { original_failure_job: resume.original_failure_job } : {}),
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(value) + '\n');
    await writeFile(
      privateDir + '/branch-expiration-checkpoint.json',
      JSON.stringify(value) + '\n',
      { mode: 0o600 },
    );
  }
  async function get(url: string) {
    const response = await context.request.get(url);
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function operation(id: string, want = 'succeeded') {
    await expect
      .poll(
        async () => {
          const value = await get(base + '/operations/' + id);
          if (['failed', 'cancelled'].includes(value.state))
            throw new Error(
              'Original operation ' + id + ' ' + value.state + ' ' + value.error_code,
            );
          return value.state;
        },
        { timeout: 300000, intervals: [1000, 2500] },
      )
      .toBe(want);
    return get(base + '/operations/' + id);
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
    throw new Error('Original storage observer deadline: ' + name);
  }
  async function marker(name: string, value: unknown) {
    await writeFile(privateDir + '/' + name, JSON.stringify(value) + '\n', {
      mode: 0o600,
      flag: 'wx',
    });
  }
  async function sql(statement: string) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/endpoints/' + endpoint + '/query' &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function openPolicy() {
    await page.goto(`/#/projects/${project}/lifecycle`);
    await page
      .getByTestId('lifecycle-' + branch)
      .getByRole('button', { name: /^(设置到期时间|修改到期时间)$/ })
      .click();
    await expect(page.getByRole('dialog', { name: '分支到期设置' })).toBeVisible();
    await page.getByLabel('到期分支名称确认').fill(branchName);
  }
  async function policy(seconds: number | null) {
    await openPolicy();
    if (seconds === null) await page.getByLabel('取消自动到期').check();
    else
      await page
        .getByLabel('UTC 到期时间')
        .fill(new Date(Date.now() + seconds * 1000).toISOString().slice(0, 19));
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/branches/' + branch + '/expiration' &&
        r.request().method() === 'PATCH',
    );
    await page.getByRole('button', { name: '确认到期设置', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const value = await response.json();
    operations.push(value.operation.id);
    expect(value.operation.state).toBe('succeeded');
    await expect(page.getByRole('dialog', { name: '分支到期设置' })).not.toBeVisible();
    await save();
    return value;
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    expect((await get(base + '/endpoints/' + writer)).observed_state).toBe('suspended');
    if (!resume) {
      await writeFile(privateDir + '/branch-expiration-database-password', password, {
        mode: 0o600,
        flag: 'wx',
      });
      branchName = 'ci-expiry-' + (process.env.NEON_E2E_ATTEMPT || Date.now());
      await page.goto(`/#/projects/${project}/branches`);
      await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
      await page.getByLabel('分支名称').fill(branchName);
      await page.getByLabel('父分支').selectOption(parent);
      await page.getByLabel('同时创建读写 Compute Endpoint').check();
      await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
      await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
      await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base + '/branches' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建并验证', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(202);
      const accepted = await response.json();
      branch = accepted.resource.id;
      creation = accepted.operation.id;
      operations.push(creation);
      await save();
      await operation(creation);
      const current = await get(base + '/branches/' + branch);
      timeline = current.timeline_id;
      const endpoints = (await get(base + '/endpoints')).items.filter(
        (value: { branch_id: string }) => value.branch_id === branch,
      );
      expect(endpoints).toHaveLength(1);
      endpoint = endpoints[0].id;
      await save('created_original_native_child_writer');
      await sql('CREATE TABLE public.expiration_probe(id int PRIMARY KEY, value text)');
      await sql("INSERT INTO public.expiration_probe VALUES(1,'retained-expiry')");
      expect((await sql('SELECT * FROM public.expiration_probe ORDER BY id')).rows).toEqual([
        [1, 'retained-expiry'],
      ]);
      await save('real_sql_bytes_written_and_read');
    }
    await marker('branch-expiration-observe.json', {
      project_id: project,
      parent_branch_id: parent,
      branch_id: branch,
      endpoint_id: endpoint,
      create_operation_id: creation,
      timeline_id: timeline,
    });
    const observed = await waitMarker('branch-expiration-storage-observed.json');
    expect(observed.branch_id).toBe(branch);
    expect(observed.timeline_id).toBe(timeline);
    await save('original_native_timeline_and_secrets_observed');
    if (!expiration) {
      await openPolicy();
      await page
        .getByLabel('UTC 到期时间')
        .fill(new Date(Date.now() + 300000).toISOString().slice(0, 19));
      const url = base + '/branches/' + branch + '/expiration';
      let first = '',
        key = '';
      await page.route('**' + url, async (route) => {
        if (route.request().method() !== 'PATCH') {
          await route.continue();
          return;
        }
        key = route.request().headers()['idempotency-key'];
        const response = await route.fetch();
        expect(response.status()).toBe(202);
        const value = await response.json();
        first = value.operation.id;
        operations.push(first);
        await save();
        await route.abort('failed');
      });
      await page.getByRole('button', { name: '确认到期设置', exact: true }).click();
      await expect(page.getByRole('alert')).toBeVisible();
      await page.unroute('**' + url);
      const replay = page.waitForResponse(
        (r) => new URL(r.url()).pathname === url && r.request().method() === 'PATCH',
      );
      await page.getByRole('button', { name: '确认到期设置', exact: true }).click();
      const recovered = await replay;
      expect(recovered.status()).toBe(202);
      expect(recovered.request().headers()['idempotency-key']).toBe(key);
      expect((await recovered.json()).operation.id).toBe(first);
      await expect(page.getByRole('dialog', { name: '分支到期设置' })).not.toBeVisible();
      await save('set_expiration_lost_reply_replayed_same_policy');
      expect((await get(base + '/branches/' + branch)).expiration_status).toBe('scheduled');
      await expect(
        page
          .getByTestId('lifecycle-' + branch)
          .getByRole('button', { name: '保护分支', exact: true }),
      ).toBeDisabled();
      await policy(360);
      await save('updated_future_expiration');
      await policy(null);
      const cleared = await get(base + '/branches/' + branch);
      expect(cleared.expires_at).toBeNull();
      expect(cleared.expiration_status).toBe('none');
      await save('cancelled_future_expiration');
      await policy(30);
      await save('final_expiration_scheduled');
      await expect
        .poll(
          async () => {
            const snapshot = await get(base + '/lifecycle');
            const current = snapshot.branches.find((value: { id: string }) => value.id === branch);
            if (current.expiration_status === 'blocked')
              throw new Error('Expiration blocked: ' + current.expiration_error_code);
            if (current.state === 'deleting' || current.state === 'deleted') {
              expiration = current.expiration_operation_id;
              await save();
              return true;
            }
            return false;
          },
          { timeout: 180000, intervals: [1000, 2500] },
        )
        .toBe(true);
    }
    const finished = await operation(expiration);
    expect(finished.action).toBe('expire_branch');
    expect(finished.resource_id).toBe(branch);
    expect(finished.steps).toHaveLength(6);
    if (!operations.includes(expiration)) operations.push(expiration);
    await save('original_automatic_expiration_completed');
    expect((await context.request.get(base + '/branches/' + branch)).status()).toBe(404);
    expect((await context.request.get(base + '/endpoints/' + endpoint)).status()).toBe(404);
    const snapshot = await get(base + '/lifecycle');
    const retired = snapshot.branches.find((value: { id: string }) => value.id === branch);
    expect(retired.expiration_status).toBe('retired');
    expect(
      snapshot.tombstones.find(
        (value: { operation_id: string }) => value.operation_id === expiration,
      ).physical_gc_state,
    ).toBe('held');
    expect((await get(base + '/branches/' + parent)).state).toBe('ready');
    expect((await get(base + '/endpoints/' + writer)).observed_state).toBe('suspended');
    await marker('branch-expiration-finished.json', {
      branch_id: branch,
      endpoint_id: endpoint,
      expiration_operation_id: expiration,
    });
    const proof = await waitMarker('branch-expiration-retained.json');
    expect(proof.branch_id).toBe(branch);
    expect(proof.vm_count).toBe(0);
    expect(proof.service_count).toBe(0);
    await save('held_tombstone_native_data_and_parent_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
