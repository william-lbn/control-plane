import { expect, test, type Response } from '@playwright/test';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Real browser -> Console session -> Go API -> deployed metadata PostgreSQL.
// All opaque tokens stay in process memory; no trace, HAR or secret attachments.
test('Backend credentials: UI lineage, one-time replay, model scope, rotation and revocation', async ({
  page,
  context,
}, testInfo) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const project = process.env.NEON_E2E_CREDENTIAL_PROJECT;
  if (!adminFile || !project) throw new Error('Linux credentials fixture required');
  const attempt = process.env.NEON_E2E_ATTEMPT || String(Date.now());
  const prefix = `/api/v1/projects/${project}`;
  const checks: { name: string; [key: string]: unknown }[] = [];
  const credentialIDs = new Set<string>();
  const branchIDs: string[] = [];
  let result = 'running';
  let root = '';
  await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
  async function record(name: string, details: Record<string, unknown> = {}) {
    checks.push({ name, ...details });
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          branch_ids: branchIDs,
          credential_ids: [...credentialIDs],
          checks,
          credentials_in_report: false,
          inference_tested: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  async function credentials(branch: string) {
    await page.goto(`/#/projects/${project}/credentials`);
    await page.getByLabel('凭据分支', { exact: true }).selectOption(branch);
    await expect(page.getByRole('button', { name: '创建应用凭据', exact: true })).toBeEnabled();
  }
  async function create(name: string, scope: 'self' | 'self_and_descendants', models = '') {
    await page.getByLabel('凭据名称').fill(name);
    await page.getByLabel('凭据分支范围').selectOption(scope);
    await page.getByLabel('凭据模型约束').fill(models);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith('/credentials') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(201);
    const reply = await response.json();
    credentialIDs.add(reply.credential.id);
    const token = await page.getByLabel('新 Backend Token').inputValue();
    expect(/^ncb_[0-9a-f]{24}\.[0-9a-f]{64}$/.test(token)).toBe(true);
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    await expect(page.getByTestId('backend-one-time')).toHaveCount(0);
    return { id: reply.credential.id as string, token, response };
  }
  async function check(token: string, status: number, model = '') {
    await page.getByLabel('检查 Backend Token').fill(token);
    await page.getByLabel('检查模型 ID').fill(model);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith('/credentials/check') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '检查应用凭据', exact: true }).click();
    expect((await pending).status()).toBe(status);
    await expect(page.getByTestId('backend-check-result')).toContainText(
      status === 200 ? '授权通过' : '授权未通过',
    );
    await page.getByLabel('检查 Backend Token').fill('');
  }
  async function dataBranch(parent: string, name: string) {
    await page.goto(`/#/projects/${project}/branches`);
    await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
    await page.getByLabel('分支名称').fill(name);
    await page.getByLabel('父分支').selectOption(parent);
    await page.getByLabel('同时创建读写 Compute Endpoint').uncheck();
    const pending = page.waitForResponse(
      (r) => r.url().endsWith(prefix + '/branches') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const reply = await response.json();
    branchIDs.push(reply.resource.id);
    await expect(page.locator('.create-modal')).toHaveCount(0, { timeout: 490000 });
    await record('ui_data_only_branch_ready', {
      branch_id: reply.resource.id,
      operation_id: reply.operation.id,
    });
    return reply.resource.id as string;
  }
  async function replay(response: Response) {
    const headers = response.request().headers();
    const repeated = await context.request.post(new URL(response.url()).pathname, {
      headers: {
        'X-CSRF-Token': headers['x-csrf-token'],
        'Idempotency-Key': headers['idempotency-key'],
      },
      data: response.request().postDataJSON(),
    });
    expect(repeated.status()).toBe(200);
    expect(repeated.headers()['idempotency-replayed']).toBe('true');
    const value = await repeated.json();
    expect(Boolean(value.api_token)).toBe(false);
    expect(value.secret_available).toBe(false);
    const changed = await context.request.post(new URL(response.url()).pathname, {
      headers: {
        'X-CSRF-Token': headers['x-csrf-token'],
        'Idempotency-Key': headers['idempotency-key'],
      },
      data: { ...response.request().postDataJSON(), name: 'conflicting-input' },
    });
    expect(changed.status()).toBe(409);
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const fixture = await context.request.get(prefix);
    expect(fixture.status()).toBe(200);
    expect((await fixture.json()).name.startsWith('ci-')).toBe(true);
    const branches = await context.request.get(prefix + '/branches');
    expect(branches.status()).toBe(200);
    root = (await branches.json()).items.find((b: { is_default: boolean }) => b.is_default).id;
    const child = await dataBranch(root, 'credentials-child-' + attempt);
    const sibling = await dataBranch(root, 'credentials-sibling-' + attempt);
    await credentials(root);
    const parent = await create('ancestor-' + attempt, 'self_and_descendants', 'model-a');
    await replay(parent.response);
    await record('one_time_ui_token_and_durable_replay_no_plaintext');
    await check(parent.token, 200, 'model-a');
    await check(parent.token, 403, 'model-b');
    await credentials(child);
    await check(parent.token, 200, 'model-a');
    const childCredential = await create('child-' + attempt, 'self_and_descendants');
    await check(childCredential.token, 200);
    await credentials(sibling);
    await check(childCredential.token, 403);
    await check(parent.token, 200, 'model-a');
    await credentials(root);
    await check(childCredential.token, 403);
    await record('ui_parent_descendants_allowed_child_parent_sibling_denied');
    const self = await create('self-' + attempt, 'self');
    await credentials(child);
    await check(self.token, 403);
    await credentials(root);
    await check(self.token, 200);
    await record('ui_self_scope_cannot_reach_child');
    const row = page.getByTestId(`backend-row-${parent.id}`);
    const rotating = page.waitForResponse(
      (r) =>
        r.url().endsWith(`/credentials/${parent.id}/rotate`) && r.request().method() === 'POST',
    );
    await row.getByRole('button', { name: '轮换凭据', exact: true }).click();
    expect((await rotating).status()).toBe(200);
    const rotated = await page.getByLabel('新 Backend Token').inputValue();
    expect(rotated !== parent.token).toBe(true);
    await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    await check(parent.token, 401, 'model-a');
    await check(rotated, 200, 'model-a');
    const revoking = page.waitForResponse(
      (r) => r.url().endsWith(`/credentials/${parent.id}`) && r.request().method() === 'DELETE',
    );
    await row.getByRole('button', { name: '撤销凭据', exact: true }).click();
    expect((await revoking).status()).toBe(200);
    await check(rotated, 401, 'model-a');
    await record('ui_rotation_old_token_denied_revocation_immediate');
    await page.reload();
    await expect(page.getByTestId('backend-one-time')).toHaveCount(0);
    await expect(page.getByTestId(`backend-row-${parent.id}`)).toContainText('已撤销');
    await page.screenshot({
      path: testInfo.outputPath('backend-credentials.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]'), page.getByLabel('新 Backend Token')],
    });
    await record('ui_metadata_reload_never_recovers_plaintext');
    result = 'pass';
  } finally {
    // Scoped cleanup revokes only credentials minted by this test; history and
    // data-only branches remain. No database or shared project is deleted.
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value || '';
    for (const branch of [root, ...branchIDs].filter(Boolean)) {
      const listing = await context.request.get(`${prefix}/branches/${branch}/credentials`);
      if (listing.status() !== 200) continue;
      for (const c of (await listing.json()).items)
        if (credentialIDs.has(c.id) && c.state !== 'revoked') {
          const revoked = await context.request.delete(
            `${prefix}/branches/${branch}/credentials/${c.id}`,
            {
              headers: {
                'X-CSRF-Token': csrf,
                'If-Match': `"${c.generation}"`,
                'Idempotency-Key': 'cleanup-' + c.id + '-' + attempt,
              },
            },
          );
          expect(revoked.status()).toBe(200);
        }
    }
    if (result !== 'pass') result = 'fail';
    await record('owned_credentials_revoked_records_retained');
  }
});
