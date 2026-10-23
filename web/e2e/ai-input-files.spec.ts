import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Branch input files preserve original uploads and native supplier scope', async ({
  page,
  context,
}, info) => {
  test.setTimeout(300000);
  page.setDefaultTimeout(15000);
  page.setDefaultNavigationTimeout(20000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !fixtureFile || !admin || !privateDir)
    throw Error('Protected Linux input fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    fixture.synthetic !== true ||
    !/^http:\/\/10\.43\.[0-9]+\.[0-9]+:9081$/.test(fixture.upstream_url) ||
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.parent_branch_id) ||
    !Array.isArray(fixture.assets) ||
    fixture.assets.length !== 3
  )
    throw Error('Exact synthetic input scope required');
  type Asset = { filename: string; content_type: string; base64: string; sha256: string };
  type FileMeta = {
    id: string;
    filename: string;
    sha256: string;
    content_type: string;
    bytes: number;
  };
  type State = {
    result: string;
    checks: string[];
    files: Record<string, FileMeta>;
    pending: Record<string, { key: string; id?: string }>;
    requests: Record<string, string>;
    gui_retry: boolean;
    probe_files: FileMeta[];
  };
  const state: State = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : {
        result: 'running',
        checks: [],
        files: {},
        pending: {},
        requests: {},
        gui_retry: false,
        probe_files: [],
      };
  state.result = 'running';
  const ai = `/api/v1/projects/${fixture.project_id}/branches/${fixture.branch_id}/ai-gateway`,
    files = ai + '/files';
  const assets: Asset[] = fixture.assets;
  for (const a of assets)
    expect(createHash('sha256').update(Buffer.from(a.base64, 'base64')).digest('hex')).toBe(
      a.sha256,
    );
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'ai-input-files-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result: state.result,
        checks: state.checks,
        file_ids: [...Object.values(state.files), ...state.probe_files].map((v) => v.id),
        request_ids: state.requests,
        synthetic_files: true,
        synthetic_upstream: true,
        customer_files_used: false,
        commercial_model_call: false,
        compute_created: false,
        credentials_exported: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function get(url: string) {
    const r = await context.request.get(url);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function listed(): Promise<FileMeta[]> {
    return (await get(files)).data;
  }
  async function csrf() {
    return decodeURIComponent(
      (await context.cookies()).find((v) => v.name === 'neon_v2_csrf')?.value || '',
    );
  }
  async function uploadUI(asset: Asset, lose = false, probe = false) {
    const existing = (await listed()).filter(
      (v) => v.filename === asset.filename && v.sha256 === asset.sha256,
    );
    expect(existing.length).toBeLessThanOrEqual(1);
    if (existing.length) {
      if (probe) {
        if (!state.probe_files.some((v) => v.id === existing[0].id))
          state.probe_files.push(existing[0]);
      } else state.files[asset.filename] = existing[0];
      const pending = state.pending[asset.filename];
      if (pending) {
        const replay = await context.request.post(files, {
          headers: {
            'X-CSRF-Token': await csrf(),
            'Idempotency-Key': pending.key,
            'X-Neon-File-TTL': '86400',
          },
          multipart: {
            purpose: 'user_data',
            file: {
              name: asset.filename,
              mimeType: asset.content_type,
              buffer: Buffer.from(asset.base64, 'base64'),
            },
          },
        });
        expect(replay.status()).toBe(201);
        expect(replay.headers()['idempotency-replayed']).toBe('true');
        expect((await replay.json()).id).toBe(existing[0].id);
      }
      await save();
      return;
    }
    await page.getByLabel('选择 AI 输入文件', { exact: true }).setInputFiles({
      name: asset.filename,
      mimeType: asset.content_type,
      buffer: Buffer.from(asset.base64, 'base64'),
    });
    await page.getByLabel('输入文件保留秒数', { exact: true }).fill('86400');
    const pattern = '**' + files;
    if (lose) {
      let captured = false;
      await page.route(pattern, async (route) => {
        if (captured || route.request().method() !== 'POST') {
          await route.continue();
          return;
        }
        captured = true;
        state.pending[asset.filename] = { key: route.request().headers()['idempotency-key'] };
        await save();
        const response = await route.fetch();
        expect(response.status()).toBe(201);
        const meta: FileMeta = await response.json();
        state.pending[asset.filename].id = meta.id;
        if (probe) state.probe_files.push(meta);
        else state.files[asset.filename] = meta;
        await save();
        await route.abort('failed');
      });
      await page.getByRole('button', { name: '上传 AI 输入文件', exact: true }).click();
      await expect(
        page.getByRole('button', { name: '重试原输入文件提交', exact: true }),
      ).toBeVisible();
      await page.unroute(pattern);
      const pending = page.waitForResponse(
        (r) => r.request().method() === 'POST' && r.url().endsWith(files),
      );
      await page.getByRole('button', { name: '重试原输入文件提交', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(201);
      expect(response.headers()['idempotency-replayed']).toBe('true');
      expect(response.request().headers()['idempotency-key']).toBe(
        state.pending[asset.filename].key,
      );
      const meta = await response.json();
      expect(meta.id).toBe(state.pending[asset.filename].id);
      state.gui_retry = true;
      await save();
    } else {
      const pending = page.waitForResponse(
        (r) => r.request().method() === 'POST' && r.url().endsWith(files),
      );
      await page.getByRole('button', { name: '上传 AI 输入文件', exact: true }).click();
      const response = await pending;
      expect(response.status()).toBe(201);
      const meta = await response.json();
      if (probe) state.probe_files.push(meta);
      else state.files[asset.filename] = meta;
      await save();
    }
    await expect(page.getByLabel('附加文件 ' + asset.filename, { exact: true })).toBeVisible();
    expect(
      (await listed()).filter((v) => v.filename === asset.filename && v.sha256 === asset.sha256),
    ).toHaveLength(1);
  }
  await save();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByLabel('切换组织', { exact: true })).toBeVisible();
    const org = (await get('/api/v1/organizations')).items.find(
      (v: { id: string }) => v.id === fixture.org_id,
    );
    expect(org).toBeTruthy();
    await page.getByLabel('切换组织', { exact: true }).click();
    await page.getByRole('option', { name: org.name + ' · ' + org.role, exact: true }).click();
    await page.goto(`/#/projects/${fixture.project_id}/ai-gateway`);
    await page.getByLabel('AI 分支', { exact: true }).selectOption(fixture.branch_id);
    await expect(page.getByRole('region', { name: 'AI 输入文件', exact: true })).toBeVisible();
    if (!state.checks.includes('upload_commit_replays_original_key_without_duplicate')) {
      for (const asset of assets) {
        const exists = (await listed()).some(
          (v) => v.filename === asset.filename && v.sha256 === asset.sha256,
        );
        await uploadUI(asset, !state.gui_retry && !exists);
      }
      for (let attempt = 0; !state.gui_retry && attempt < 3; attempt++) {
        const probe = {
          ...assets[0],
          filename: 'recovery-' + state.probe_files.length + '-' + assets[0].filename,
        };
        await uploadUI(probe, true, true);
      }
      expect(state.gui_retry).toBe(true);
      await save('upload_commit_replays_original_key_without_duplicate');
    }
    if (!state.checks.includes('downloads_preserve_exact_media_and_sha')) {
      for (const asset of assets) {
        const stored = state.files[asset.filename];
        expect(stored).toBeTruthy();
        const response = await context.request.get(files + '/' + stored.id + '/content');
        expect(response.status()).toBe(200);
        expect(response.headers()['cache-control']).toBe('no-store');
        expect(response.headers()['content-type']).toBe(asset.content_type);
        expect(
          createHash('sha256')
            .update(await response.body())
            .digest('hex'),
        ).toBe(asset.sha256);
      }
      await save('downloads_preserve_exact_media_and_sha');
    }
    if (!state.checks.includes('foreign_branch_reference_denied_before_supplier')) {
      const parent = `/api/v1/projects/${fixture.project_id}/branches/${fixture.parent_branch_id}/ai-gateway`;
      const asset = assets.find((v) => v.content_type === 'application/pdf')!;
      const response = await context.request.post(parent + '/request/responses', {
        headers: { 'X-CSRF-Token': await csrf() },
        data: {
          model: fixture.model_ids.responses,
          input: [
            {
              role: 'user',
              content: [{ type: 'input_file', file_id: state.files[asset.filename].id }],
            },
          ],
          max_output_tokens: 8,
          store: false,
        },
      });
      expect(response.status()).toBe(422);
      const stats = await get(fixture.upstream_url + '/stats');
      expect(Object.values(stats.calls).reduce((a: any, b: any) => a + b, 0)).toBe(0);
      await save('foreign_branch_reference_denied_before_supplier');
    }
    for (const protocol of ['responses', 'messages', 'gemini', 'chat_completions']) {
      const check = protocol + '_native_files_confirmed_usage';
      if (state.checks.includes(check)) continue;
      const model = fixture.model_ids[protocol];
      const prior = (await get(ai + '/usage')).items.filter(
        (v: { model_id: string }) => v.model_id === model,
      );
      expect(prior.length).toBeLessThanOrEqual(1);
      if (!prior.length) {
        expect(state.requests[protocol]).toBeUndefined();
        await page.getByLabel('AI 模型', { exact: true }).selectOption(model);
        await expect(page.getByLabel('AI 协议', { exact: true })).toHaveValue(protocol);
        const stream = page.getByLabel('AI 流式输出', { exact: true });
        if (await stream.isEnabled()) await stream.uncheck();
        await page
          .getByLabel('AI 输入', { exact: true })
          .fill('Synthetic native input file acceptance');
        const wanted = assets.filter(
          (v) =>
            v.content_type === 'image/png' ||
            v.content_type === (protocol === 'chat_completions' ? 'audio/wav' : 'application/pdf'),
        );
        for (const asset of wanted) {
          const checkbox = page.getByLabel('附加文件 ' + asset.filename, { exact: true });
          await expect(checkbox).toBeEnabled();
          await checkbox.check();
          await expect(checkbox).toBeChecked();
        }
        const button = page.getByRole('button', { name: '发送到模型', exact: true });
        await expect(button).toBeEnabled();
        const pending = page.waitForResponse(
          (r) =>
            r.request().method() === 'POST' &&
            r
              .url()
              .endsWith(ai + '/request' + (protocol === 'chat_completions' ? '' : '/' + protocol)),
        );
        await button.click();
        const response = await pending;
        state.requests[protocol] = response.headers()['x-request-id'];
        await save();
        expect(response.status()).toBe(200);
        expect(state.requests[protocol]).toMatch(/^air_[a-f0-9]{24}$/);
        expect(await response.text()).toContain('Synthetic input files accepted');
      } else {
        expect(prior[0].state).toBe('completed');
        expect(prior[0].total_tokens).toBe(5);
        if (state.requests[protocol]) expect(prior[0].id).toBe(state.requests[protocol]);
        state.requests[protocol] = prior[0].id;
      }
      await expect
        .poll(
          async () =>
            (await get(ai + '/usage')).items.find(
              (v: { id: string }) => v.id === state.requests[protocol],
            )?.total_tokens,
        )
        .toBe(5);
      const stats = await get(fixture.upstream_url + '/stats');
      expect(stats.calls[protocol]).toBe(1);
      expect(stats.rejected).toBe(0);
      await save(check);
    }
    if (!state.checks.includes('deleted_inputs_unavailable_confirmed_usage_retained')) {
      for (const file of [...Object.values(state.files), ...state.probe_files]) {
        const available = (await listed()).some((v) => v.id === file.id);
        if (available) {
          const pending = page.waitForResponse(
            (r) => r.request().method() === 'DELETE' && r.url().endsWith(files + '/' + file.id),
          );
          await page
            .getByRole('button', { name: '删除输入文件 ' + file.filename, exact: true })
            .click();
          expect((await pending).status()).toBe(200);
          await expect(page.getByLabel('附加文件 ' + file.filename, { exact: true })).toHaveCount(
            0,
          );
        }
        expect((await context.request.get(files + '/' + file.id + '/content')).status()).toBe(404);
      }
      const usages = (await get(ai + '/usage')).items;
      for (const id of Object.values(state.requests))
        expect(usages.find((v: { id: string }) => v.id === id)?.total_tokens).toBe(5);
      const stats = await get(fixture.upstream_url + '/stats');
      expect(stats.calls).toEqual({ responses: 1, messages: 1, gemini: 1, chat_completions: 1 });
      expect(stats.rejected).toBe(0);
      await save('deleted_inputs_unavailable_confirmed_usage_retained');
    }
    expect(state.checks).toHaveLength(8);
    state.result = 'pass';
    await save();
  } catch (e) {
    state.result = 'fail';
    await save();
    throw e;
  }
});
