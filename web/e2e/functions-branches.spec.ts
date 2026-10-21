import { expect, test, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

// Real browser/Worker -> original Neon timelines, verified SQL login and Guest.
// Only a committed 202 response is deliberately lost; no resource is mocked.
test('Frozen branch Functions prepare paused, isolate SQL and recover original copies', async ({
  page,
  context,
}, info) => {
  test.setTimeout(2200000);
  page.setDefaultTimeout(30000);
  const input = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    replacement = process.env.NEON_E2E_FUNCTION_REPLACEMENT_ZIP;
  if (process.platform !== 'linux' || !input || !admin || !zip || !privateDir || !replacement)
    throw Error('Protected Linux owner and pinned SQL bundle required');
  const original = JSON.parse(await readFile(input, 'utf8')),
    project = original.project_id;
  if (!/^prj_[a-f0-9]{16}$/.test(project) || !/^br_[a-f0-9]{16}$/.test(original.branch_id))
    throw Error('Original owner required');
  const state = original.branch_copy_resume || {
    result: 'running',
    checks: [],
    operations: [],
    parent: null,
    child: null,
    historical: null,
    source_function_id: '',
    definition_id: '',
    source_deployment_id: '',
    new_source_deployment_id: '',
    historical_lsn: '',
    explicit_function_id: '',
    explicit_operation_id: '',
    trigger_id: '',
  };
  state.result = 'running';
  const password = original.branch_copy_resume
    ? (await readFile(privateDir + '/branch-copy-password', 'utf8')).trim()
    : 'Branch-' + randomBytes(28).toString('base64url');
  if (!original.branch_copy_resume)
    await writeFile(privateDir + '/branch-copy-password', password, { flag: 'wx', mode: 0o600 });
  const base = `/api/v1/projects/${project}`,
    slug = 'branchprobe56',
    empty = 'branchdefinition56';
  const fn = (branch: string, name = slug) => `${base}/branches/${branch}/functions/${name}`;
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    const report = {
      ...state,
      project_id: project,
      original_parent_branch_id: original.branch_id,
      credentials_in_evidence: false,
      performance_reliability_campaign: false,
      data_retained: true,
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(report) + '\n');
    await writeFile(privateDir + '/branch-copy-checkpoint.json', JSON.stringify(report) + '\n', {
      mode: 0o600,
    });
  }
  async function get(path: string) {
    const r = await context.request.get(path);
    expect(r.status(), path).toBe(200);
    return r.json();
  }
  async function operation(id: string, want = 'succeeded') {
    if (!state.operations.includes(id)) {
      state.operations.push(id);
      await save();
    }
    await expect
      .poll(
        async () => {
          const op = await get(base + '/operations/' + id);
          if (want === 'succeeded' && ['failed', 'cancelled'].includes(op.state))
            throw Error('Original operation ' + id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe(want);
    return get(base + '/operations/' + id);
  }
  async function action(path: string, button: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path && r.request().method() === method,
      { timeout: 210000 },
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function waitResponse(response: Response) {
    expect(response.status()).toBe(202);
    const v = await response.json();
    await operation(v.operation.id);
    return v;
  }
  async function retryOriginal(id: string, want = 'succeeded') {
    await page.goto(`/#/projects/${project}/operations`);
    await page.getByRole('button', { name: '查看操作 ' + id + ' 的步骤', exact: true }).click();
    const response = await action(base + '/operations/' + id + '/retry', '重试原操作');
    expect(response.status()).toBe(202);
    expect((await response.json()).id).toBe(id);
    return operation(id, want);
  }
  async function functions(branch: string, name?: string) {
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByLabel('Functions 分支', { exact: true }).selectOption(branch);
    await action(`${base}/branches/${branch}/functions`, '刷新函数状态', 'GET');
    if (name) await page.getByRole('button', { name: '查看函数 ' + name, exact: true }).click();
  }
  async function query(branch: any, statement: string) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(branch.writer_id);
    await page.getByLabel('目标数据库', { exact: true }).fill('fn_native');
    await page.getByLabel('SQL 查询', { exact: true }).fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const response = await action(
      base + '/endpoints/' + branch.writer_id + '/query',
      '▶ 执行 SQL',
    );
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function createBranch(
    key: 'parent' | 'child' | 'historical',
    source: string,
    withWriter: boolean,
    policy: 'omit' | 'inherit_paused',
    lsn = '',
  ) {
    if (!state[key]) {
      await page.goto(`/#/projects/${project}/branches`);
      await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
      await page
        .getByLabel('分支名称', { exact: true })
        .fill('ci-functions-' + key + '-' + Date.now());
      await page.getByLabel('父分支', { exact: true }).selectOption(source);
      await page.getByLabel('函数策略', { exact: true }).selectOption(policy);
      await page
        .getByLabel('同时创建读写 Compute Endpoint', { exact: true })
        .setChecked(withWriter);
      if (lsn) {
        await page.getByLabel('分支起点', { exact: true }).selectOption('lsn');
        await page.getByLabel('恢复 LSN', { exact: true }).fill(lsn);
      }
      if (policy === 'inherit_paused') {
        await expect(page.getByRole('button', { name: '创建并验证', exact: true })).toBeDisabled();
        await page.getByLabel('确认复制函数环境', { exact: true }).check();
        if (lsn) {
          await expect(
            page.getByRole('button', { name: '创建并验证', exact: true }),
          ).toBeDisabled();
          await page.getByLabel('确认历史 SQL 使用当前函数代码', { exact: true }).check();
        }
      }
      if (withWriter) {
        await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
        await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
        await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
      }
      const response = await action(base + '/branches', '创建并验证');
      expect(response.status()).toBe(202);
      const v = await response.json();
      state[key] = {
        id: v.resource.id,
        operation_id: v.operation.id,
        writer_id: '',
        timeline_id: '',
      };
      await save();
    }
    await operation(state[key].operation_id);
    const v = await get(base + '/branches/' + state[key].id);
    state[key].timeline_id = v.timeline_id;
    const writers = (await get(base + '/endpoints')).items.filter(
      (e: any) =>
        e.branch_id === state[key].id && e.endpoint_type === 'read_write' && e.state !== 'deleted',
    );
    expect(writers).toHaveLength(withWriter ? 1 : 0);
    if (withWriter) state[key].writer_id = writers[0].id;
    await save();
    return state[key];
  }
  async function suspend(branch: string, name = slug) {
    const v = await get(fn(branch, name));
    if (v.state !== 'suspended') {
      await functions(branch, name);
      await waitResponse(await action(fn(branch, name) + '/suspend', '停止函数实例'));
    }
  }
  async function createDefinition(branch: string, name: string) {
    await functions(branch);
    await page.getByLabel('函数名称', { exact: true }).fill(name);
    await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
    await page.getByLabel('授权 Schema', { exact: true }).fill('app');
    const response = await action(`${base}/branches/${branch}/functions`, '创建函数定义');
    expect(response.status()).toBe(202);
    const v = await response.json();
    await operation(v.operation.id);
    return v.function_id;
  }
  async function deploy(environment: string, archive = zip!) {
    await functions(state.parent.id, slug);
    await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(archive);
    await page
      .getByLabel('环境变量 JSON', { exact: true })
      .fill(JSON.stringify({ BRANCH_MARKER: environment }));
    await waitResponse(await action(fn(state.parent.id) + '/deployments', '部署函数版本'));
    return (await get(fn(state.parent.id))).active_deployment_id;
  }
  async function plans(branch: string, ready: boolean) {
    await expect
      .poll(
        async () => {
          const p = (await get(base + '/branches/' + branch + '/function-copy-plans')).items;
          return (
            p.length === 2 &&
            p.every((x: any) =>
              ready
                ? ['succeeded', 'definition_only'].includes(x.preparation_state)
                : x.preparation_state === 'waiting_for_writer',
            )
          );
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe(true);
    const p = (await get(base + '/branches/' + branch + '/function-copy-plans')).items;
    for (const v of p) if (v.operation_id) await operation(v.operation_id);
    return p;
  }
  async function invoke(branch: string, name = slug, value?: string) {
    await functions(branch, name);
    await page
      .getByLabel('调用方法', { exact: true })
      .selectOption(value === undefined ? 'GET' : 'POST');
    await page.getByLabel('调用路径', { exact: true }).fill('/');
    await page
      .getByLabel('调用请求体', { exact: true })
      .fill(value === undefined ? '' : JSON.stringify({ value }));
    const response = await action(fn(branch, name) + '/console-invoke', '调用函数');
    expect(response.status()).toBe(200);
    const result = await response.json();
    expect(result.status_code).toBe(200);
    const body = JSON.parse(Buffer.from(result.body, 'base64').toString('utf8'));
    await expect(page.getByTestId('function-invocation-result')).toContainText('branch-copy-v');
    return body;
  }
  async function wake(branch: string, name = slug) {
    if ((await get(fn(branch, name))).state === 'active') return;
    await functions(branch, name);
    await waitResponse(await action(fn(branch, name) + '/resume', '唤醒保留版本'));
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!state.checks.includes('isolated_source_branch_and_real_sql_code')) {
      const parent = await createBranch('parent', original.branch_id, true, 'omit');
      await query(
        parent,
        'CREATE TABLE IF NOT EXISTS app.function_branch_probe(id int PRIMARY KEY,value text)',
      );
      await query(
        parent,
        "INSERT INTO app.function_branch_probe VALUES(1,'parent-initial') ON CONFLICT(id) DO NOTHING",
      );
      await query(
        parent,
        'GRANT SELECT,INSERT,UPDATE,DELETE ON app.function_branch_probe TO control_probe WITH GRANT OPTION',
      );
      if (!state.source_function_id) {
        state.source_function_id = await createDefinition(parent.id, slug);
        await save();
      }
      if (!state.source_deployment_id) {
        state.source_deployment_id = await deploy('frozen-environment-v1');
        await save();
      }
      const actual = await invoke(parent.id);
      expect(actual).toMatchObject({
        code: 'branch-copy-v1',
        environment: 'frozen-environment-v1',
        value: 'parent-initial',
        role: 'fn_' + state.source_function_id.slice(4),
      });
      state.parent_role = actual.role;
      await suspend(parent.id);
      if (!state.definition_id) {
        state.definition_id = await createDefinition(parent.id, empty);
        await save();
      }
      const current = await query(parent, 'SELECT pg_current_wal_lsn()::text');
      state.historical_lsn = current.rows[0][0];
      await save('isolated_source_branch_and_real_sql_code');
    }
    if (!state.checks.includes('no_writer_frozen_plan_keeps_original_code_and_disabled_trigger')) {
      if (!state.trigger_id) {
        await functions(state.parent.id, slug);
        await page.getByLabel('定时计划名称', { exact: true }).fill('branch frozen schedule');
        await page.getByLabel('UTC Cron', { exact: true }).fill('* * * * *');
        const response = await action(
          `${base}/branches/${state.parent.id}/functions-triggers`,
          '保存暂停计划',
        );
        expect(response.status()).toBe(201);
        state.trigger_id = (await response.json()).trigger.trigger_id;
        await save();
      }
      const child = await createBranch('child', state.parent.id, false, 'inherit_paused');
      const p = await plans(child.id, false);
      expect(p.find((x: any) => x.slug === slug).source_deployment_id).toBe(
        state.source_deployment_id,
      );
      expect((await get(`${base}/branches/${child.id}/functions`)).items).toHaveLength(0);
      await functions(child.id);
      await expect(page.getByRole('region', { name: '分支函数准备', exact: true })).toContainText(
        '等待此分支的 Writer',
      );
      await save('no_writer_frozen_plan_keeps_original_code_and_disabled_trigger');
    }
    if (!state.checks.includes('delayed_writer_uses_admitted_version_after_parent_update')) {
      if (!state.new_source_deployment_id) {
        state.new_source_deployment_id = await deploy('parent-environment-v2', replacement!);
        expect(await invoke(state.parent.id)).toMatchObject({
          code: 'branch-copy-v2',
          environment: 'parent-environment-v2',
        });
        await suspend(state.parent.id);
        await save();
      }
      await query(
        state.parent,
        "UPDATE app.function_branch_probe SET value='parent-after-fork' WHERE id=1",
      );
      if (!state.child.writer_id) {
        await page.goto(`/#/projects/${project}/branches/${state.child.id}`);
        await page.getByRole('button', { name: '＋ 创建 Endpoint', exact: true }).click();
        await page.getByLabel('数据库角色密码', { exact: true }).fill(password);
        await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
        await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
        const response = await action(base + '/endpoints', '创建并验证');
        expect(response.status()).toBe(202);
        const v = await response.json();
        state.child.writer_id = v.resource.id;
        await save();
        await operation(v.operation.id);
      }
      const p = await plans(state.child.id, true);
      state.child.function_id = p.find((x: any) => x.slug === slug).function_id;
      state.child.definition_id = p.find((x: any) => x.slug === empty).function_id;
      const copied = await get(fn(state.child.id));
      expect(copied.state).toBe('suspended');
      expect(copied.invocation_url).toBeNull();
      expect(copied.public_enabled || false).toBe(false);
      expect(await get(fn(state.child.id) + '/runtime')).toMatchObject({
        observed: false,
        observation_source: 'metadata_only',
      });
      const childTriggers = (await get(`${base}/branches/${state.child.id}/functions-triggers`))
        .items;
      expect(childTriggers).toHaveLength(1);
      expect(childTriggers[0]).toMatchObject({
        trigger_id: state.trigger_id,
        enabled: false,
        next_run_at: null,
        inherited: true,
      });
      await save('delayed_writer_uses_admitted_version_after_parent_update');
    }
    if (!state.checks.includes('actual_child_sql_isolation_new_role_and_frozen_environment')) {
      await wake(state.child.id);
      const before = await invoke(state.child.id);
      expect(before).toMatchObject({
        code: 'branch-copy-v1',
        value: 'parent-initial',
        environment: 'frozen-environment-v1',
        role: 'fn_' + state.child.function_id.slice(4),
      });
      expect(before.role).not.toBe(state.parent_role);
      const after = await invoke(state.child.id, slug, 'child-only');
      expect(after.value).toBe('child-only');
      expect(
        (await query(state.parent, 'SELECT value FROM app.function_branch_probe WHERE id=1')).rows,
      ).toEqual([['parent-after-fork']]);
      await suspend(state.child.id);
      await save('actual_child_sql_isolation_new_role_and_frozen_environment');
    }
    if (!state.checks.includes('explicit_copy_lost_202_and_normal_original_failure_retry')) {
      if (!state.explicit_operation_id) {
        // A real missing owner grant fails SQL preparation, without running code.
        await query(state.child, 'REVOKE USAGE ON SCHEMA app FROM control_probe CASCADE');
        await functions(state.child.id);
        await page.getByLabel('复制来源分支', { exact: true }).selectOption(state.parent.id);
        await page.getByLabel('复制来源函数', { exact: true }).selectOption(slug);
        await page
          .getByLabel('复制来源部署', { exact: true })
          .selectOption(state.source_deployment_id);
        await page.getByLabel('复制目标函数名称', { exact: true }).fill('explicitcopy56');
        await page.getByLabel('确认复制环境到目标分支', { exact: true }).check();
        const path = `${base}/branches/${state.child.id}/functions-copy`;
        let lost: any = null;
        await page.route('**' + path, async (route) => {
          if (route.request().method() !== 'POST') {
            await route.continue();
            return;
          }
          const r = await route.fetch();
          expect(r.status()).toBe(202);
          lost = {
            key: route.request().headers()['idempotency-key'],
            version: route.request().headers()['if-match'],
            body: route.request().postData(),
            value: await r.json(),
          };
          state.explicit_function_id = lost.value.function_id;
          state.explicit_operation_id = lost.value.operation.id;
          await save();
          await route.abort('failed');
        });
        await page.getByRole('button', { name: '复制并准备暂停函数', exact: true }).click();
        await expect(
          page.getByRole('button', { name: '重试原复制请求', exact: true }),
        ).toBeVisible();
        await page.unroute('**' + path);
        const response = await action(path, '重试原复制请求');
        expect(response.status()).toBe(202);
        const replayed = await response.json();
        expect(replayed.function_id).toBe(lost.value.function_id);
        expect(replayed.operation.id).toBe(lost.value.operation.id);
        expect(response.request().headers()['idempotency-key']).toBe(lost.key);
        expect(response.request().headers()['if-match']).toBe(lost.version);
        expect(response.request().postData()).toBe(lost.body);
        state.explicit_admission_verified = true;
        await save();
      } else expect(state.explicit_admission_verified).toBe(true);
      // An existing failed receipt stays on the same original operation while
      // a reviewed SQL grant diagnostic repair is validated, before owner repair.
      const current = await get(base + '/operations/' + state.explicit_operation_id);
      if (current.state !== 'succeeded') {
        if (
          current.state === 'failed' &&
          current.error_code === 'reconcile_failed' &&
          state.original_generic_failure
        ) {
          await retryOriginal(state.explicit_operation_id, 'failed');
        }
        const failed = await operation(state.explicit_operation_id, 'failed');
        expect(failed.retryable).toBe(true);
        expect(failed.error_code).toBe('function_database_grant_required');
        state.original_copy_failure = {
          id: failed.id,
          error_code: failed.error_code,
          attempts: failed.attempts,
          finished_at: failed.finished_at,
        };
        await save();
        expect(await get(fn(state.child.id, 'explicitcopy56') + '/runtime')).toMatchObject({
          observed: false,
          observation_source: 'metadata_only',
        });
        await query(state.child, 'GRANT USAGE ON SCHEMA app TO control_probe WITH GRANT OPTION');
        await query(
          state.child,
          'GRANT USAGE ON SCHEMA app TO fn_' + state.child.function_id.slice(4),
        );
        await retryOriginal(state.explicit_operation_id);
        expect((await get(fn(state.child.id, 'explicitcopy56'))).state).toBe('suspended');
        state.explicit_paused_copy_verified = true;
        await save();
      } else expect(state.explicit_paused_copy_verified).toBe(true);
      await wake(state.child.id, 'explicitcopy56');
      const actual = await invoke(state.child.id, 'explicitcopy56');
      expect(actual).toMatchObject({
        code: 'branch-copy-v1',
        value: 'child-only',
        environment: 'frozen-environment-v1',
        role: 'fn_' + state.explicit_function_id.slice(4),
      });
      await suspend(state.child.id, 'explicitcopy56');
      await save('explicit_copy_lost_202_and_normal_original_failure_retry');
    }
    if (!state.checks.includes('historical_sql_current_function_ack_and_paused_snapshot')) {
      const historical = await createBranch(
        'historical',
        state.parent.id,
        true,
        'inherit_paused',
        state.historical_lsn,
      );
      const p = await plans(historical.id, true);
      historical.function_id = p.find((x: any) => x.slug === slug).function_id;
      historical.definition_id = p.find((x: any) => x.slug === empty).function_id;
      expect(p.find((x: any) => x.slug === slug).source_deployment_id).toBe(
        state.new_source_deployment_id,
      );
      expect(historical.timeline_id).not.toBe(state.parent.timeline_id);
      expect(historical.timeline_id).not.toBe(state.child.timeline_id);
      expect((await get(fn(historical.id))).state).toBe('suspended');
      expect(await get(fn(historical.id) + '/runtime')).toMatchObject({
        observed: false,
        observation_source: 'metadata_only',
      });
      await wake(historical.id);
      expect(await invoke(historical.id)).toMatchObject({
        code: 'branch-copy-v2',
        value: 'parent-initial',
        environment: 'parent-environment-v2',
        role: 'fn_' + historical.function_id.slice(4),
      });
      await suspend(historical.id);
      await save('historical_sql_current_function_ack_and_paused_snapshot');
    }
    if (!state.checks.includes('all_owned_functions_retired_writers_zero_original_data_retained')) {
      for (const branch of [state.parent, state.child, state.historical]) {
        const definitions = (await get(`${base}/branches/${branch.id}/functions`)).items;
        const owned =
          branch.id === state.parent.id
            ? [state.source_function_id, state.definition_id]
            : branch.id === state.child.id
              ? [state.child.function_id, state.child.definition_id, state.explicit_function_id]
              : [state.historical.function_id, state.historical.definition_id];
        expect(definitions.every((f: any) => owned.includes(f.id))).toBe(true);
        for (const definition of definitions) {
          await functions(branch.id, definition.slug);
          await waitResponse(await action(fn(branch.id, definition.slug), '删除函数', 'DELETE'));
        }
        await page.goto(`/#/projects/${project}/compute`);
        // This visible label also contains its option text. Match the same
        // endpoint selector used by the other Compute lifecycle acceptance.
        await page.locator('select').first().selectOption(branch.writer_id);
        const endpoint = await get(base + '/endpoints/' + branch.writer_id);
        if (endpoint.observed_state !== 'suspended') {
          expect(endpoint.observed_state).toBe('active');
          await waitResponse(
            await action(base + '/endpoints/' + branch.writer_id + '/suspend', '现在缩到 0'),
          );
        }
        await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
          timeout: 150000,
        });
      }
      await save('all_owned_functions_retired_writers_zero_original_data_retained');
    }
    state.result = 'pass';
    await save();
  } catch (error) {
    state.result = 'fail';
    await save();
    throw error;
  }
});
