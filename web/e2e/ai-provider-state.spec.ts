import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test('Owned provider Responses and Conversations finish without duplicate mutations', async ({
  page,
  context,
}, info) => {
  test.setTimeout(240000);
  page.setDefaultTimeout(20000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (process.platform !== 'linux' || !fixtureFile || !admin || !privateDir)
    throw Error('Protected Linux state fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    fixture.synthetic !== true ||
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.sibling_id) ||
    !/^ci-state-[a-f0-9]+$/.test(fixture.model_id) ||
    !/^http:\/\/10\.43\.[0-9]+\.[0-9]+:9081$/.test(fixture.upstream_url)
  )
    throw Error('Exact owned provider-state scope required');
  type State = {
    result: string;
    checks: string[];
    conversation?: string;
    create?: { key: string; body: unknown };
    response_json?: string;
    response_stream?: string;
    requests: string[];
    inference_pending?: string;
  };
  const state: State = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : { result: 'running', checks: [], requests: [] };
  state.result = 'running';
  const prefix = `/api/v1/projects/${fixture.project_id}/branches/${fixture.branch_id}/ai-gateway`,
    root = prefix + '/state/v1/';
  const headers = async () => ({
    'X-Neon-Model': fixture.model_id,
    'X-CSRF-Token': decodeURIComponent(
      (await context.cookies()).find((v) => v.name === 'neon_v2_csrf')?.value || '',
    ),
  });
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 'ai-provider-state-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result: state.result,
        checks: state.checks,
        project_id: fixture.project_id,
        branch_id: fixture.branch_id,
        response_ids: [state.response_json, state.response_stream].filter(Boolean),
        conversation_id: state.conversation,
        request_ids: state.requests,
        synthetic_supplier: true,
        commercial_model_calls: false,
        credentials_exported: false,
        compute_created: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function get(url: string) {
    const response = await context.request.get(url, { headers: await headers() });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function handles(): Promise<
    { id: string; kind: string; model_id: string; state: string }[]
  > {
    return (await get(prefix + '/provider-state')).items;
  }
  async function select(id: string) {
    await page.getByLabel('持久上下文', { exact: true }).selectOption(id);
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
    await page.getByLabel('AI 模型', { exact: true }).selectOption(fixture.model_id);
    await expect(page.getByLabel('AI 协议', { exact: true })).toHaveValue('responses');
    await expect(page.getByLabel('保存本次响应', { exact: true })).toBeVisible();
    if (!state.checks.includes('gui_lost_creation_replays_original_receipt')) {
      if (!state.create) {
        const pattern = '**' + root + 'conversations';
        await page.route(pattern, async (route) => {
          if (route.request().method() !== 'POST' || state.create) return route.continue();
          state.create = {
            key: route.request().headers()['idempotency-key'],
            body: route.request().postDataJSON(),
          };
          await save();
          const original = await route.fetch({ maxRetries: 0, maxRedirects: 0 });
          expect(original.status()).toBe(200);
          state.conversation = (await original.json()).id;
          await save();
          await route.abort('failed');
        });
        await page.getByRole('button', { name: '创建持久会话', exact: true }).click();
        await expect(
          page.getByRole('button', { name: '读取原操作回执', exact: true }),
        ).toBeVisible();
        await page.unroute(pattern);
        const replay = page.waitForResponse(
          (r) => r.url().endsWith(root + 'conversations') && r.request().method() === 'POST',
        );
        await page.getByRole('button', { name: '读取原操作回执', exact: true }).click();
        expect((await replay).headers()['idempotency-replayed']).toBe('true');
      } else {
        const replay = await context.request.post(root + 'conversations', {
          headers: { ...(await headers()), 'Idempotency-Key': state.create.key },
          data: state.create.body,
        });
        expect(replay.status()).toBe(200);
        expect(replay.headers()['idempotency-replayed']).toBe('true');
        state.conversation = (await replay.json()).id;
      }
      expect(
        (await handles()).filter(
          (v) => v.model_id === fixture.model_id && v.kind === 'conversation',
        ),
      ).toHaveLength(1);
      await save('gui_lost_creation_replays_original_receipt');
    }
    await select(state.conversation!);
    if (!state.checks.includes('gui_items_append_read_delete_parent_receipt')) {
      await page
        .getByLabel('追加会话文本', { exact: true })
        .fill('Synthetic owned conversation item');
      const append = page.waitForResponse(
        (r) => r.url().endsWith('/items') && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '追加会话条目', exact: true }).click();
      expect((await append).status()).toBe(200);
      await expect(page.getByRole('button', { name: '删除会话条目', exact: true })).toHaveCount(1);
      const remove = page.waitForResponse(
        (r) => r.url().includes('/items/nit_') && r.request().method() === 'DELETE',
      );
      await page.getByRole('button', { name: '删除会话条目', exact: true }).click();
      const deleted = await remove;
      expect(deleted.status()).toBe(200);
      expect((await deleted.json()).id).toBe(state.conversation);
      expect(
        (await get(root + 'conversations/' + state.conversation + '/items')).data,
      ).toHaveLength(0);
      await save('gui_items_append_read_delete_parent_receipt');
    }
    for (const mode of ['json', 'stream'] as const) {
      const check = 'gui_response_' + mode + '_scoped_history_and_usage';
      if (state.checks.includes(check)) continue;
      if (
        state.inference_pending === 'stream' &&
        mode === 'stream' &&
        fixture.original_inference_observation
      ) {
        const original = fixture.original_inference_observation;
        expect(original.project_id).toBe(fixture.project_id);
        expect(original.branch_id).toBe(fixture.branch_id);
        expect(original.model_id).toBe(fixture.model_id);
        expect(original.previous_response_id).toBe(state.response_json);
        expect(original.streaming).toBe(true);
        expect(original.provider_status).toBe('completed');
        const usage = (await get(prefix + '/usage')).items.find(
          (value: { id: string }) => value.id === original.request_id,
        );
        expect(usage.state).toBe('completed');
        expect(usage.total_tokens).toBe(11);
        const retained = await get(root + 'responses/' + original.id);
        expect(retained.id).toBe(original.id);
        expect(retained.previous_response_id).toBe(state.response_json);
        expect(JSON.stringify(retained)).not.toContain('resp_fixture');
        expect(JSON.stringify(retained)).not.toContain('_neon_binding');
        state.response_stream = original.id;
        if (!state.requests.includes(original.request_id)) state.requests.push(original.request_id);
        delete state.inference_pending;
        await save(check);
        continue;
      }
      if (state.inference_pending)
        throw Error('Original inference requires observation; never replay a pending inference');
      await select(mode === 'json' ? state.conversation! : state.response_json!);
      await page.getByLabel('保存本次响应', { exact: true }).check();
      if (mode === 'stream') await page.getByLabel('AI 流式输出', { exact: true }).check();
      else await page.getByLabel('AI 流式输出', { exact: true }).uncheck();
      await page.getByLabel('AI 输入', { exact: true }).fill('Synthetic owned state ' + mode);
      state.inference_pending = mode;
      await save();
      let capturedStream = '';
      const streamPattern = '**' + prefix + '/request/responses';
      if (mode === 'stream')
        await page.route(streamPattern, async (route) => {
          const original = await route.fetch({ maxRetries: 0, maxRedirects: 0 });
          capturedStream = (await original.body()).toString();
          await route.fulfill({ response: original });
        });
      const inference = page.waitForResponse(
        (r) => r.url().endsWith(prefix + '/request/responses') && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '发送到模型', exact: true }).click();
      const response = await inference;
      expect(response.status()).toBe(200);
      const raw = mode === 'stream' ? capturedStream : (await response.body()).toString();
      if (mode === 'stream') await page.unroute(streamPattern);
      expect(raw).not.toContain('resp_fixture');
      expect(raw).not.toContain('conv_fixture');
      expect(raw).not.toContain('msg_fixture');
      expect(raw).not.toContain('_neon_binding');
      const id =
        mode === 'json'
          ? JSON.parse(raw).id
          : JSON.parse(
              raw
                .split('\n')
                .find((line) => line.startsWith('data:') && line.includes('response.created'))!
                .slice(5),
            ).response.id;
      expect(id).toMatch(/^nrp_[a-f0-9]{24}$/);
      state[mode === 'json' ? 'response_json' : 'response_stream'] = id;
      state.requests.push(response.headers()['x-request-id']);
      delete state.inference_pending;
      await save();
      await expect(page.getByRole('button', { name: '发送到模型', exact: true })).toBeEnabled();
      const usage = (await get(prefix + '/usage')).items.find(
        (v: { id: string }) => v.id === state.requests.at(-1),
      );
      expect(usage.total_tokens).toBe(11);
      expect(usage.state).toBe('completed');
      expect((await get(root + 'responses/' + id)).id).toBe(id);
      await save(check);
    }
    if (!state.checks.includes('foreign_reference_denied_without_supplier_call')) {
      const before = await get(fixture.upstream_url + '/stats');
      const response = await context.request.post(
        prefix.replace(fixture.branch_id, fixture.sibling_id) + '/request/responses',
        {
          headers: await headers(),
          data: {
            model: fixture.model_id,
            input: 'Synthetic sibling denial',
            max_output_tokens: 8,
            store: false,
            previous_response_id: state.response_json,
          },
        },
      );
      expect(response.status()).toBe(404);
      expect((await get(fixture.upstream_url + '/stats')).calls).toEqual(before.calls);
      await save('foreign_reference_denied_without_supplier_call');
    }
    if (!state.checks.includes('owned_supplier_resources_deleted_history_retained')) {
      for (const h of (await handles()).filter(
        (v) => v.model_id === fixture.model_id && v.state !== 'deleted',
      )) {
        const path = root + (h.kind === 'response' ? 'responses/' : 'conversations/') + h.id;
        if (h.kind === 'conversation')
          for (const item of (await get(path + '/items')).data) {
            const response = await context.request.delete(
              path + '/items/' + encodeURIComponent(item.id),
              {
                headers: {
                  ...(await headers()),
                  'Idempotency-Key': 'state-close-item-' + item.id.slice(-60),
                },
              },
            );
            expect(response.status()).toBe(200);
          }
        const response = await context.request.delete(path, {
          headers: { ...(await headers()), 'Idempotency-Key': 'state-close-' + h.id },
        });
        expect(response.status()).toBe(200);
        expect((await context.request.get(path, { headers: await headers() })).status()).toBe(410);
      }
      expect(
        (await handles())
          .filter((v) => v.model_id === fixture.model_id)
          .every((v) => v.state === 'deleted'),
      ).toBe(true);
      await save('owned_supplier_resources_deleted_history_retained');
    }
    state.result = 'pass';
    await save();
  } finally {
    await save();
  }
});
