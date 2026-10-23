import { expect, test, request as playwrightRequest } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

test('S3 settings UI, original receipts, browser POST and actual SDK metadata remain isolated', async ({
  page,
  context,
}, info) => {
  page.setDefaultTimeout(30000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    probe = process.env.NEON_E2E_S3_PROBE;
  if (!fixtureFile || !adminFile || !privateDir || !probe)
    throw Error('Protected retained storage branch and Linux SDK required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database: string;
    resume_file?: string;
  };
  if (
    f.project_id !== 'prj_6f3c51bd15b772e3' ||
    f.branch_id !== 'br_6302722fb381d460' ||
    f.database !== 'owner_transfer_v1'
  )
    throw Error('Exact retained storage scope required');
  type State = {
    project_id: string;
    branch_id: string;
    bucket: string;
    checks: string[];
    stage: string;
    credential: null | { token_id: string; s3_secret_access_key: string };
    operations: string[];
  };
  const state: State = f.resume_file
    ? JSON.parse(await readFile(f.resume_file, 'utf8'))
    : {
        project_id: f.project_id,
        branch_id: f.branch_id,
        bucket: 'ci-s3-settings-' + Date.now(),
        checks: [],
        stage: '',
        credential: null,
        operations: [],
      };
  if (
    state.project_id !== f.project_id ||
    state.branch_id !== f.branch_id ||
    !/^ci-s3-settings-\d+$/.test(state.bucket)
  )
    throw Error('Original settings checkpoint differs');
  const base = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}`,
    bucketPath = base + '/storage/buckets/' + state.bucket,
    bucketSettings = bucketPath + '/settings',
    objectSettings = bucketPath + '/objects/settings?key=ui%2Fsettings.txt',
    payload = Buffer.from('Owned UI S3 settings bytes retained.\n');
  let result = 'running';
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 's3-settings-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: f.project_id,
        branch_id: f.branch_id,
        bucket: state.bucket,
        checks: state.checks,
        stage: state.stage,
        credential_id: state.credential?.token_id,
        operations: state.operations,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  async function step(name: string, run: () => Promise<void>) {
    if (state.checks.includes(name)) return;
    state.stage = name;
    await save();
    await run();
    await save(name);
  }
  async function selectBucket() {
    await page.goto(`/#/projects/${f.project_id}/storage`);
    await page.getByLabel('存储分支', { exact: true }).selectOption(f.branch_id);
    await expect(page.getByLabel('存储数据库', { exact: true })).toHaveValue(f.database);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: state.bucket, exact: true }) })
      .getByRole('button', { name: '选择', exact: true })
      .click();
  }
  async function follow(id: string) {
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${f.project_id}/operations/${id}`);
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Original settings operation failed ' + op.error_code);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  const anon = await playwrightRequest.newContext({ baseURL: 'http://192.168.146.100:30893' });
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    await step('owned_new_bucket_created_on_previously_verified_active_storage', async () => {
      const service = await context.request.get(base + '/storage');
      expect(service.status()).toBe(200);
      expect((await service.json()).state).toBe('active');
      await page.goto(`/#/projects/${f.project_id}/storage`);
      await page.getByLabel('存储分支', { exact: true }).selectOption(f.branch_id);
      await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
      const buckets = await context.request.get(base + '/storage/buckets');
      expect(buckets.status()).toBe(200);
      if ((await buckets.json()).items.some((v: { name: string }) => v.name === state.bucket))
        throw Error('Inspect original bucket creation before repeating');
      await page.getByLabel('存储桶名称', { exact: true }).fill(state.bucket);
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/storage/buckets' &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建存储桶', exact: true }).click();
      expect((await pending).status()).toBe(201);
    });
    await selectBucket();
    const bucketPanel = page.getByRole('region', { name: '桶设置', exact: true });
    await bucketPanel.getByRole('button', { name: '加载桶设置', exact: true }).click();
    await step(
      'UI_bucket_tags_stored_configuration_and_original_key_retry_preserve_successor',
      async () => {
        await bucketPanel
          .getByLabel('桶标签 JSON', { exact: true })
          .fill('[{"key":"phase","value":"ui-original"}]');
        await bucketPanel.getByLabel('桶版本配置声明').selectOption('Suspended');
        await bucketPanel
          .getByLabel('桶生命周期配置 JSON')
          .fill(
            '{"rules":[{"id":"ui-stored","status":"Disabled","filter":{"prefix":"ui/"},"expiration":{"days":30}}]}',
          );
        await bucketPanel
          .getByLabel('桶 Object Lock 配置 JSON')
          .fill('{"object_lock_enabled":"Enabled"}');
        await expect(bucketPanel).toContainText('不执行对象版本、自动过期或 WORM 保留');
        let intercepted = false;
        let original: { key: string; match: string; body: string } | null = null;
        await page.route('**' + bucketSettings, async (route) => {
          const r = route.request();
          if (r.method() !== 'PUT') return route.continue();
          if (intercepted) {
            expect(r.headers()['idempotency-key']).toBe(original?.key);
            expect(r.headers()['if-match']).toBe(original?.match);
            expect(r.postData()).toBe(original?.body);
            return route.continue();
          }
          intercepted = true;
          original = {
            key: r.headers()['idempotency-key'],
            match: r.headers()['if-match'],
            body: r.postData() || '',
          };
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          await route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({
              error: { code: 'synthetic_reply_loss', message: 'Owned response loss after commit' },
            }),
          });
        });
        await bucketPanel.getByRole('button', { name: '保存桶设置', exact: true }).click();
        await expect(bucketPanel.getByRole('button', { name: '重试原桶设置提交' })).toBeVisible();
        await expect(bucketPanel.getByLabel('桶标签 JSON')).toBeDisabled();
        const current = await context.request.get(bucketSettings);
        expect(current.status()).toBe(200);
        const value = await current.json();
        value.settings.tags = [{ key: 'phase', value: 'ui-successor' }];
        const successor = await context.request.put(bucketSettings, {
          headers: {
            'X-CSRF-Token': csrf,
            'Idempotency-Key': state.bucket + '-successor',
            'If-Match': current.headers().etag,
          },
          data: { access: value.access, settings: value.settings },
        });
        expect(successor.status()).toBe(200);
        await bucketPanel.getByRole('button', { name: '重试原桶设置提交' }).click();
        await expect(bucketPanel.getByLabel('桶标签 JSON')).toHaveValue(/ui-successor/);
        await page.unroute('**' + bucketSettings);
        expect(original).not.toBeNull();
      },
    );
    await step('settings_API_rejects_stale_missing_and_key_reuse_conflicts', async () => {
      const current = await context.request.get(bucketSettings);
      expect(current.status()).toBe(200);
      const value = await current.json();
      const headers = { 'X-CSRF-Token': csrf, 'Idempotency-Key': state.bucket + '-invalid' };
      expect(
        (
          await context.request.put(bucketSettings, {
            headers,
            data: { access: value.access, settings: value.settings },
          })
        ).status(),
      ).toBe(428);
      expect(
        (
          await context.request.put(bucketSettings, {
            headers: { ...headers, 'If-Match': '"' + '0'.repeat(64) + '"' },
            data: { access: value.access, settings: value.settings },
          })
        ).status(),
      ).toBe(412);
      expect(
        (
          await context.request.put(bucketSettings, {
            headers: { ...headers, 'If-Match': current.headers().etag },
            data: { access: value.access },
          })
        ).status(),
      ).toBe(422);
      const changed = {
        access: value.access,
        settings: { ...value.settings, tags: [{ key: 'phase', value: 'changed-original' }] },
      };
      expect(
        (
          await context.request.put(bucketSettings, {
            headers: {
              ...headers,
              'Idempotency-Key': state.bucket + '-successor',
              'If-Match': current.headers().etag,
            },
            data: changed,
          })
        ).status(),
      ).toBe(409);
    });
    await step('new_owned_UI_object_uploaded_once', async () => {
      const resource = bucketPath + '/objects?key=ui%2Fsettings.txt';
      expect((await context.request.head(resource)).status()).toBe(404);
      await page.getByLabel('对象键', { exact: true }).fill('ui/settings.txt');
      await page
        .getByLabel('上传文件', { exact: true })
        .setInputFiles({ name: 'settings.txt', mimeType: 'text/plain', buffer: payload });
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === bucketPath + '/objects' && r.request().method() === 'PUT',
      );
      await page.getByRole('button', { name: '上传对象', exact: true }).click();
      expect((await pending).status()).toBe(200);
    });
    await page.getByRole('button', { name: '加载对象', exact: true }).click();
    await page.getByRole('button', { name: '对象设置 ui/settings.txt', exact: true }).click();
    const objectPanel = page.getByRole('region', { name: '对象设置', exact: true });
    await objectPanel.getByRole('button', { name: '加载对象设置' }).click();
    await step('UI_object_tags_response_headers_CAS_and_original_bytes', async () => {
      await objectPanel.getByLabel('对象标签 JSON').fill('[{"key":"phase","value":"ui-object"}]');
      await objectPanel
        .getByLabel('对象响应头 JSON')
        .fill(
          '{"content-disposition":"attachment; filename=ui-settings.txt","cache-control":"private, max-age=9"}',
        );
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === bucketPath + '/objects/settings' &&
          r.request().method() === 'PUT',
      );
      await objectPanel.getByRole('button', { name: '保存对象设置' }).click();
      expect((await pending).status()).toBe(200);
      await expect(objectPanel.getByLabel('对象标签 JSON')).toHaveValue(/ui-object/);
      const current = await context.request.get(objectSettings);
      expect(current.status()).toBe(200);
      const value = await current.json();
      expect(value.settings.tags).toEqual([{ key: 'phase', value: 'ui-object' }]);
      const invalid = await context.request.put(objectSettings, {
        headers: {
          'X-CSRF-Token': csrf,
          'Idempotency-Key': state.bucket + '-object-header',
          'If-Match': current.headers().etag,
        },
        data: {
          settings: {
            tags: [],
            response_headers: { 'cache-control': 'private\r\nInjected: true' },
          },
        },
      });
      expect(invalid.status()).toBe(422);
      const content = await context.request.get(bucketPath + '/objects?key=ui%2Fsettings.txt');
      expect(content.status()).toBe(200);
      expect(await content.body()).toEqual(payload);
      expect(content.headers()['cache-control']).toBe('private, max-age=9');
      expect(content.headers()['x-content-type-options']).toBe('nosniff');
      expect(content.headers()['content-security-policy']).toContain('sandbox');
    });
    await step('new_branch_scoped_SDK_credential_saved_privately', async () => {
      if (state.credential) return;
      await page.goto(`/#/projects/${f.project_id}/credentials`);
      await page.getByLabel('凭据分支', { exact: true }).selectOption(f.branch_id);
      await page.getByLabel('凭据能力', { exact: true }).selectOption('storage:write');
      await page.getByLabel('凭据名称', { exact: true }).fill('S3 settings ' + state.bucket);
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials' && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建应用凭据', exact: true }).click();
      const r = await pending;
      expect(r.status()).toBe(201);
      const body = await r.json();
      state.credential = {
        token_id: body.token_id,
        s3_secret_access_key: body.s3_secret_access_key,
      };
      await save();
      await page.getByRole('button', { name: '已保存，关闭', exact: true }).click();
    });
    await step('actual_SDK_configuration_POST_trailer_and_metadata_closed_loop', async () => {
      if (!state.credential) throw Error('Original SDK credential required');
      const cfg = path.join(privateDir, 's3-settings-sdk.private.json');
      await writeFile(
        cfg,
        JSON.stringify({
          endpoint: 'http://192.168.146.100:30893',
          region: 'us-east-2',
          access_key: state.credential.token_id,
          secret_key: state.credential.s3_secret_access_key,
          bucket: state.bucket,
          branch_id: f.branch_id,
          mode: 's3-settings',
          output: path.join(privateDir, 's3-settings-sdk-checkpoint.private.json'),
        }),
        { mode: 0o600 },
      );
      let stdout = '';
      try {
        stdout = (await promisify(execFile)(probe, [cfg], { timeout: 230000, maxBuffer: 65536 }))
          .stdout;
      } catch (e) {
        const output = (e as { stdout?: string }).stdout;
        if (output) {
          const report = JSON.parse(output);
          for (const check of report.checks) await save(check);
          throw Error(
            `Original SDK action ${report.stage}; code ${report.error_code}; HTTP ${report.http_statuses?.join(',')}`,
          );
        }
        throw Error('Inspect protected original SDK process outcome');
      }
      const report = JSON.parse(stdout);
      expect(report.result).toBe('pass');
      for (const check of report.checks) await save(check);
    });
    await selectBucket();
    await bucketPanel.getByRole('button', { name: '加载桶设置' }).click();
    await step('UI_observes_SDK_configs_and_anonymous_public_block_enforcement', async () => {
      await expect(bucketPanel.getByLabel('桶版本配置声明')).toHaveValue('Enabled');
      await expect(bucketPanel.getByLabel('桶 Object Lock 配置 JSON')).toHaveValue(/GOVERNANCE/);
      await bucketPanel.getByLabel('桶配置访问级别').selectOption('public_read');
      const saveSettings = async () => {
        const pending = page.waitForResponse(
          (r) => new URL(r.url()).pathname === bucketSettings && r.request().method() === 'PUT',
        );
        await bucketPanel.getByRole('button', { name: '保存桶设置', exact: true }).click();
        expect((await pending).status()).toBe(200);
        await expect(
          bucketPanel.getByRole('button', { name: '保存桶设置', exact: true }),
        ).toBeEnabled();
      };
      await saveSettings();
      const target = `/s3/${f.branch_id}/${state.bucket}/ui/settings.txt`;
      const publicRead = await anon.get(target);
      expect(publicRead.status()).toBe(200);
      expect(await publicRead.body()).toEqual(payload);
      expect((await anon.head(target)).status()).toBe(200);
      await bucketPanel.getByLabel('忽略公开 ACL', { exact: true }).check();
      await saveSettings();
      expect((await anon.get(target)).status()).toBe(403);
      await bucketPanel.getByRole('button', { name: '移除公开访问阻断配置' }).click();
      await bucketPanel.getByLabel('桶配置访问级别').selectOption('private');
      await saveSettings();
      expect((await anon.get(target)).status()).toBe(403);
    });
    await step('only_new_SDK_credential_revoked', async () => {
      if (!state.credential) throw Error('Original credential required');
      await page.goto(`/#/projects/${f.project_id}/credentials`);
      await page.getByLabel('凭据分支', { exact: true }).selectOption(f.branch_id);
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === base + '/credentials/' + state.credential!.token_id &&
          r.request().method() === 'DELETE',
      );
      await page
        .getByTestId('backend-row-' + state.credential.token_id)
        .getByRole('button', { name: '撤销凭据', exact: true })
        .click();
      expect((await pending).status()).toBe(200);
    });
    await step('retained_data_compute_returns_to_zero', async () => {
      const target = `/api/v1/projects/${f.project_id}/endpoints/${f.writer_id}`;
      const current = await context.request.get(target);
      expect(current.status()).toBe(200);
      if ((await current.json()).observed_state !== 'suspended') {
        const r = await context.request.post(target + '/suspend', {
          headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': state.bucket + '-zero' },
          data: {},
        });
        expect(r.status()).toBe(202);
        const body = await r.json();
        state.operations.push(body.operation.id);
        await save();
        await follow(body.operation.id);
      }
    });
    state.stage = 'complete';
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  } finally {
    await anon.dispose();
  }
});
