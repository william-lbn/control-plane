import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

const boto3Source = String.raw`from pathlib import Path
import base64,hashlib,json,os,re,stat
import boto3,botocore
from botocore.config import Config
from botocore.exceptions import ClientError
p=Path(__file__).resolve().parent
assert re.fullmatch(r'/var/lib/neon-control/linux-delivery/runs/publication-ui-[0-9]{14}/private',str(p))
cfg=p/'s3-capabilities-sdk.private.json';assert stat.S_IMODE(cfg.stat().st_mode)==0o600
c=json.loads(cfg.read_text());assert re.fullmatch('ci-s3-capabilities-[0-9]{13}',c['bucket']) and c['endpoint']=='http://192.168.146.100:30893'
statefile=p/'boto3-checkpoint.private.json'
state=json.loads(statefile.read_text()) if statefile.exists() else {'bucket':c['bucket'],'checks':[],'running':''}
assert state['bucket']==c['bucket']
def save():
 statefile.write_text(json.dumps(state));statefile.chmod(0o600);os.chown(statefile,cfg.stat().st_uid,cfg.stat().st_gid)
def step(name,run):
 if name in state['checks']:return
 assert not state['running'],'Inspect original SDK mutation before continuing'
 state['running']=name;save();run();state['checks'].append(name);state['running']='';save()
client=boto3.client('s3',endpoint_url=c['endpoint'],region_name=c['region'],aws_access_key_id=c['access_key'],aws_secret_access_key=c['secret_key'],config=Config(signature_version='s3v4',s3={'addressing_style':'path','payload_signing_enabled':True},proxies={},retries={'total_max_attempts':1},connect_timeout=15,read_timeout=120))
result={'result':'fail','checks':state['checks'],'boto3':boto3.__version__,'botocore':botocore.__version__,'credentials_exported':False}
try:
 def attrs():
  value=client.get_object_attributes(Bucket=c['bucket'],Key='capabilities/private-multipart.txt',ObjectAttributes=['ETag','ObjectSize','Checksum','ObjectParts','StorageClass'],MaxParts=1)
  assert value['ObjectParts']['TotalPartsCount']==1 and len(value['ObjectParts']['Parts'])==1 and value['ObjectParts']['Parts'][0]['PartNumber']==1 and value['ObjectSize']>0
  acl=client.get_object_acl(Bucket=c['bucket'],Key='capabilities/source.txt');assert acl['Owner']['ID']==c['branch_id']
 step('Boto3_typed_attributes_and_object_ACL',attrs)
 def put():
  body=b'Owned boto3 SHA256 bytes, retained on this new bucket only.\n'
  sha=base64.b64encode(hashlib.sha256(body).digest()).decode()
  client.put_object(Bucket=c['bucket'],Key='capabilities/boto3.txt',Body=body,ChecksumSHA256=sha,ContentType='text/plain',Metadata={'sdk':'boto3'})
  response=client.get_object(Bucket=c['bucket'],Key='capabilities/boto3.txt');assert response['Body'].read()==body and response['Metadata']['sdk']=='boto3'
 step('Boto3_signed_checksum_PUT_and_exact_GET',put)
 def denied():
  try:client.put_object(Bucket=c['bucket'],Key='rejected/boto3-public.txt',Body=b'rejected',ACL='public-read')
  except ClientError as e:assert e.response['ResponseMetadata']['HTTPStatusCode']==501
  else:raise AssertionError('Unsupported public ACL acknowledged')
  try:client.head_object(Bucket=c['bucket'],Key='rejected/boto3-public.txt')
  except ClientError as e:assert e.response['ResponseMetadata']['HTTPStatusCode']==404
  else:raise AssertionError('Rejected object published')
 step('Boto3_public_grant_rejected_without_object',denied)
 result['result']='pass'
except Exception as e:result['error_type']=type(e).__name__
finally:
 result['checks']=state['checks'];out=p/'boto3-result.private.json.tmp';out.write_text(json.dumps(result));out.chmod(0o600);os.chown(out,cfg.stat().st_uid,cfg.stat().st_gid);out.replace(p/'boto3-result.private.json')
`;

test('S3 capability matrix rejects unsupported grants and interoperates with two SDKs', async ({
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
        bucket: 'ci-s3-capabilities-' + Date.now(),
        checks: [],
        stage: '',
        credential: null,
        operations: [],
      };
  if (
    state.project_id !== f.project_id ||
    state.branch_id !== f.branch_id ||
    !/^ci-s3-capabilities-\d+$/.test(state.bucket)
  )
    throw Error('Original settings checkpoint differs');
  const base = `/api/v1/projects/${f.project_id}/branches/${f.branch_id}`;
  let result = 'running';
  async function save(check?: string) {
    if (check && !state.checks.includes(check)) state.checks.push(check);
    await writeFile(
      path.join(privateDir!, 's3-capabilities-checkpoint.private.json'),
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
    await step('actual_SDK_capability_matrix_closed_loop', async () => {
      if (!state.credential) throw Error('Original SDK credential required');
      const cfg = path.join(privateDir, 's3-capabilities-sdk.private.json');
      await writeFile(
        cfg,
        JSON.stringify({
          endpoint: 'http://192.168.146.100:30893',
          region: 'us-east-2',
          access_key: state.credential.token_id,
          secret_key: state.credential.s3_secret_access_key,
          bucket: state.bucket,
          branch_id: f.branch_id,
          mode: 's3-capabilities',
          output: path.join(privateDir, 's3-capabilities-sdk-checkpoint.private.json'),
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
    await step('Boto3_cross_SDK_closed_loop', async () => {
      const priorBoto3Checks = state.checks.filter((v) =>
        [
          'Boto3_typed_attributes_and_object_ACL',
          'Boto3_signed_checksum_PUT_and_exact_GET',
          'Boto3_public_grant_rejected_without_object',
        ].includes(v),
      );
      if (priorBoto3Checks.length)
        await writeFile(
          path.join(privateDir, 'boto3-checkpoint.private.json'),
          JSON.stringify({
            bucket: state.bucket,
            checks: priorBoto3Checks,
            running: '',
          }),
          { mode: 0o600 },
        );
      if (!state.credential) throw Error('Original credential required');
      await writeFile(
        path.join(privateDir, 's3-capabilities-sdk.private.json'),
        JSON.stringify({
          endpoint: 'http://192.168.146.100:30893',
          region: 'us-east-2',
          access_key: state.credential.token_id,
          secret_key: state.credential.s3_secret_access_key,
          bucket: state.bucket,
          branch_id: f.branch_id,
          mode: 's3-capabilities',
          output: path.join(privateDir, 's3-capabilities-sdk-checkpoint.private.json'),
        }),
        { mode: 0o600 },
      );
      await writeFile(path.join(privateDir, 'boto3-probe.private.py'), boto3Source, {
        mode: 0o600,
      });
      await writeFile(path.join(privateDir, 'external-sdk.ready'), 'ready', { mode: 0o600 });
      const output = path.join(privateDir, 'boto3-result.private.json');
      await expect
        .poll(
          async () => {
            try {
              return JSON.parse(await readFile(output, 'utf8')).result;
            } catch (e) {
              if ((e as { code?: string }).code === 'ENOENT') return 'pending';
              throw e;
            }
          },
          { timeout: 180000, intervals: [1000, 2500] },
        )
        .not.toBe('pending');
      const report = JSON.parse(await readFile(output, 'utf8'));
      if (report.result !== 'pass')
        throw Error('Inspect original Boto3 action: ' + report.error_type);
      for (const check of report.checks) await save(check);
    });
    await step('UI_observes_only_verified_SDK_objects', async () => {
      await selectBucket();
      await page.getByRole('button', { name: '加载对象', exact: true }).click();
      await expect(
        page.getByRole('cell', { name: 'capabilities/source.txt', exact: true }),
      ).toBeVisible();
      await expect(
        page.getByRole('cell', { name: 'capabilities/private-multipart.txt', exact: true }),
      ).toBeVisible();
      await expect(
        page.getByRole('cell', { name: 'capabilities/boto3.txt', exact: true }),
      ).toBeVisible();
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
  }
});
