import { expect, test } from '@playwright/test';
import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Product writes begin in the real Console. Direct protocol probes only add
// negative/concurrency assertions. No token/password/query URL enters evidence.
test('Object Storage: UI files, native timeline clone, isolated changes and cold wake', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  page.setDefaultTimeout(40000);
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!baseURL || !adminFile || !privateDir) throw new Error('Protected Linux E2E inputs required');
  const attempt = `${process.env.NEON_E2E_ATTEMPT || Date.now()}-storage`;
  const password = randomBytes(30).toString('base64url');
  const original = Buffer.from('Immutable parent content · 初始文件\n');
  const updated = Buffer.from('Independent child content · 子分支文件\n');
  const key = 'documents/report.txt';
  const digest = (data: Buffer) => createHash('sha256').update(data).digest('hex');
  const checks: { name: string; [key: string]: unknown }[] = [];
  const endpoints = new Set<string>();
  const cleanup: {
    resource: string;
    action: string;
    status?: number;
    operation_id?: string;
    state?: string;
  }[] = [];
  let project = '';
  let root = '';
  let child = '';
  let writer = '';
  let childWriter = '';
  let result = 'running';
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    flag: 'wx',
    mode: 0o600,
  });
  async function save() {
    await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          linux_only: process.platform === 'linux',
          project_id: project,
          branch_id: root,
          child_id: child,
          writer_id: writer,
          endpoints: [...endpoints],
          checks,
          failure_cleanup: cleanup,
          data_retained: true,
          s3_compatible: false,
          production_qualified: false,
          credentials_in_evidence: false,
        },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(privateDir!, attempt + '-fixture.json'),
      JSON.stringify({
        project_id: project,
        branch_id: root,
        child_id: child,
        endpoints: [...endpoints],
      }) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, fields: Record<string, unknown> = {}) {
    checks.push({ name, ...fields });
    await save();
  }
  const branchPath = (branch: string) => `/api/v1/projects/${project}/branches/${branch}/storage`;
  const objectPath = (branch: string, bucket = 'uploads') =>
    `${branchPath(branch)}/buckets/${bucket}/objects?${new URLSearchParams({ key })}`;
  async function headers() {
    const csrf = (await context.cookies(baseURL!)).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw new Error('CSRF cookie missing');
    return { 'X-CSRF-Token': csrf, Origin: new URL(baseURL!).origin };
  }
  async function storagePage(branch: string) {
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByLabel('存储分支').selectOption(branch);
    await expect(page.getByRole('heading', { name: 'Object Storage', exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: '加载存储桶', exact: true })).toBeEnabled({
      timeout: 490000,
    });
  }
  async function mutate(branch: string, disable: boolean) {
    const pending = page.waitForResponse(
      (r) =>
        r.url().endsWith(branchPath(branch)) &&
        r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '禁用对象存储' : '启用对象存储', exact: true })
      .click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const op = (await response.json()).operation;
    await expect(page.getByTestId('storage-operation')).toContainText(op.id);
    await expect(page.getByTestId('storage-operation')).toContainText(
      /succeeded|failed|cancelled/,
      { timeout: 490000 },
    );
    await expect(page.getByTestId('storage-operation')).toContainText('succeeded');
    return { response, op };
  }
  async function load(branch: string, bucket = 'uploads') {
    await storagePage(branch);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    const row = page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: bucket, exact: true }) });
    await expect(row).toBeVisible({ timeout: 190000 });
    await row.getByRole('button', { name: '选择', exact: true }).click();
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
  }
  async function upload(data: Buffer, overwrite = false) {
    await page.getByLabel('对象键', { exact: true }).fill(key);
    await page
      .getByLabel('上传文件')
      .setInputFiles({ name: 'report.txt', mimeType: 'text/plain', buffer: data });
    await page.getByLabel('覆盖已有对象').setChecked(overwrite);
    const pending = page.waitForResponse(
      (r) => r.url().includes('/objects?') && r.request().method() === 'PUT',
    );
    await page.getByRole('button', { name: '上传对象', exact: true }).click();
    expect((await pending).status()).toBe(200);
    await expect(page.getByTestId('storage-notice')).toContainText('上传已完成', {
      timeout: 190000,
    });
  }
  async function suspend(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
  }
  async function presign(branch: string) {
    const response = await context.request.post(`${branchPath(branch)}/buckets/uploads/presign`, {
      headers: await headers(),
      data: { key, expires_seconds: 300 },
    });
    expect(response.status()).toBe(200);
    return (await response.json()).url as string;
  }
  async function lifecycleAction(button: string, name: string, branch = '') {
    const target = branch
      ? page.getByTestId('lifecycle-' + branch)
      : page.getByTestId('project-lifecycle');
    await target.getByRole('button', { name: button, exact: true }).click();
    await page.getByLabel('资源名称确认').fill(name);
    const pending = page.waitForResponse(
      (r) =>
        r.request().method() !== 'GET' &&
        new URL(r.url()).pathname.startsWith(`/api/v1/projects/${project}`) &&
        !r.url().includes('/operations/'),
    );
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    const response = await pending;
    expect([200, 202]).toContain(response.status());
    await expect(page.getByRole('dialog', { name: '确认生命周期操作' })).toHaveCount(0, {
      timeout: 490000,
    });
    if (response.status() === 202) {
      const op = (await response.json()).operation;
      const observed = await context.request.get(`/api/v1/projects/${project}/operations/${op.id}`);
      expect((await observed.json()).state).toBe('succeeded');
    }
  }
  async function retireFailure() {
    const deadline = Date.now() + 100000;
    if (!project) return;
    const h = await headers();
    async function stop(
      url: string,
      method: 'DELETE' | 'POST',
      resource: string,
      generation?: number,
    ) {
      if (Date.now() >= deadline) return;
      const entry: (typeof cleanup)[number] = {
        resource,
        action: method === 'DELETE' ? 'disable_storage' : 'suspend_compute',
      };
      cleanup.push(entry);
      await save();
      try {
        const r = await context.request.fetch(url, {
          method,
          headers: {
            ...h,
            'Idempotency-Key': randomUUID(),
            ...(generation === undefined ? {} : { 'If-Match': `"${generation}"` }),
          },
          ...(method === 'POST' ? { data: {} } : {}),
          timeout: 10000,
        });
        entry.status = r.status();
        if (r.status() !== 202) {
          entry.state = 'not_accepted';
          return;
        }
        entry.operation_id = (await r.json()).operation.id;
        await save();
        while (Date.now() < deadline) {
          const observed = await context.request.get(
            `/api/v1/projects/${project}/operations/${entry.operation_id}`,
            { timeout: 10000 },
          );
          if (!observed.ok()) break;
          entry.state = (await observed.json()).state;
          if (['succeeded', 'failed', 'cancelled'].includes(entry.state!)) break;
          await new Promise((resolve) => setTimeout(resolve, 1000));
        }
      } catch {
        entry.state = 'uncertain_retain_original_operation';
      } finally {
        await save();
      }
    }
    for (const branch of [child, root].filter(Boolean)) {
      const r = await context.request.get(branchPath(branch), { timeout: 10000 });
      if (r.ok()) {
        const v = await r.json();
        if (v.state !== 'disabled') await stop(branchPath(branch), 'DELETE', branch, v.generation);
      }
    }
    for (const endpoint of endpoints)
      await stop(`/api/v1/projects/${project}/endpoints/${endpoint}/suspend`, 'POST', endpoint);
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
    await page.getByLabel('项目名称').fill('ci-' + attempt);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const creating = page.waitForResponse(
      (r) => r.url().endsWith('/organizations/local/projects') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const created = await creating;
    expect(created.status()).toBe(202);
    project = (await created.json()).resource.id;
    await expect(page.locator('.create-modal')).toHaveCount(0, { timeout: 490000 });
    const e = (await (await context.request.get(`/api/v1/projects/${project}/endpoints`)).json())
      .items[0];
    root = e.branch_id;
    writer = e.id;
    endpoints.add(writer);
    await record('ui_created_bounded_neon_project');
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('SQL 查询').fill('GRANT CREATE ON DATABASE postgres TO control_probe');
    await page.locator('input[type="password"]').fill(password);
    const grant = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${writer}/query`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    expect((await grant).status()).toBe(200);
    await page.goto(`/#/projects/${project}/storage`);
    const enabled = await mutate(root, false);
    const replay = await context.request.post(branchPath(root), {
      headers: {
        ...(await headers()),
        'Idempotency-Key': enabled.response.request().headers()['idempotency-key'],
        'If-Match': '"0"',
      },
      data: { database: 'postgres' },
    });
    expect(replay.status()).toBe(202);
    expect((await replay.json()).operation.id).toBe(enabled.op.id);
    await record('ui_enabled_storage_and_exact_operation_idempotency_replayed', {
      operation_id: enabled.op.id,
    });
    await page.getByLabel('存储桶名称').fill('uploads');
    await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
    await expect(page.getByTestId('storage-notice')).toContainText('存储桶已创建', {
      timeout: 190000,
    });
    await upload(original);
    await record('ui_private_bucket_upload_published_real_immutable_blob');
    const row = page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: key, exact: true }) });
    const downloading = page.waitForEvent('download');
    await row.getByRole('button', { name: '下载', exact: true }).click();
    const download = await downloading;
    const stream = await download.createReadStream();
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    expect(digest(Buffer.concat(chunks))).toBe(digest(original));
    await record('ui_downloaded_bytes_and_sha256_match');
    const anonymous = await context.request.get(
      `/storage/v1/${root}/uploads?${new URLSearchParams({ key })}`,
    );
    expect(anonymous.status()).toBe(404);
    const signed = await presign(root);
    const range = await context.request.get(signed, { headers: { Range: 'bytes=0-8' } });
    expect(range.status()).toBe(206);
    expect(await range.body()).toEqual(original.subarray(0, 9));
    expect((await context.request.head(signed)).status()).toBe(200);
    expect(
      (
        await context.request.get(signed, { headers: { 'If-None-Match': `"${digest(original)}"` } })
      ).status(),
    ).toBe(304);
    expect((await context.request.get(signed + 'tampered')).status()).toBe(404);
    await record('private_acl_range_head_conditional_get_and_signature_tampering_checked');
    const h = await headers();
    expect(
      (
        await context.request.put(objectPath(root), {
          headers: { ...h, 'Content-Type': 'text/plain', 'If-None-Match': '*' },
          data: updated,
        })
      ).status(),
    ).toBe(412);
    expect(
      (
        await context.request.put(objectPath(root), {
          headers: { ...h, 'Content-Type': 'text/plain', 'If-Match': '"stale"' },
          data: updated,
        })
      ).status(),
    ).toBe(412);
    expect(
      (
        await context.request.put(objectPath(root), {
          headers: { ...h, 'Content-Type': 'text/plain' },
          data: updated,
        })
      ).status(),
    ).toBe(428);
    await record('write_preconditions_prevent_unconditional_or_stale_overwrites');
    await suspend(writer);
    await record('ui_storage_writer_suspended_to_zero');
    await page.goto(`/#/projects/${project}/storage`);
    expect(
      (await (await context.request.get(`/api/v1/projects/${project}/endpoints/${writer}`)).json())
        .observed_state,
    ).toBe('suspended');
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await expect(page.getByRole('cell', { name: 'uploads', exact: true })).toBeVisible({
      timeout: 190000,
    });
    await record('metadata_page_preserved_zero_and_ui_directory_read_cold_woke_writer');
    await page.goto(`/#/projects/${project}/branches`);
    await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
    await page.getByLabel('分支名称').fill('child-' + attempt);
    await page.getByLabel('父分支').selectOption(root);
    await page.getByLabel('数据库角色密码').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const cloning = page.waitForResponse(
      (r) => r.url().endsWith(`/projects/${project}/branches`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const cloned = await cloning;
    expect(cloned.status()).toBe(202);
    child = (await cloned.json()).resource.id;
    await expect(page.locator('.create-modal')).toHaveCount(0, { timeout: 490000 });
    const ce = (
      await (await context.request.get(`/api/v1/projects/${project}/endpoints`)).json()
    ).items.find((v: { branch_id: string }) => v.branch_id === child);
    childWriter = ce.id;
    endpoints.add(childWriter);
    await load(child);
    await expect(page.getByRole('cell', { name: key, exact: true })).toBeVisible({
      timeout: 190000,
    });
    expect(await (await context.request.get(objectPath(child))).body()).toEqual(original);
    expect((await context.request.get(signed.replace(root, child))).status()).toBe(404);
    await record('ui_native_branch_inherited_bucket_objects_and_rejected_parent_download_token', {
      child_id: child,
    });
    await upload(updated, true);
    expect(await (await context.request.get(objectPath(root))).body()).toEqual(original);
    expect(await (await context.request.get(objectPath(child))).body()).toEqual(updated);
    await record('ui_child_overwrite_preserved_parent_bytes');
    const currentETag = `"${digest(updated)}"`;
    const candidateA = Buffer.from('concurrent candidate A');
    const candidateB = Buffer.from('concurrent candidate B');
    const races = await Promise.all(
      [candidateA, candidateB].map((data) =>
        context.request.put(objectPath(child), {
          headers: { ...h, 'Content-Type': 'text/plain', 'If-Match': currentETag },
          data,
        }),
      ),
    );
    expect(races.map((r) => r.status()).sort()).toEqual([200, 412]);
    const raceBytes = await (await context.request.get(objectPath(child))).body();
    expect([digest(candidateA), digest(candidateB)]).toContain(digest(raceBytes));
    await record('real_concurrent_etag_compare_and_swap_admitted_exactly_one_writer');
    expect(
      (
        await context.request.delete(`${branchPath(child)}/buckets/uploads`, { headers: h })
      ).status(),
    ).toBe(409);
    expect(
      (
        await context.request.put(
          `${branchPath(child)}/buckets/uploads/objects?key=bad%2F..%2Fescape`,
          { headers: { ...h, 'If-None-Match': '*' }, data: updated },
        )
      ).status(),
    ).toBe(422);
    expect(
      (
        await context.request.put(`${branchPath(child)}/buckets/uploads/objects?key=oversize`, {
          headers: { ...h, 'If-None-Match': '*' },
          data: Buffer.alloc(8 * 1024 * 1024 + 1),
        })
      ).status(),
    ).toBe(413);
    await record('nonempty_bucket_key_validation_and_upload_size_limits_enforced');
    const oldChildLink = await presign(child);
    await storagePage(child);
    await mutate(child, true);
    expect((await context.request.get(oldChildLink)).status()).toBe(404);
    await mutate(child, false);
    expect((await context.request.get(oldChildLink)).status()).toBe(404);
    expect(await (await context.request.get(await presign(child))).body()).toEqual(raceBytes);
    await record('ui_disable_reenable_preserved_objects_and_invalidated_old_generation_links');
    await load(child);
    const childRow = page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: key, exact: true }) });
    await expect(childRow).toBeVisible({ timeout: 190000 });
    page.once('dialog', (d) => void d.accept());
    await childRow.getByRole('button', { name: '删除', exact: true }).click();
    await expect(page.getByTestId('storage-notice')).toContainText('当前分支对象已删除', {
      timeout: 190000,
    });
    expect(await (await context.request.get(objectPath(root))).body()).toEqual(original);
    page.once('dialog', (d) => void d.accept());
    await page.getByRole('button', { name: '删除空存储桶', exact: true }).click();
    await expect(page.getByTestId('storage-notice')).toContainText('空存储桶已删除', {
      timeout: 190000,
    });
    await record('ui_child_object_and_empty_bucket_deletion_preserved_parent');
    await page.getByLabel('存储桶名称').fill('assets');
    await page.getByLabel('存储桶访问级别').selectOption('public_read');
    await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
    await expect(page.getByTestId('storage-notice')).toContainText('存储桶已创建', {
      timeout: 190000,
    });
    await upload(updated);
    const publicRead = await context.request.get(
      `/storage/v1/${child}/assets?${new URLSearchParams({ key })}`,
    );
    expect(publicRead.status()).toBe(200);
    expect(await publicRead.body()).toEqual(updated);
    expect(publicRead.headers()['content-disposition']).toContain('attachment');
    expect(publicRead.headers()['x-content-type-options']).toBe('nosniff');
    await record('ui_public_read_bucket_allowed_safe_anonymous_download');
    await page.screenshot({ path: testInfo.outputPath('object-storage.png'), fullPage: true });
    await mutate(child, true);
    await storagePage(root);
    await mutate(root, true);
    for (const endpoint of endpoints) await suspend(endpoint);
    await record('ui_disabled_services_and_released_all_owned_compute_preserving_data');
    await page.goto(`/#/projects/${project}/lifecycle`);
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
    await lifecycleAction('解除项目保护', 'ci-' + attempt);
    for (const [id, name] of [
      [root, 'main'],
      [child, 'child-' + attempt],
    ]) {
      const button = page
        .getByTestId('lifecycle-' + id)
        .getByRole('button', { name: '解除分支保护', exact: true });
      if (await button.count()) await lifecycleAction('解除分支保护', name, id);
    }
    await lifecycleAction('删除项目', 'ci-' + attempt);
    expect((await context.request.get(objectPath(root))).status()).toBe(404);
    await record('ui_retained_project_deletion_closed_object_admission');
    await lifecycleAction('恢复项目', 'ci-' + attempt);
    expect((await (await context.request.get(branchPath(root))).json()).state).toBe('disabled');
    await page.goto(`/#/projects/${project}/storage`);
    await mutate(root, false);
    expect(await (await context.request.get(objectPath(root))).body()).toEqual(original);
    await record('ui_retained_project_recovery_and_explicit_reenable_restored_file_bytes');
    await mutate(root, true);
    await suspend(writer);
    await page.goto(`/#/projects/${project}/lifecycle`);
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
    await lifecycleAction('删除项目', 'ci-' + attempt);
    const receipt = await (
      await context.request.get(`/api/v1/projects/${project}/lifecycle`)
    ).json();
    expect(receipt.project.state).toBe('deleted');
    expect(receipt.physical_gc_enabled).toBe(false);
    expect(
      receipt.tombstones.some((t: { physical_gc_state: string }) => t.physical_gc_state === 'held'),
    ).toBe(true);
    await record('ui_released_logical_test_quota_with_retained_tombstone_no_physical_gc');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  } finally {
    if (result === 'fail') {
      testInfo.setTimeout(testInfo.timeout + 120000);
      await retireFailure().catch(() => {});
    }
    await save();
  }
});
