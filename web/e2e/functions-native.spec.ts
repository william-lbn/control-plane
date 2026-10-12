import { expect, test, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Actual Linux browser -> UI -> Go API/Worker -> owned microVM -> Proxy -> Neon
// SQL. No browser route mock or invented live resource/status fixture is used.
test('Functions native: UI ZIP deployment, SQL, safe invocation, stop, redeploy and delete', async ({
  page,
  context,
}, info) => {
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const zipFile = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!adminFile || !privateDir || !zipFile)
    throw new Error('Protected Linux native Functions inputs required');
  const attempt = process.env.NEON_E2E_ATTEMPT || String(Date.now());
  let password = 'Fn-' + randomBytes(24).toString('base64url');
  const checks: string[] = [];
  let project = '',
    branch = '',
    writer = '',
    functionID = '',
    result = 'running';
  const operations: string[] = [];
  const resumeFile = process.env.NEON_E2E_FUNCTIONS_RESUME_FIXTURE;
  const resume = resumeFile
    ? (JSON.parse(await readFile(resumeFile, 'utf8')) as {
        project_id: string;
        branch_id: string;
        writer_id: string;
        function_id: string;
        failed_deployment_operation_id?: string;
        database_password_file: string;
      })
    : null;
  if (resume) {
    if (
      !/^prj_[0-9a-f]{16}$/.test(resume.project_id) ||
      !/^br_[0-9a-f]{16}$/.test(resume.branch_id) ||
      !/^ep_[0-9a-f]{16}$/.test(resume.writer_id) ||
      (resume.function_id !== '' &&
        (!/^fnc_[0-9a-f]{16}$/.test(resume.function_id) ||
          !/^op_[0-9a-f]{16}$/.test(resume.failed_deployment_operation_id || '')))
    )
      throw new Error('Exact original Function checkpoint required');
    project = resume.project_id;
    branch = resume.branch_id;
    writer = resume.writer_id;
    functionID = resume.function_id;
    password = (await readFile(resume.database_password_file, 'utf8')).trim();
  }
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  async function save() {
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          branch_id: branch,
          writer_id: writer,
          function_id: functionID,
          operations,
          checks,
          production_qualified: false,
          public_http_tested: false,
          automatic_zero_tested: false,
          credentials_in_evidence: false,
        },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(privateDir!, attempt + '-fixture.json'),
      JSON.stringify(
        {
          project_id: project,
          branch_id: branch,
          writer_id: writer,
          function_id: functionID,
          database_password_file: path.join(privateDir!, attempt + '-database-password'),
        },
        null,
        2,
      ) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string) {
    checks.push(name);
    await save();
  }
  async function wait(response: Response) {
    expect(response.status()).toBe(202);
    const accepted = await response.json();
    const op = (accepted.operation || accepted).id;
    expect(op).toMatch(/^op_[0-9a-f]{16}$/);
    operations.push(op);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${project}/operations/${op}`);
          if (r.status() !== 200) return 'unavailable';
          const o = await r.json();
          if (o.state === 'failed' || o.state === 'cancelled')
            throw new Error(
              `Original Function operation ${op} failed (${o.error_code || o.state})`,
            );
          return o.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
  }
  async function sql(query: string, database = 'postgres') {
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标数据库', { exact: true }).fill(database);
    await page.getByLabel('SQL 查询').fill(query);
    await page.locator('input[type=password]').fill(password);
    const done = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query` &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const r = await done;
    expect(r.status()).toBe(200);
    return r.json();
  }
  const base = () => `/api/v1/projects/${project}/branches/${branch}/functions`;
  async function action(name: string, suffix: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base() + '/hello' + suffix && r.request().method() === method,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name, exact: true }).click();
    return wait(await pending);
  }
  async function invoke(route: string, method = 'GET', body = '') {
    await page.getByLabel('调用方法').selectOption(method);
    await page.getByLabel('调用路径').fill(route);
    await page.getByLabel('调用请求体').fill(body);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base() + '/hello/console-invoke' &&
        r.request().method() === 'POST',
      { timeout: 60000 },
    );
    await page.getByRole('button', { name: '调用函数', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(200);
    const data = await r.json();
    return { ...data, decoded: Buffer.from(data.body, 'base64').toString('utf8') };
  }
  try {
    await writeFile(path.join(privateDir, attempt + '-database-password'), password + '\n', {
      flag: 'wx',
      mode: 0o600,
    });
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (!resume) {
      await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
      await page.getByLabel('项目名称').fill('ci-functions-' + attempt);
      await page.locator('.create-modal input[type=password]').fill(password);
      await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
      await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
      const created = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === '/api/v1/organizations/local/projects' &&
          r.request().method() === 'POST',
        { timeout: 190000 },
      );
      await page.getByRole('button', { name: '创建并验证', exact: true }).click();
      const response = await created;
      expect(response.status()).toBe(202);
      project = (await response.json()).resource.id;
      await wait(response);
      await expect(page.locator('.create-modal')).toBeHidden({ timeout: 490000 });
      const endpointResponse = await context.request.get(`/api/v1/projects/${project}/endpoints`);
      expect(endpointResponse.status()).toBe(200);
      const endpoints = (await endpointResponse.json()).items;
      expect(endpoints).toHaveLength(1);
      writer = endpoints[0].id;
      branch = endpoints[0].branch_id;
      await record('ui_native_owned_project_and_writer');
      // Owner grants are explicit and confined to this fresh test database. They
      // are not automatic global PUBLIC privilege changes made by the Driver.
      for (const statement of [
        'CREATE DATABASE fn_native TEMPLATE template0',
        'ALTER ROLE control_probe CREATEROLE',
      ])
        await sql(statement);
      for (const statement of [
        'CREATE SCHEMA app',
        'CREATE TABLE app.function_probe(id bigserial PRIMARY KEY,marker text NOT NULL)',
        'CREATE SCHEMA private_area',
        'CREATE TABLE private_area.secrets(value text)',
        'REVOKE ALL ON SCHEMA public FROM PUBLIC',
        'REVOKE TEMP ON DATABASE fn_native FROM PUBLIC',
        'GRANT CONNECT ON DATABASE fn_native TO control_probe WITH GRANT OPTION',
        'GRANT USAGE ON SCHEMA app TO control_probe WITH GRANT OPTION',
        'GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA app TO control_probe WITH GRANT OPTION',
        'GRANT USAGE,SELECT,UPDATE ON ALL SEQUENCES IN SCHEMA app TO control_probe WITH GRANT OPTION',
      ])
        await sql(statement, 'fn_native');
      await record('ui_explicit_owned_database_schema_and_provisioner_grants');
    } else {
      const endpoints = (
        await (await context.request.get(`/api/v1/projects/${project}/endpoints`)).json()
      ).items;
      const original = endpoints.find((e: { id: string }) => e.id === writer);
      expect(original.branch_id).toBe(branch);
      expect(original.endpoint_type).toBe('read_write');
      const probe = await sql(
        "SELECT current_database(), current_user, (SELECT rolsuper FROM pg_roles WHERE rolname='control_probe'), (SELECT count(*)::integer FROM app.function_probe)",
        'fn_native',
      );
      expect(probe.rows).toEqual([['fn_native', 'cloud_admin', false, 0]]);
      const definitions = await context.request.get(base());
      expect(definitions.status()).toBe(200);
      const originalDefinitions = (await definitions.json()).items;
      expect(originalDefinitions).toHaveLength(functionID ? 1 : 0);
      if (functionID) {
        const originalFunction = originalDefinitions[0];
        expect(originalFunction.id).toBe(functionID);
        expect(originalFunction.slug).toBe('hello');
        expect(originalFunction.database_name).toBe('fn_native');
        expect(originalFunction.sql_schema).toBe('app');
        expect(originalFunction.state).toBe('degraded');
        expect(originalFunction.active_deployment_id).toBeNull();
        const deployments = await context.request.get(base() + '/hello/deployments');
        expect(deployments.status()).toBe(200);
        const originalDeployments = (await deployments.json()).items;
        expect(originalDeployments).toHaveLength(1);
        expect(originalDeployments[0].id).toBe(originalFunction.target_deployment_id);
        expect(originalDeployments[0].operation_id).toBe(resume!.failed_deployment_operation_id);
        expect(originalDeployments[0].bundle_digest).toMatch(/^[a-f0-9]{64}$/);
        const failedOperation = await context.request.get(
          `/api/v1/projects/${project}/operations/${resume!.failed_deployment_operation_id}`,
        );
        expect(failedOperation.status()).toBe(200);
        const originalOperation = await failedOperation.json();
        expect(originalOperation.state).toBe('failed');
        expect(originalOperation.action).toBe('deploy_function');
        expect(originalOperation.resource_id).toBe(functionID);
        expect(originalOperation.retryable).toBe(true);
        await record('original_failed_deployment_bundle_and_operation_identity_verified');
      }
      await record('resume_original_ui_project_and_grants_without_recreating_resources');
    }
    await page.goto(`/#/projects/${project}/functions`);
    await expect(page.getByText('原生 microVM 函数执行预览', { exact: true })).toBeVisible();
    if (!functionID) {
      await page.getByLabel('函数名称', { exact: true }).fill('hello');
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      const definition = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base() && r.request().method() === 'POST',
        { timeout: 30000 },
      );
      await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
      functionID = (await wait(await definition)).function_id;
      await expect(page.getByRole('button', { name: '查看函数 hello', exact: true })).toBeVisible();
      await record('ui_function_definition_created');
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zipFile);
      await page
        .getByLabel('环境变量 JSON', { exact: true })
        .fill('{"APP_MESSAGE":"first-version"}');
      await action('部署函数版本', '/deployments');
    } else {
      const originalOperation = resume!.failed_deployment_operation_id!;
      await page.goto(`/#/projects/${project}/operations`);
      await page
        .getByRole('button', { name: `查看操作 ${originalOperation} 的步骤`, exact: true })
        .click();
      const retry = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname ===
            `/api/v1/projects/${project}/operations/${originalOperation}/retry` &&
          r.request().method() === 'POST',
        { timeout: 30000 },
      );
      await page.getByRole('button', { name: '重试原操作', exact: true }).click();
      expect((await wait(await retry)).id).toBe(originalOperation);
      const recovered = await context.request.get(base() + '/hello');
      expect(recovered.status()).toBe(200);
      const definition = await recovered.json();
      expect(definition.id).toBe(functionID);
      expect(definition.active_deployment_id).toBe(definition.target_deployment_id);
      const deployments = await context.request.get(base() + '/hello/deployments');
      expect(deployments.status()).toBe(200);
      expect((await deployments.json()).items).toHaveLength(1);
      await record('ui_retry_original_failed_deployment_without_new_bundle_or_identity');
      await page.goto(`/#/projects/${project}/functions`);
      await page.getByRole('button', { name: '查看函数 hello', exact: true }).click();
    }
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled({
      timeout: 30000,
    });
    await record('ui_zip_environment_microvm_deployment_published');
    const read = await invoke('/');
    expect(read.status_code).toBe(200);
    const readData = JSON.parse(read.decoded);
    expect(readData.database).toBe('fn_native');
    expect(readData.role).toBe('fn_' + functionID.slice(4));
    expect(readData.message).toBe('first-version');
    expect(readData.rows).toBe(0);
    await record('function_actual_proxy_tls_sql_identity_and_read');
    const write = await invoke('/write', 'POST', JSON.stringify({ marker: attempt }));
    expect(write.status_code).toBe(200);
    expect(JSON.parse(write.decoded).rows).toBe(1);
    await record('function_actual_neon_sql_write_once');
    const denied = await invoke('/deny');
    expect(JSON.parse(denied.decoded).denied).toBe(true);
    await record('function_other_schema_permission_denied');
    const html = await invoke('/html');
    expect(html.status_code).toBe(200);
    await expect(page.getByTestId('function-invocation-result')).toContainText('<script>');
    expect(
      await page.evaluate(() =>
        Boolean(
          (globalThis as typeof globalThis & { functionXSSExecuted?: boolean }).functionXSSExecuted,
        ),
      ),
    ).toBe(false);
    await record('customer_html_cannot_execute_on_console_origin');
    const appError = await invoke('/error');
    expect(appError.status_code).toBe(500);
    await expect(page.getByTestId('function-invocation-result')).toContainText('HTTP 500');
    await record('application_error_preserved_inside_json_envelope');
    page.on('dialog', (dialog) => void dialog.accept());
    await action('停止函数实例', '/suspend');
    await expect(page.getByRole('button', { name: '部署函数版本', exact: true })).toBeEnabled({
      timeout: 30000,
    });
    await record('ui_manual_stop_original_vm_runner_and_capacity_retirement');
    await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles([]);
    await page
      .getByLabel('环境变量 JSON', { exact: true })
      .fill('{"APP_MESSAGE":"second-version"}');
    await action('部署函数版本', '/deployments');
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled({
      timeout: 30000,
    });
    const second = JSON.parse((await invoke('/')).decoded);
    expect(second.message).toBe('second-version');
    expect(second.rows).toBe(1);
    expect(second.role).toBe(readData.role);
    await record('ui_config_only_redeploy_new_boot_original_sql_role_and_data');
    await action('删除函数', '', 'DELETE');
    await expect(page.getByRole('button', { name: '查看函数 hello', exact: true })).toHaveCount(0);
    const login = await sql(
      `SELECT rolcanlogin FROM pg_roles WHERE rolname='fn_${functionID.slice(4)}'`,
      'fn_native',
    );
    expect(login.rows).toEqual([[false]]);
    const retained = await sql('SELECT count(*)::integer FROM app.function_probe', 'fn_native');
    expect(retained.rows).toEqual([[1]]);
    await record('ui_delete_original_guest_and_disable_owned_sql_login');
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    const suspended = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${writer}/suspend`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    expect((await suspended).status()).toBe(202);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
    await record('ui_owned_compute_zero_after_function_test');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
