#!/usr/bin/env node
// Explicit Linux integration entry. No credentials are accepted on argv.
import { readFile, writeFile, mkdtemp, chmod, stat } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

export const probeSource = String.raw`import base64,hashlib,json,os,re,stat,sys,zlib
from pathlib import Path
import boto3,botocore
from botocore.config import Config
from botocore.exceptions import ClientError
from xml.etree import ElementTree
cfg=Path(sys.argv[1]);assert stat.S_IMODE(cfg.stat().st_mode)==0o600
c=json.loads(cfg.read_text());assert c['owned_fixture'] is True and re.fullmatch(r'ci-checksum-[0-9]{14}',c['bucket'])
assert c['endpoint']=='http://192.168.146.100:30893' and c['project_id']=='prj_6f3c51bd15b772e3' and c['branch_id']=='br_6302722fb381d460'
checkpoint=cfg.parent/'sdk-checkpoint.private.json';output=cfg.parent/'sdk-result.public.json'
state=json.loads(checkpoint.read_text()) if checkpoint.exists() else {'bucket':c['bucket'],'checks':[],'running':''}
assert state['bucket']==c['bucket'] and not state['running'],'Observe the original mutation before resuming'
def save():
 checkpoint.write_text(json.dumps(state));checkpoint.chmod(0o600)
def step(name,work):
 if name in state['checks']:return
 state['running']=name;save();work();state['checks'].append(name);state['running']='';save()
client=boto3.client('s3',endpoint_url=c['endpoint'],region_name='us-east-2',aws_access_key_id=c['access_key'],aws_secret_access_key=c['secret_key'],config=Config(signature_version='s3v4',s3={'addressing_style':'path','payload_signing_enabled':True},proxies={},retries={'total_max_attempts':1},connect_timeout=15,read_timeout=140))
# The retained Boto3 1.34 model predates CRC64NVME. Use its standard signing
# hook and inspect real response headers/XML; no replacement SDK download.
override=None
def crc64_header(params,**kwargs):
 if override is not None:params['headers'].update({'x-amz-sdk-checksum-algorithm':'CRC64NVME','x-amz-checksum-crc64nvme':override})
client.meta.events.register('before-call.s3.PutObject',crc64_header)
def attribute_wire(http_response,parsed,**kwargs):
 root=ElementTree.fromstring(http_response.content);node=root.find('{http://s3.amazonaws.com/doc/2006-03-01/}Checksum')
 if node is not None:parsed['_wire_checksum']={v.tag.rsplit('}',1)[-1]:v.text for v in node}
client.meta.events.register('after-call.s3.GetObjectAttributes',attribute_wire)
body=b'123456789'
checks={'ChecksumSHA256':base64.b64encode(hashlib.sha256(body).digest()).decode(),'ChecksumSHA1':base64.b64encode(hashlib.sha1(body).digest()).decode(),'ChecksumCRC32':base64.b64encode(zlib.crc32(body).to_bytes(4,'big')).decode(),'ChecksumCRC32C':base64.b64encode(bytes.fromhex('e3069283')).decode(),'ChecksumCRC64NVME':base64.b64encode(bytes.fromhex('ae8b14860a799888')).decode()}
def read(key,expected=checks):
 for method in ['head_object','get_object','get_object_attributes']:
  args={'Bucket':c['bucket'],'Key':key}
  if method=='get_object_attributes':args['ObjectAttributes']=['Checksum','ObjectSize']
  else:args['ChecksumMode']='ENABLED'
  result=getattr(client,method)(**args)
  if method=='get_object_attributes':values=result['_wire_checksum']
  else:
   headers=result['ResponseMetadata']['HTTPHeaders'];values={name:headers['x-amz-checksum-'+name[len('Checksum'):].lower()] for name in expected}
   values['ChecksumType']=headers['x-amz-checksum-type']
  for name,value in expected.items():assert values[name]==value,(method,name)
  assert values['ChecksumType']=='FULL_OBJECT'
  if method=='get_object':assert result['Body'].read()==(body if expected==checks else b'')
report={'result':'fail','checks':state['checks'],'boto3':boto3.__version__,'botocore':botocore.__version__,'credentials_exported':False,'accepted_object_content_bytes':18,'retained_SDK_CRC64_signing_hook':True}
try:
 def ordinary():
  global override
  override=checks['ChecksumCRC64NVME'];client.put_object(Bucket=c['bucket'],Key='checksums/source',Body=body,ContentType='application/octet-stream');override=None;read('checksums/source')
 step('actual_Boto3_CRC64NVME_PUT_and_all_five_HEAD_GET_attributes',ordinary)
 def wrong():
  global override
  override='AAAAAAAAAAA='
  try:client.put_object(Bucket=c['bucket'],Key='rejected/bad-crc64',Body=body)
  except ClientError as e:assert e.response['Error']['Code']=='BadDigest'
  else:raise AssertionError('Bad CRC64 acknowledged')
  finally:override=None
  try:client.head_object(Bucket=c['bucket'],Key='rejected/bad-crc64')
  except ClientError as e:assert e.response['ResponseMetadata']['HTTPStatusCode']==404
  else:raise AssertionError('Bad CRC64 published an object')
 step('bad_CRC64_rejected_without_publication',wrong)
 def copied():
  client.copy_object(Bucket=c['bucket'],Key='checksums/copy',CopySource={'Bucket':c['bucket'],'Key':'checksums/source'});read('checksums/copy')
  client.put_object(Bucket=c['bucket'],Key='checksums/copy',Body=b'')
  empty={'ChecksumSHA256':base64.b64encode(hashlib.sha256(b'').digest()).decode(),'ChecksumSHA1':base64.b64encode(hashlib.sha1(b'').digest()).decode(),'ChecksumCRC32':'AAAAAA==','ChecksumCRC32C':'AAAAAA==','ChecksumCRC64NVME':'AAAAAAAAAAA='};read('checksums/copy',empty)
 step('copy_preserves_and_overwrite_replaces_all_checksums',copied)
 def multipart():
  value=client.create_multipart_upload(Bucket=c['bucket'],Key='checksums/multipart');state['upload_id']=value['UploadId'];save()
  part=client.upload_part(Bucket=c['bucket'],Key='checksums/multipart',UploadId=state['upload_id'],PartNumber=1,Body=body)
  client.complete_multipart_upload(Bucket=c['bucket'],Key='checksums/multipart',UploadId=state['upload_id'],MultipartUpload={'Parts':[{'PartNumber':1,'ETag':part['ETag']}]});read('checksums/multipart')
 step('multipart_publishes_full_byte_checksums_in_same_generation',multipart)
 report['result']='pass'
except Exception as e:report['error_type']=type(e).__name__
finally:
 report['checks']=state['checks'];output.write_text(json.dumps(report));output.chmod(0o600)
print(json.dumps(report));sys.exit(0 if report['result']=='pass' else 1)
`;

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
  if (process.platform !== 'linux' || !process.env.NEON_STORAGE_CHECKSUM_FIXTURE) throw new Error('Explicit protected Linux fixture required');
  const fixture = resolve(process.env.NEON_STORAGE_CHECKSUM_FIXTURE);
  if ((await stat(fixture)).mode % 512 !== 0o600) throw new Error('Fixture must be private');
  const parsed = JSON.parse(await readFile(fixture, 'utf8'));
  if (parsed.owned_fixture !== true) throw new Error('Owned fixture required');
  const temporary = await mkdtemp(join(dirname(fixture), 'probe-source-'));
  await chmod(temporary, 0o700);
  const source = join(temporary, 'probe.py');
  await writeFile(source, probeSource, { mode: 0o600 });
  try {
    const result = await promisify(execFile)('python3', [source, fixture], { timeout: 580000, maxBuffer: 65536,
      env: { ...process.env, NO_PROXY: '*', no_proxy: '*', HTTP_PROXY: '', HTTPS_PROXY: '', ALL_PROXY: '' } });
    process.stdout.write(result.stdout);
  } catch {
    process.stderr.write('Storage checksum probe failed; inspect its original protected checkpoint\n');
    process.exitCode = 1;
  }
}
