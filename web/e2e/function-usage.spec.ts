import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });

type Usage = {
  function_id: string;
  state: string;
  resource_status: string;
  edge: Record<string, number | null>;
  invocations: { channel: string; unknown_invocations: number | null }[];
  resource_observations: {
    instance_id: string;
    deployment_id: string;
    boot_id: string;
    observed_cpu_usec: number;
    observed_memory_current_bytes: number;
    observed_memory_peak_bytes: number;
  }[];
};
type Fixture = {
  project_id: string;
  branch_id: string;
  function_id: string;
  slug: string;
  org_name: string;
  cookie?: string;
  cookie_file?: string;
  empty_branch_id: string;
  deployments: { a: string; b: string; rollback: string };
};

// Only GETs on the retained Function. Real responses prove the visible usage
// and delayed cross-branch delivery. Synthetic responses below are explicitly
// view checks; they are not server failure/reliability evidence.
test('Retained Function usage: original boot totals historical scope and late response isolation', async ({
  page,
  context,
  baseURL,
}, info) => {
  test.setTimeout(180000);
  const fixtureFile = process.env.NEON_E2E_FUNCTION_USAGE_FIXTURE;
  if (process.platform !== 'linux' || !fixtureFile || !baseURL)
    throw Error('Protected Linux retained Function usage fixture required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')) as Fixture;
  const deploymentIDs = Object.values(fixture.deployments || {});
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.empty_branch_id) ||
    fixture.empty_branch_id === fixture.branch_id ||
    !/^fnc_[a-f0-9]{16}$/.test(fixture.function_id) ||
    !/^[a-z0-9]{1,20}$/.test(fixture.slug) ||
    !fixture.org_name ||
    deploymentIDs.length !== 3 ||
    new Set(deploymentIDs).size !== 3 ||
    deploymentIDs.some((id) => !/^fdp_[a-f0-9]{16}$/.test(id))
  )
    throw Error(
      'Exact retained Function, empty child branch and three deployment identities required',
    );
  const cookie = fixture.cookie_file
    ? (await readFile(fixture.cookie_file, 'utf8')).trim()
    : fixture.cookie?.trim();
  if (!cookie || /[\r\n]/.test(cookie)) throw Error('Protected original session cookie required');
  const session = cookie.includes('=')
    ? cookie
        .split(';')
        .map((part) => part.trim())
        .find((part) => part.startsWith('neon_v2_session='))
        ?.slice('neon_v2_session='.length)
    : cookie;
  if (!session) throw Error('Original Console session cookie required');
  await context.addCookies([{ name: 'neon_v2_session', value: session, url: baseURL }]);

  const root = `/api/v1/projects/${fixture.project_id}/branches/${fixture.branch_id}/functions/${fixture.slug}`;
  const usageURL = new URL(root + '/usage', baseURL).href;
  const liveChecks: string[] = [],
    viewChecks: string[] = [];
  const writes: { method: string; path: string }[] = [];
  let publicInvocations = 0;
  let result = 'running';
  async function save() {
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: fixture.project_id,
        branch_id: fixture.branch_id,
        function_id: fixture.function_id,
        empty_branch_id: fixture.empty_branch_id,
        deployments: fixture.deployments,
        live_read_checks: liveChecks,
        browser_view_checks: viewChecks,
        business_mutations: writes.length,
        public_invocations: publicInvocations,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
        synthetic_responses_are_view_checks_only: true,
      }) + '\n',
    );
  }
  async function get(path: string) {
    const response = await context.request.get(path);
    expect(response.status()).toBe(200);
    return response.json();
  }
  const panel = () => page.getByTestId('function-usage');
  async function load() {
    const pending = page.waitForResponse(
      (response) => response.url() === usageURL && response.request().method() === 'GET',
    );
    await panel().getByRole('button', { name: '加载函数用量', exact: true }).click();
    return pending;
  }
  async function selectOriginal() {
    await page.getByLabel('Functions 分支', { exact: true }).selectOption(fixture.branch_id);
    await page.getByRole('button', { name: '查看函数 ' + fixture.slug, exact: true }).click();
    await expect(panel()).toBeVisible();
  }
  await page.route('**/api/**', async (route) => {
    const request = route.request();
    if (!['GET', 'HEAD', 'OPTIONS'].includes(request.method())) {
      writes.push({ method: request.method(), path: new URL(request.url()).pathname });
      await route.abort('blockedbyclient');
      return;
    }
    await route.fallback();
  });
  await page.route('**/functions/v1/**', async (route) => {
    publicInvocations++;
    await route.abort('blockedbyclient');
  });

  try {
    await save();
    const before = await get(root);
    expect(before.id).toBe(fixture.function_id);
    expect(before.state).toBe('suspended');
    const usage = (await get(root + '/usage')) as Usage;
    expect(usage.function_id).toBe(fixture.function_id);
    expect(usage.resource_status).toBe('historical_only');
    for (const id of deploymentIDs)
      expect(usage.resource_observations.some((row) => row.deployment_id === id)).toBe(true);
    const organizations = await get('/api/v1/organizations');
    const org = organizations.items.find(
      (entry: { name: string }) => entry.name === fixture.org_name,
    );
    expect(org).toBeTruthy();
    await page.goto('/#/projects');
    await page.getByLabel('切换组织', { exact: true }).click();
    await page.getByRole('option', { name: org.name + ' · ' + org.role, exact: true }).click();
    await page.goto(`/#/projects/${fixture.project_id}/functions`);
    await selectOriginal();
    expect((await load()).status()).toBe(200);
    await expect(panel().getByTestId('function-resource-status')).toHaveText(
      '仅有历史资源观测，不能代表当前实例',
    );
    await expect(panel().getByTestId('function-edge-unconfirmed')).toHaveText(
      usage.edge.unconfirmed_dispatches!.toLocaleString('zh-CN'),
    );
    await expect(panel().getByText('已观测响应 / WS 下行帧字节', { exact: true })).toBeVisible();
    for (const sample of usage.resource_observations) {
      const row = panel().locator(
        `tr[data-instance="${sample.instance_id}"][data-boot="${sample.boot_id}"]`,
      );
      await expect(row).toHaveCount(1);
      await expect(row).toHaveAttribute('data-deployment', sample.deployment_id);
      await expect(row.getByTestId('function-boot-cpu')).toHaveText(
        sample.observed_cpu_usec.toLocaleString('zh-CN'),
      );
      await expect(row).toContainText('不代表当前运行状态');
    }
    await expect(panel().getByText(/不按调用重复相加/)).toBeVisible();
    for (const row of usage.invocations) {
      const cell = panel()
        .locator(`tr[data-channel="${row.channel}"]`)
        .getByTestId('function-unknown-invocations');
      await expect(cell).toHaveText(
        row.unknown_invocations === null
          ? '不可用'
          : row.unknown_invocations.toLocaleString('zh-CN'),
      );
    }
    liveChecks.push('actual_A_B_rollback_boot_totals_and_unknown_counts_displayed_as_recorded');
    await save();

    let release!: () => void, received!: () => void, delivered!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const backendReceived = new Promise<void>((resolve) => {
      received = resolve;
    });
    const deliveryFinished = new Promise<void>((resolve) => {
      delivered = resolve;
    });
    let leftScope = false;
    await page.route(
      usageURL,
      async (route) => {
        try {
          const response = await route.fetch();
          expect(response.status()).toBe(200);
          await response.body();
          received();
          await gate;
          await route.fulfill({ response });
        } catch (error) {
          if (!leftScope) throw error;
        } finally {
          delivered();
        }
      },
      { times: 1 },
    );
    // Do not await the held browser response before changing branch.
    await panel().getByRole('button', { name: '加载函数用量', exact: true }).click();
    await backendReceived;
    leftScope = true;
    await page.getByLabel('Functions 分支', { exact: true }).selectOption(fixture.empty_branch_id);
    await expect(
      page.getByRole('heading', { name: '这个分支还没有函数', exact: true }),
    ).toBeVisible();
    release();
    await deliveryFinished;
    await expect(panel()).toHaveCount(0);
    await selectOriginal();
    await expect(panel().getByTestId('function-usage-snapshot')).toHaveCount(0);
    expect((await load()).status()).toBe(200);
    await expect(panel().getByTestId('function-resource-observations')).toBeVisible();
    liveChecks.push(
      'late_actual_response_aborted_or_discarded_across_existing_empty_branch_and_return',
    );
    await save();

    await page.route(
      usageURL,
      (route) =>
        route.fulfill({
          status: 200,
          json: {
            ...usage,
            resource_status: 'unavailable',
            resource_observations: [],
            edge: { ...usage.edge, completion_receipts: null, unconfirmed_dispatches: null },
            invocations: [
              {
                channel: 'public_http',
                admitted_invocations: 9,
                completed_invocations: 2,
                in_progress_invocations: 0,
                unknown_invocations: 7,
                observed_response_bytes: null,
              },
            ],
          },
        }),
      { times: 1 },
    );
    await load();
    await expect(panel().getByTestId('function-resource-status')).toHaveText(
      '资源观测不可用，不能按零计算',
    );
    await expect(panel().getByTestId('function-edge-unconfirmed')).toHaveText('不可用');
    await expect(panel().getByTestId('function-edge-receipts')).toHaveText('不可用');
    await expect(panel().getByTestId('function-unknown-invocations')).toHaveText('7');
    await expect(panel().locator('tr[data-channel="public_http"] td').last()).toHaveText('不可用');
    await expect(panel().getByTestId('function-resource-observations')).toHaveCount(0);
    viewChecks.push(
      'synthetic_missing_counters_and_resources_are_unavailable_unknown_remains_separate',
    );

    await page.route(
      usageURL,
      (route) =>
        route.fulfill({ status: 200, json: { ...usage, function_id: 'fnc_0000000000000000' } }),
      { times: 1 },
    );
    await load();
    await expect(panel().getByRole('alert')).toHaveText('函数用量身份不匹配，请刷新');
    await expect(panel().getByTestId('function-usage-snapshot')).toHaveCount(0);
    viewChecks.push('synthetic_wrong_function_identity_is_rejected_without_old_snapshot');

    await page.route(
      usageURL,
      (route) =>
        route.fulfill({
          status: 503,
          json: {
            code: 'function_usage_unavailable',
            message: '视图检查：用量读取失败',
            request_id: 'view-only',
            retryable: true,
          },
        }),
      { times: 1 },
    );
    await load();
    await expect(panel().getByRole('alert')).toContainText('视图检查：用量读取失败');
    await expect(panel().getByTestId('function-usage-snapshot')).toHaveCount(0);
    viewChecks.push('synthetic_read_failure_clears_prior_snapshot_and_offers_explicit_reload');

    expect((await load()).status()).toBe(200);
    await expect(panel().getByTestId('function-resource-status')).toHaveText(
      '仅有历史资源观测，不能代表当前实例',
    );
    const after = await get(root);
    expect({
      id: after.id,
      state: after.state,
      version: after.version,
      generation: after.generation,
      active: after.active_deployment_id,
    }).toEqual({
      id: before.id,
      state: before.state,
      version: before.version,
      generation: before.generation,
      active: before.active_deployment_id,
    });
    expect(writes).toEqual([]);
    expect(publicInvocations).toBe(0);
    liveChecks.push(
      'GET_only_original_suspended_function_version_generation_and_deployment_preserved',
    );
    result = 'pass';
  } finally {
    await save();
  }
});
