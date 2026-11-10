// 浏览器组件合同检查：所有API均为本进程夹具，绝不向真实控制面发送创建请求。
// 真实Neon/K8s端到端由独立受保护用例执行，不能引用本结果作为产品验收。
import assert from 'node:assert/strict';
import { writeFile } from 'node:fs/promises';
import { chromium } from '@playwright/test';

const base = new URL(process.env.NEON_COMPONENT_BASE_URL || 'http://127.0.0.1:5174');
if (!['127.0.0.1', 'localhost'].includes(base.hostname))
  throw new Error('Only a local development web server is allowed');
const browser = await chromium.launch(
  process.env.NEON_COMPONENT_BROWSER_CHANNEL
    ? { channel: process.env.NEON_COMPONENT_BROWSER_CHANNEL }
    : {},
);
const checks = [];
let failure = null;
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  const requests = [];
  let available = true;
  const releases = [17, 18].map((postgres_version) => ({
    postgres_version,
    region_id: 'component-region-' + postgres_version,
    create_enabled: true,
    memory_scaling: 'vm_and_postgres',
    bounds: {
      min_cpu_milli: 500,
      max_cpu_milli: 2000,
      min_memory_mib: postgres_version === 17 ? 7168 : 8192,
      max_memory_mib: postgres_version === 17 ? 8192 : 9216,
    },
    memory_slot_mib: 1024,
  }));
  page.on('pageerror', (e) => errors.push(e.message));
  // 不放行任何未声明的API或外域请求，防止测试夹具意外触及集群。
  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.origin !== base.origin) return route.abort();
    if (!url.pathname.startsWith('/api/') && !url.pathname.startsWith('/auth/'))
      return route.continue();
    let value;
    if (url.pathname === '/api/v1/session')
      value = { user: { id: 'component_admin', username: 'component', role: 'admin' } };
    else if (url.pathname === '/api/v1/organizations')
      value = {
        items: [
          {
            id: 'org_component',
            name: 'Component fixture',
            slug: 'fixture',
            role: 'admin',
            state: 'active',
          },
        ],
      };
    else if (url.pathname === '/api/v1/capabilities')
      value = {
        features: { project_create: { enabled: true } },
        postgres_runtimes: available ? releases : [],
      };
    else if (url.pathname === '/api/v1/platform/ai-providers/capabilities')
      value = { enabled: false };
    else if (url.pathname === '/api/v1/organizations/org_component/projects') {
      if (request.method() === 'POST') {
        const body = request.postDataJSON();
        const { password: discardedFixturePassword, ...publicBody } = body;
        assert.equal(discardedFixturePassword, 'component-only-password-20261107');
        requests.push({ body: publicBody, key: request.headers()['idempotency-key'] });
        // 确定失败使表单保持可编辑；不用假的succeeded冒真实Compute就绪。
        return route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({
            error: { code: 'component_fixture', message: 'Component fixture response' },
          }),
        });
      }
      value = { items: [] };
    } else return route.fulfill({ status: 404, contentType: 'application/json', body: '{}' });
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(value),
    });
  });
  await page.goto(base.href);
  await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '创建项目', exact: true });
  const select = dialog.getByLabel('PostgreSQL 版本与地域', { exact: true });
  await select.selectOption('17:component-region-17');
  assert.deepEqual(
    await select.locator('option').evaluateAll((items) => items.map((i) => i.value)),
    ['17:component-region-17', '18:component-region-18'],
  );
  checks.push('only_configured_pg17_pg18_no_pg16');
  await dialog.getByLabel('项目名称', { exact: true }).fill('runtime-component');
  await dialog
    .getByLabel('数据库角色密码', { exact: true })
    .fill('component-only-password-20261107');
  const submit = dialog.getByRole('button', { name: '创建并验证', exact: true });
  async function submitAndInspect(version) {
    const before = requests.length;
    await submit.click();
    await page.waitForFunction(() => {
      const button = [...document.querySelectorAll('button')].find(
        (b) => b.textContent === '创建并验证',
      );
      return button && !button.disabled;
    });
    assert.equal(requests.length, before + 1);
    const request = requests.at(-1);
    const release = releases.find((r) => r.postgres_version === version);
    assert.equal(request.body.postgres_version, version);
    assert.equal(request.body.region_id, release.region_id);
    assert.deepEqual(request.body.autoscaling, release.bounds);
    assert(request.key && request.key.length >= 8);
    return request;
  }
  const first = await submitAndInspect(17);
  const replay = await submitAndInspect(17);
  assert.equal(first.key, replay.key);
  checks.push('pg17_selected_bounds_and_same_request_key');
  await select.selectOption('18:component-region-18');
  assert.equal(await dialog.getByLabel('最小内存', { exact: true }).inputValue(), '8192');
  const second = await submitAndInspect(18);
  assert.notEqual(second.key, first.key);
  checks.push('pg18_resets_class_bounds_and_request_identity');
  await dialog.getByRole('button', { name: '关闭创建窗口', exact: true }).click();
  available = false;
  await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
  await dialog.getByRole('alert').filter({ hasText: '当前没有可创建' }).waitFor();
  assert(await dialog.getByRole('button', { name: '创建并验证', exact: true }).isDisabled());
  checks.push('empty_catalog_prevents_creation');
  assert.deepEqual(errors, []);
  checks.push('no_browser_javascript_errors');
  await context.close();
} catch (error) {
  failure = error instanceof Error ? error.message : String(error);
} finally {
  await browser.close();
}
const result = {
  result: failure ? 'FAIL' : 'PASS',
  scope: 'BROWSER_COMPONENT_FIXTURES_ONLY_NOT_K8S_E2E',
  checks,
  failure,
};
if (process.env.NEON_COMPONENT_RESULTS)
  await writeFile(process.env.NEON_COMPONENT_RESULTS, JSON.stringify(result, null, 2) + '\n');
console.log(JSON.stringify(result));
if (failure) process.exitCode = 1;
