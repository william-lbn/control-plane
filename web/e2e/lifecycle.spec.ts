import { expect, test, type Response } from '@playwright/test';
import { generateKeyPairSync, randomBytes, sign } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Opt-in live browser test. Only a newly-created, unique fixture is logically
// deleted. Native data, credentials in private files and evidence are retained.
test('UI lifecycle: protections, dependency graph, retained delete and seven-day recovery', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!adminFile || !privateDir || !baseURL) throw new Error('Explicit live test inputs required');
  const privateRoot = privateDir;
  const resumeFile =
    process.env.NEON_E2E_LIFECYCLE_RESUME_FIXTURE || process.env.NEON_E2E_RECOVERY_FIXTURE;
  const resume = resumeFile
    ? (JSON.parse(await readFile(resumeFile, 'utf8')) as {
        project_id: string;
        project_name: string;
        branch_id: string;
        writer_id: string;
        reader_ids: string[];
        deleted_branch_ids: string[];
        original_failure_job: string;
        database_password_file: string;
        stage?: 'leaf_endpoint_accepted' | 'readers_in_progress';
        parent_branch_id?: string;
        leaf_branch_id?: string;
        leaf_endpoint_id?: string;
        operation_id?: string;
        reader_operation_ids?: string[];
        continuation_failure_job?: string;
      })
    : null;
  if (
    resume &&
    (!/^prj_[a-f0-9]{16}$/.test(resume.project_id) ||
      !/^br_[a-f0-9]{16}$/.test(resume.branch_id) ||
      !/^ep_[a-f0-9]{16}$/.test(resume.writer_id) ||
      !/^publication-ui-\d+$/.test(resume.original_failure_job) ||
      resume.project_name !== 'ci-lifecycle-' + resume.original_failure_job ||
      !Array.isArray(resume.reader_ids) ||
      (resume.stage === 'readers_in_progress'
        ? resume.reader_ids.length < 1 || resume.reader_ids.length > 2
        : resume.reader_ids.length !== (resume.stage === 'leaf_endpoint_accepted' ? 0 : 2)) ||
      !resume.reader_ids.every((id) => /^ep_[a-f0-9]{16}$/.test(id)) ||
      new Set([resume.writer_id, ...resume.reader_ids]).size !== 1 + resume.reader_ids.length ||
      !Array.isArray(resume.deleted_branch_ids) ||
      resume.deleted_branch_ids.length !== (resume.stage === 'leaf_endpoint_accepted' ? 0 : 2) ||
      !resume.deleted_branch_ids.every((id) => /^br_[a-f0-9]{16}$/.test(id)) ||
      new Set([resume.branch_id, ...resume.deleted_branch_ids]).size !==
        1 + resume.deleted_branch_ids.length ||
      (resume.stage !== undefined &&
        !['leaf_endpoint_accepted', 'readers_in_progress'].includes(resume.stage)) ||
      (resume.stage === 'readers_in_progress' &&
        (!Array.isArray(resume.reader_operation_ids) ||
          resume.reader_operation_ids.length !== resume.reader_ids.length ||
          !resume.reader_operation_ids.every((id) => /^op_[a-f0-9]{24}$/.test(id)) ||
          new Set(resume.reader_operation_ids).size !== resume.reader_operation_ids.length ||
          !/^publication-ui-\d+$/.test(resume.continuation_failure_job || ''))) ||
      (resume.stage === 'leaf_endpoint_accepted' &&
        (!/^br_[a-f0-9]{16}$/.test(resume.parent_branch_id || '') ||
          !/^br_[a-f0-9]{16}$/.test(resume.leaf_branch_id || '') ||
          new Set([resume.branch_id, resume.parent_branch_id, resume.leaf_branch_id]).size !== 3 ||
          !/^ep_[a-f0-9]{16}$/.test(resume.leaf_endpoint_id || '') ||
          resume.leaf_endpoint_id === resume.writer_id ||
          !/^op_[a-f0-9]{24}$/.test(resume.operation_id || ''))))
  )
    throw new Error('Invalid original lifecycle fixture');
  const attempt = process.env.NEON_E2E_ATTEMPT || new Date().toISOString().replace(/[^0-9]/g, '');
  if (!/^[a-z0-9_-]+$/.test(attempt)) throw new Error('Invalid attempt');
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  const password = resume
    ? (await readFile(resume.database_password_file, 'utf8')).trim()
    : randomBytes(30).toString('base64url');
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    mode: 0o600,
    flag: 'wx',
  });
  let project = '';
  let result = 'running';
  const projectName = resume?.project_name || 'ci-lifecycle-' + attempt;
  const endpoints = new Set<string>();
  const checks: { name: string; [key: string]: unknown }[] = [];
  const operations: { id: string; action: string; resource_id?: string }[] = [];
  let recoveryFault = process.env.NEON_E2E_LIFECYCLE_RECOVERY_FAULT === 'true';
  async function save() {
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          checks,
          operations,
          ...(resume
            ? {
                original_failure_job: resume.original_failure_job,
                creates_new_project: false,
                ...(resume.continuation_failure_job
                  ? { continuation_failure_job: resume.continuation_failure_job }
                  : {}),
              }
            : {}),
          credentials_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(privateRoot, attempt + '-fixture.json'),
      JSON.stringify({ project_id: project, endpoints: [...endpoints] }, null, 2) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await save();
  }
  async function data(url: string) {
    const response = await context.request.get(baseURL! + url);
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function created(path: string) {
    const reply = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path && r.request().method() === 'POST',
      { timeout: 480_000 },
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const response = await reply;
    expect(response.status()).toBe(202);
    const value = await response.json();
    operations.push({
      id: value.operation.id,
      action: value.operation.action,
      resource_id: value.resource.id,
    });
    // Persist the accepted identity before waiting for the modal to finish.
    // A process failure must not erase an unknown creation's recovery input.
    await save();
    await expect(page.locator('.create-modal')).not.toBeVisible({ timeout: 480_000 });
    return value.resource.id as string;
  }
  async function sql(endpoint: string, statement: string, status = 200) {
    // Recovery can repeat only these fixed read-only observations after a
    // service timeout. Never retry arbitrary SELECT/functions or any DML whose
    // result is unknown. Normal first-pass acceptance keeps one attempt.
    const retryableRead =
      resume &&
      status === 200 &&
      [
        "SELECT pg_is_in_recovery(),current_setting('transaction_read_only')",
        'SELECT id,marker FROM public.lifecycle_probe ORDER BY id',
        'SELECT count(*) FROM public.lifecycle_probe',
        'SELECT marker FROM public.lifecycle_probe',
      ].includes(statement);
    for (let attempt = 0; attempt < (retryableRead ? 2 : 1); attempt++) {
      await page.goto('/#/projects/' + project + '/query');
      await page.locator('select').first().selectOption(endpoint);
      await page.getByLabel('SQL 查询').fill(statement);
      await page.locator('input[type="password"]').fill(password);
      const reply = page.waitForResponse(
        (r) =>
          r.url().endsWith('/endpoints/' + endpoint + '/query') && r.request().method() === 'POST',
        { timeout: 190_000 },
      );
      await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
      const response = await reply;
      if (retryableRead && attempt === 0 && [503, 504].includes(response.status())) {
        await record('explicit_original_fixed_read_only_sql_retry_after_service_timeout', {
          endpoint_id: endpoint,
          status: response.status(),
        });
        continue;
      }
      expect(response.status()).toBe(status);
      return response.json();
    }
    throw new Error('Fixed read-only recovery observation exhausted');
  }
  async function suspend(endpoint: string) {
    await page.goto('/#/projects/' + project + '/compute');
    await page.locator('select').first().selectOption(endpoint);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145_000,
    });
  }
  async function lifecycle() {
    await page.goto('/#/projects/' + project + '/lifecycle');
    await expect(page.getByRole('heading', { name: '保护与删除', exact: true })).toBeVisible();
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
  }
  async function action(button: string, name: string, branch = ''): Promise<Response> {
    const injectRecovery = button === '恢复项目' && recoveryFault;
    if (injectRecovery) {
      // Trusted Linux orchestrator pauses the Worker, then injects a terminal
      // failure only into this fixture's queued recovery Operation. This is a
      // UI/admission recovery test, not a native storage failure certification.
      await writeFile(
        path.join(privateRoot, 'lifecycle-recovery-prepare.json'),
        JSON.stringify({ project_id: project }),
        { mode: 0o600, flag: 'wx' },
      );
      await expect
        .poll(
          async () => {
            try {
              return JSON.parse(
                await readFile(
                  path.join(privateRoot, 'lifecycle-recovery-worker-paused.json'),
                  'utf8',
                ),
              ).project_id;
            } catch (e) {
              if ((e as NodeJS.ErrnoException).code === 'ENOENT') return '';
              throw e;
            }
          },
          { timeout: 150_000 },
        )
        .toBe(project);
    }
    const target = branch
      ? page.getByTestId('lifecycle-' + branch)
      : page.getByTestId('project-lifecycle');
    await target.getByRole('button', { name: button, exact: true }).click();
    await page.getByLabel('资源名称确认').fill(name);
    const reply = page.waitForResponse(
      (r) =>
        r.request().method() !== 'GET' &&
        new URL(r.url()).pathname.startsWith('/api/v1/projects/' + project) &&
        !r.url().includes('/operations/'),
    );
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    const response = await reply;
    expect([200, 202]).toContain(response.status());
    if (response.status() === 202) {
      const v = await response.json();
      operations.push({ id: v.operation.id, action: v.operation.action });
      await save();
      if (injectRecovery) {
        await writeFile(
          path.join(privateRoot, 'lifecycle-recovery-queued.json'),
          JSON.stringify({ project_id: project, operation_id: v.operation.id }),
          { mode: 0o600, flag: 'wx' },
        );
        const retry = page
          .locator('.lifecycle-modal')
          .getByRole('button', { name: '重试既有操作', exact: true });
        await expect(retry).toBeEnabled({ timeout: 180_000 });
        await expect(
          page.locator('.lifecycle-modal').getByRole('button', { name: '确认执行', exact: true }),
        ).toBeDisabled();
        const pending = page.waitForResponse(
          (r) =>
            r.url().endsWith('/operations/' + v.operation.id + '/retry') &&
            r.request().method() === 'POST',
        );
        await retry.click();
        const retried = await pending;
        expect(retried.status()).toBe(202);
        expect((await retried.json()).id).toBe(v.operation.id);
        recoveryFault = false;
        await record('ui_failed_retained_recovery_retries_same_operation_inside_modal', {
          operation_id: v.operation.id,
          fault: 'operator_injected_terminal_status',
        });
      }
    }
    await expect(page.locator('.lifecycle-modal')).not.toBeVisible({ timeout: 480_000 });
    return response;
  }
  await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    let writer: string;
    let root: string;
    const readers: string[] = [];
    if (!resume || resume.stage) {
      let parent = '';
      let leaf = '';
      if (!resume) {
        await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
        await page.getByLabel('项目名称').fill(projectName);
        await page.locator('.create-modal input[type="password"]').fill(password);
        await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
        await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
        project = await created('/api/v1/organizations/local/projects');
        const initial = await data('/api/v1/projects/' + project + '/endpoints');
        writer = initial.items[0].id as string;
        root = initial.items[0].branch_id as string;
        endpoints.add(writer);
        await record('ui_creates_owned_isolated_project', { writer, root });
        await sql(
          writer,
          'CREATE TABLE public.lifecycle_probe(id integer PRIMARY KEY, marker text NOT NULL)',
        );
        await sql(writer, "INSERT INTO public.lifecycle_probe VALUES(1,'retained-parent')");
        await suspend(writer);
        for (const branchName of ['parent', 'leaf']) {
          await page.goto('/#/projects/' + project + '/branches');
          await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
          await page.getByLabel('分支名称').fill(branchName);
          await page.getByLabel('父分支').selectOption(parent || root);
          await page.getByLabel('同时创建读写 Compute Endpoint').uncheck();
          const id = await created('/api/v1/projects/' + project + '/branches');
          if (parent) leaf = id;
          else parent = id;
        }
        await lifecycle();
        await expect(
          page
            .getByTestId('lifecycle-' + root)
            .getByRole('button', { name: '删除分支', exact: true }),
        ).toBeDisabled();
        await expect(
          page
            .getByTestId('lifecycle-' + parent)
            .getByRole('button', { name: '删除分支', exact: true }),
        ).toBeDisabled();
        await expect(
          page
            .getByTestId('project-lifecycle')
            .getByRole('button', { name: '删除项目', exact: true }),
        ).toBeDisabled();
        await record('ui_root_child_dependencies_and_protection_block_delete');
        await action('保护分支', 'leaf', leaf);
        await expect(
          page
            .getByTestId('lifecycle-' + leaf)
            .getByRole('button', { name: '删除分支', exact: true }),
        ).toBeDisabled();
        await action('解除分支保护', 'leaf', leaf);
        await record('ui_admin_explicit_branch_protection');
      } else if (resume.stage === 'leaf_endpoint_accepted') {
        // Recover the original accepted leaf creation after an API outage.
        // Never create a replacement project, branch, Endpoint or SQL table.
        project = resume.project_id;
        root = resume.branch_id;
        writer = resume.writer_id;
        parent = resume.parent_branch_id!;
        leaf = resume.leaf_branch_id!;
        endpoints.add(writer);
        endpoints.add(resume.leaf_endpoint_id!);
        const original = await data('/api/v1/projects/' + project);
        expect(original.name).toBe(projectName);
        expect(original.source).toBe('managed');
        expect(original.state).toBe('ready');
        expect(original.protected).toBe(true);
        const graph = await data('/api/v1/projects/' + project + '/lifecycle');
        expect(
          graph.branches
            .filter((b: { deleted_at: string | null }) => !b.deleted_at)
            .map((b: { id: string }) => b.id)
            .sort(),
        ).toEqual([root, parent, leaf].sort());
        for (const [id, name, ancestor] of [
          [root, 'main', null],
          [parent, 'parent', root],
          [leaf, 'leaf', parent],
        ]) {
          const b = graph.branches.find((b: { id: string }) => b.id === id);
          expect(b.name).toBe(name);
          expect(b.parent_branch_id).toBe(ancestor);
          expect(b.state).toBe('ready');
          expect(b.protected).toBe(id === root);
        }
        const opPath = '/api/v1/projects/' + project + '/operations/' + resume.operation_id;
        const op = await data(opPath);
        expect(op.project_id).toBe(project);
        expect(op.resource_id).toBe(resume.leaf_endpoint_id);
        expect(op.action).toBe('create_endpoint');
        expect(['failed', 'queued', 'running', 'retry_wait', 'succeeded']).toContain(op.state);
        if (op.state === 'failed') {
          expect(op.retryable).toBe(true);
          await page.goto('/#/projects/' + project + '/operations');
          await page.getByRole('button', { name: `查看操作 ${op.id} 的步骤`, exact: true }).click();
          const reply = page.waitForResponse(
            (r) =>
              new URL(r.url()).pathname === opPath + '/retry' && r.request().method() === 'POST',
          );
          await page.getByRole('button', { name: '重试原操作', exact: true }).click();
          const retried = await reply;
          expect(retried.status()).toBe(202);
          expect((await retried.json()).id).toBe(op.id);
        }
        await expect
          .poll(async () => (await data(opPath)).state, {
            timeout: 480_000,
            intervals: [1000, 2000, 5000],
          })
          .toBe('succeeded');
        const current = (await data('/api/v1/projects/' + project + '/endpoints')).items;
        expect(current.map((e: { id: string }) => e.id).sort()).toEqual(
          [writer, resume.leaf_endpoint_id].sort(),
        );
        const child = current.find((e: { id: string }) => e.id === resume.leaf_endpoint_id);
        expect(child.branch_id).toBe(leaf);
        expect(child.endpoint_type).toBe('read_write');
        expect(
          (await sql(writer, 'SELECT id,marker FROM public.lifecycle_probe ORDER BY id')).rows,
        ).toEqual([[1, 'retained-parent']]);
        await suspend(writer);
        await record(
          'original_accepted_leaf_endpoint_recovered_without_recreating_resources_or_sql',
          { operation_id: op.id, leafWriter: child.id },
        );
      } else {
        project = resume.project_id;
        root = resume.branch_id;
        writer = resume.writer_id;
        readers.push(...resume.reader_ids);
        for (const id of [writer, ...readers]) endpoints.add(id);
        const retained = await data('/api/v1/projects/' + project);
        expect(retained.name).toBe(projectName);
        expect(retained.source).toBe('managed');
        expect(retained.state).toBe('ready');
        expect(retained.protected).toBe(false);
        const graph = await data('/api/v1/projects/' + project + '/lifecycle');
        expect(
          graph.branches
            .filter((b: { deleted_at: string | null }) => !b.deleted_at)
            .map((b: { id: string }) => b.id),
        ).toEqual([root]);
        for (const id of resume.deleted_branch_ids)
          expect(
            graph.tombstones.some(
              (t: { resource_type: string; resource_id: string; physical_gc_state: string }) =>
                t.resource_type === 'branch' &&
                t.resource_id === id &&
                t.physical_gc_state === 'held',
            ),
          ).toBe(true);
        const current = (await data('/api/v1/projects/' + project + '/endpoints')).items;
        expect(current.map((e: { id: string }) => e.id).sort()).toEqual(
          [writer, ...readers].sort(),
        );
        for (const id of readers) {
          const e = current.find((e: { id: string }) => e.id === id);
          expect(e.branch_id).toBe(root);
          expect(e.endpoint_type).toBe('read_only');
        }
        expect(
          (await sql(writer, 'SELECT id,marker FROM public.lifecycle_probe ORDER BY id')).rows,
        ).toEqual([[1, 'retained-parent']]);
        await suspend(writer);
        await record(
          'original_partial_readers_and_held_branches_reused_without_recreating_project_or_sql',
        );
      }
      if (resume?.stage !== 'readers_in_progress') {
        let leafWriter: string;
        if (!resume) {
          await page.goto('/#/projects/' + project + '/branches/' + leaf);
          await page.getByRole('button', { name: '＋ 创建 Endpoint', exact: true }).click();
          await page.locator('.create-modal input[type="password"]').fill(password);
          await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
          await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
          leafWriter = await created('/api/v1/projects/' + project + '/endpoints');
        } else {
          leafWriter = resume.leaf_endpoint_id!;
        }
        endpoints.add(leafWriter);
        expect((await sql(leafWriter, 'SELECT marker FROM public.lifecycle_probe')).rows).toEqual([
          ['retained-parent'],
        ]);
        await record('ui_leaf_compute_inherits_native_data', { leafWriter });
        await lifecycle();
        const deleted = await action('删除分支', 'leaf', leaf);
        const accepted = await deleted.json();
        const cookies = await context.cookies();
        const csrf = cookies.find((c) => c.name === 'neon_v2_csrf')!.value;
        const original = deleted.request();
        const replay = await context.request.delete(original.url(), {
          headers: {
            'X-CSRF-Token': csrf,
            'If-Match': original.headers()['if-match'],
            'Idempotency-Key': original.headers()['idempotency-key'],
          },
          data: original.postData()!,
        });
        expect(replay.status()).toBe(202);
        expect((await replay.json()).operation.id).toBe(accepted.operation.id);
        await expect(page.getByTestId('lifecycle-' + leaf)).toContainText('已删除');
        expect(
          (
            await context.request.get(baseURL + '/api/v1/projects/' + project + '/branches/' + leaf)
          ).status(),
        ).toBe(404);
        expect(
          (
            await context.request.get(
              baseURL + '/api/v1/projects/' + project + '/endpoints/' + leafWriter,
            )
          ).status(),
        ).toBe(404);
        await record('ui_active_leaf_delete_retirement_tombstone_and_same_operation_replay', {
          operation_id: accepted.operation.id,
        });
        await action('删除分支', 'parent', parent);
        await record('ui_parent_delete_after_child');
        await action('解除项目保护', projectName);
        await action('解除分支保护', 'main', root);
      }
      // Each reader has its own selector and VM, with the writer's database roles.
      // Run them serially on the resource-constrained cluster and retain all IDs.
      for (let n = 0; n < 2; n++) {
        let reader = readers[n];
        if (reader) {
          const opPath =
            '/api/v1/projects/' + project + '/operations/' + resume!.reader_operation_ids![n];
          const op = await data(opPath);
          expect(op.project_id).toBe(project);
          expect(op.resource_id).toBe(reader);
          expect(op.action).toBe('create_endpoint');
          expect(['failed', 'queued', 'running', 'retry_wait', 'succeeded']).toContain(op.state);
          if (op.state === 'failed') {
            expect(op.retryable).toBe(true);
            await page.goto('/#/projects/' + project + '/operations');
            await page
              .getByRole('button', { name: `查看操作 ${op.id} 的步骤`, exact: true })
              .click();
            const reply = page.waitForResponse(
              (r) =>
                new URL(r.url()).pathname === opPath + '/retry' && r.request().method() === 'POST',
            );
            await page.getByRole('button', { name: '重试原操作', exact: true }).click();
            const retried = await reply;
            expect(retried.status()).toBe(202);
            expect((await retried.json()).id).toBe(op.id);
          }
          await expect
            .poll(async () => (await data(opPath)).state, {
              timeout: 480_000,
              intervals: [1000, 2000, 5000],
            })
            .toBe('succeeded');
          await record(
            'original_accepted_reader_operation_recovered_or_observed_without_replacement',
            { operation_id: op.id, reader },
          );
        } else {
          await page.goto('/#/projects/' + project + '/branches/' + root);
          await page.getByRole('button', { name: '＋ 添加 Compute', exact: true }).click();
          await page.getByLabel('Compute 类型').selectOption('read_only');
          await expect(page.locator('.create-modal input[type="password"]')).toHaveCount(0);
          await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
          await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
          reader = await created('/api/v1/projects/' + project + '/endpoints');
          readers.push(reader);
        }
        endpoints.add(reader);
        const replica = await sql(
          reader,
          "SELECT pg_is_in_recovery(),current_setting('transaction_read_only')",
        );
        expect(replica.rows).toEqual([[true, 'on']]);
        const rejected = await sql(
          reader,
          `INSERT INTO public.lifecycle_probe VALUES(${10 + n},'must-not-write')`,
          422,
        );
        expect(rejected.code).toBe('sql_failed');
        expect(rejected.message).toMatch(/read.only/i);
        await record('ui_reader_is_real_replica_and_rejects_writes', { reader, ordinal: n });
        await suspend(reader);
        const state = await data('/api/v1/projects/' + project + '/endpoints');
        expect(state.items.find((e: { id: string }) => e.id === reader).observed_state).toBe(
          'suspended',
        );
        await record('ui_reader_independent_one_to_zero', { reader });
      }
      if (resume)
        await record('original_lifecycle_dependencies_readers_and_held_branch_deletions_confirmed');
    } else {
      project = resume.project_id;
      root = resume.branch_id;
      writer = resume.writer_id;
      readers.push(...resume.reader_ids);
      for (const id of [writer, ...readers]) endpoints.add(id);
      const retained = await data('/api/v1/projects/' + project);
      expect(retained.name).toBe(projectName);
      expect(retained.source).toBe('managed');
      expect(retained.state).toBe('ready');
      expect(retained.protected).toBe(false);
      const graph = await data('/api/v1/projects/' + project + '/lifecycle');
      expect(
        graph.branches
          .filter((b: { deleted_at: string | null }) => !b.deleted_at)
          .map((b: { id: string }) => b.id),
      ).toEqual([root]);
      for (const id of resume.deleted_branch_ids)
        expect(
          graph.tombstones.some(
            (t: { resource_type: string; resource_id: string; physical_gc_state: string }) =>
              t.resource_type === 'branch' &&
              t.resource_id === id &&
              t.physical_gc_state === 'held',
          ),
        ).toBe(true);
      const current = (await data('/api/v1/projects/' + project + '/endpoints')).items;
      expect(current.map((e: { id: string }) => e.id).sort()).toEqual([writer, ...readers].sort());
      for (const id of readers) {
        const endpoint = current.find((e: { id: string }) => e.id === id);
        expect(endpoint.branch_id).toBe(root);
        expect(endpoint.endpoint_type).toBe('read_only');
        expect(endpoint.observed_state).toBe('suspended');
      }
      await record('original_lifecycle_dependencies_readers_and_held_branch_deletions_confirmed');
    }
    // The original failing request may have executed its INSERT before the
    // reply was lost. Resolve the exact rows first; never blindly repeat DML.
    const existing = (await sql(writer, 'SELECT id,marker FROM public.lifecycle_probe ORDER BY id'))
      .rows;
    if (existing.length === 1) {
      expect(existing).toEqual([[1, 'retained-parent']]);
      await sql(writer, "INSERT INTO public.lifecycle_probe VALUES(2,'before-delete')");
    } else {
      expect(existing).toEqual([
        [1, 'retained-parent'],
        [2, 'before-delete'],
      ]);
    }
    if (resume) await record('original_unknown_insert_resolved_by_exact_rows_without_blind_replay');
    for (const reader of readers) {
      await expect
        .poll(
          async () =>
            (await sql(reader, 'SELECT id,marker FROM public.lifecycle_probe ORDER BY id')).rows,
          { timeout: 120_000, intervals: [1000, 2000, 5000] },
        )
        .toEqual([
          [1, 'retained-parent'],
          [2, 'before-delete'],
        ]);
      await record('ui_reader_independent_zero_to_one_follows_writer_wal', { reader });
      await suspend(reader);
    }
    // Keep one reader and the writer live for deletion to retire distinct VMs.
    await sql(readers[0], 'SELECT count(*) FROM public.lifecycle_probe');
    await sql(writer, 'SELECT count(*) FROM public.lifecycle_probe');
    await lifecycle();
    await page.screenshot({
      path: testInfo.outputPath('before-project-delete.png'),
      fullPage: true,
    });
    await action('删除项目', projectName);
    await expect(page.getByTestId('project-lifecycle')).toContainText('已删除');
    await expect(page.getByTestId('project-lifecycle')).toContainText('恢复期限');
    expect((await context.request.get(baseURL + '/api/v1/projects/' + project)).status()).toBe(404);
    await record('ui_project_delete_closes_access_with_seven_day_tombstone');
    await page.goto('/#/projects');
    await page.getByRole('button', { name: '查看已删除项目', exact: true }).click();
    const card = page.locator('.project-card').filter({ hasText: projectName });
    await expect(card).toBeVisible();
    await card.click();
    await expect(page.getByRole('heading', { name: '保护与删除', exact: true })).toBeVisible();
    await action('恢复项目', projectName);
    await expect(page.getByTestId('project-lifecycle')).toContainText('就绪');
    const recovered = await data('/api/v1/projects/' + project + '/endpoints');
    expect(recovered.items.map((e: { id: string }) => e.id).sort()).toEqual(
      [writer, ...readers].sort(),
    );
    expect(
      recovered.items.every((e: { observed_state: string }) => e.observed_state === 'suspended'),
    ).toBe(true);
    await record('ui_trash_recovery_preserves_identity_and_compute_zero');
    expect(
      (await sql(writer, 'SELECT id,marker FROM public.lifecycle_probe ORDER BY id')).rows,
    ).toEqual([
      [1, 'retained-parent'],
      [2, 'before-delete'],
    ]);
    await record('ui_recovered_project_cold_wake_uses_original_proxy_credentials_and_data');
    await suspend(writer);
    for (const reader of readers) {
      expect(
        (await sql(reader, 'SELECT id,marker FROM public.lifecycle_probe ORDER BY id')).rows,
      ).toEqual([
        [1, 'retained-parent'],
        [2, 'before-delete'],
      ]);
      await suspend(reader);
      await record('ui_recovered_reader_preserves_selector_and_original_data', { reader });
    }
    const graph = await data('/api/v1/projects/' + project + '/lifecycle');
    expect(graph.branches.filter((b: { deleted_at: string | null }) => !b.deleted_at)).toHaveLength(
      1,
    );
    expect(graph.tombstones).toHaveLength(3);
    expect(graph.physical_gc_enabled).toBe(false);
    await lifecycle();
    await page.screenshot({
      path: testInfo.outputPath('retained-history-after-recovery.png'),
      fullPage: true,
    });
    await record(
      'ui_recovery_does_not_resurrect_previously_deleted_branches_and_preserves_evidence',
    );
    // A second delete/recover round exercises the currently implemented
    // Backend driver, rather than certifying service retirement with a stub.
    for (const statement of [
      'GRANT CREATE, CONNECT ON DATABASE postgres TO control_probe WITH GRANT OPTION',
      'CREATE SCHEMA app_data',
      'CREATE TABLE app_data.notes(id text PRIMARY KEY, owner_id text NOT NULL, body text NOT NULL)',
      'ALTER TABLE app_data.notes ENABLE ROW LEVEL SECURITY',
      'ALTER TABLE app_data.notes FORCE ROW LEVEL SECURITY',
      'GRANT USAGE ON SCHEMA app_data TO control_probe WITH GRANT OPTION',
      'GRANT SELECT, INSERT, UPDATE, DELETE ON app_data.notes TO control_probe WITH GRANT OPTION',
      "CREATE POLICY lifecycle_owner ON app_data.notes USING (owner_id = current_setting('request.jwt.claims',true)::jsonb->>'sub') WITH CHECK (owner_id = current_setting('request.jwt.claims',true)::jsonb->>'sub')",
      "INSERT INTO app_data.notes VALUES ('retained','alice','service-retained')",
    ])
      await sql(writer, statement);
    const keys = generateKeyPairSync('ed25519');
    const issuer = 'https://identity.example.test';
    const provider = {
      ...keys.publicKey.export({ format: 'jwk' }),
      alg: 'EdDSA',
      kid: 'lifecycle-provider',
      use: 'sig',
    };
    const header = Buffer.from(
      JSON.stringify({ alg: 'EdDSA', kid: provider.kid, typ: 'JWT' }),
    ).toString('base64url');
    const claims = Buffer.from(
      JSON.stringify({
        iss: issuer,
        aud: root,
        sub: 'alice',
        exp: Math.floor(Date.now() / 1000) + 3600,
      }),
    ).toString('base64url');
    const input = header + '.' + claims;
    const applicationToken =
      input + '.' + sign(null, Buffer.from(input), keys.privateKey).toString('base64url');
    async function dataService(disable = false) {
      const reply = page.waitForResponse(
        (r) =>
          r.url().endsWith('/branches/' + root + '/data-api') &&
          r.request().method() === (disable ? 'DELETE' : 'POST'),
      );
      await page
        .getByRole('button', { name: disable ? '停用 Data API' : '启用 Data API', exact: true })
        .click();
      const response = await reply;
      expect(response.status()).toBe(202);
      const accepted = await response.json();
      operations.push({ id: accepted.operation.id, action: accepted.operation.action });
      await expect(page.getByTestId('data-api-operation')).toContainText('succeeded', {
        timeout: 490_000,
      });
      await save();
    }
    async function dataRequest() {
      await page.getByLabel('应用 JWT', { exact: true }).fill(applicationToken);
      await page.getByRole('button', { name: '发送 Data API 请求', exact: true }).click();
      await expect(page.getByTestId('data-api-response')).toContainText('HTTP 200', {
        timeout: 190_000,
      });
      await expect(page.getByTestId('data-api-response')).toContainText('service-retained');
      await page.getByLabel('应用 JWT', { exact: true }).fill('');
    }
    await page.goto('/#/projects/' + project + '/data-api');
    await page.getByLabel('JWT Issuer', { exact: true }).fill(issuer);
    await page.getByLabel('JWT Audience', { exact: true }).fill(root);
    await page
      .getByLabel('Provider JWKS', { exact: true })
      .fill(JSON.stringify({ keys: [provider] }));
    await dataService();
    await dataRequest();
    await record('ui_native_data_api_enabled_and_real_rls_request_before_project_delete');
    await page.goto('/#/projects/' + project + '/credentials');
    await page.getByLabel('凭据分支', { exact: true }).selectOption(root);
    await page.getByLabel('凭据名称').fill('lifecycle-' + attempt);
    await page.getByLabel('凭据分支范围').selectOption('self');
    const credentialReply = page.waitForResponse(
      (r) => r.url().endsWith('/credentials') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const credentialResponse = await credentialReply;
    expect(credentialResponse.status()).toBe(201);
    const credentialID = (await credentialResponse.json()).credential.id as string;
    const backendToken = await page.getByLabel('新 Backend Token').inputValue();
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    await lifecycle();
    await action('删除项目', projectName);
    const closed = await context.request.get(baseURL + '/data/v1/' + root + '/notes', {
      headers: { Authorization: 'Bearer ' + applicationToken },
    });
    expect(closed.status()).toBe(404);
    await record('ui_project_delete_retires_active_data_api_and_closes_public_relay');
    await action('恢复项目', projectName);
    const instance = await data('/api/v1/projects/' + project + '/branches/' + root + '/data-api');
    expect(instance.state).toBe('disabled');
    expect(
      (
        await context.request.get(baseURL + '/data/v1/' + root + '/notes', {
          headers: { Authorization: 'Bearer ' + applicationToken },
        })
      ).status(),
    ).toBe(404);
    await page.goto('/#/projects/' + project + '/credentials');
    await page.getByLabel('凭据分支', { exact: true }).selectOption(root);
    await expect(page.getByTestId('backend-row-' + credentialID)).toContainText('已撤销');
    await page.getByLabel('检查 Backend Token').fill(backendToken);
    const checked = page.waitForResponse(
      (r) => r.url().endsWith('/credentials/check') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '检查应用凭据', exact: true }).click();
    expect((await checked).status()).toBe(401);
    await page.getByLabel('检查 Backend Token').fill('');
    await record('ui_recovery_keeps_backend_credentials_revoked_and_data_api_disabled');
    await page.goto('/#/projects/' + project + '/data-api');
    await expect(page.getByLabel('JWT Audience', { exact: true })).toHaveValue(root);
    await dataService();
    await dataRequest();
    await record('ui_explicit_data_api_reenable_uses_retained_rls_schema_and_data');
    await dataService(true);
    await suspend(writer);
    result = 'pass';
  } catch (e) {
    result = 'fail';
    await page.screenshot({
      path: testInfo.outputPath('failure.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
    throw e;
  } finally {
    await save();
  }
});
