#!/usr/bin/env node
// Explicit Linux-only probe. Existing Boto3 signs every request; secrets stay
// in the protected fixture and its retained mutation checkpoints.
import { readFile, writeFile, mkdtemp, chmod, stat } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

export const probeSource = String.raw`import base64,hashlib,json,os,re,stat,sys,zlib
from pathlib import Path
from xml.etree import ElementTree as ET
import boto3,botocore
from botocore.config import Config
from botocore.exceptions import ClientError
cfg=Path(sys.argv[1]);assert stat.S_IMODE(cfg.stat().st_mode)==0o600
c=json.loads(cfg.read_text());assert c['owned_fixture'] is True and re.fullmatch(r'ci-mpchecksum-[0-9]{14}',c['bucket'])
assert c['endpoint']=='http://192.168.146.100:30893' and c['project_id']=='prj_6f3c51bd15b772e3' and c['branch_id']=='br_6302722fb381d460'
checkpoint=cfg.parent/'sdk-checkpoint.private.json';output=cfg.parent/'sdk-result.public.json'
state=json.loads(checkpoint.read_text()) if checkpoint.exists() else {'bucket':c['bucket'],'checks':[],'uploads':{},'parts':{},'running':''}
assert state['bucket']==c['bucket'] and not state['running'],'Observe the original mutation before resuming'
def save():
 checkpoint.write_text(json.dumps(state));checkpoint.chmod(0o600)
def step(name,work):
 if name in state['checks']:return
 state['running']=name;save();work();state['checks'].append(name);state['running']='';save()
client=boto3.client('s3',endpoint_url=c['endpoint'],region_name='us-east-2',aws_access_key_id=c['access_key'],aws_secret_access_key=c['secret_key'],config=Config(signature_version='s3v4',s3={'addressing_style':'path','payload_signing_enabled':True},proxies={},retries={'total_max_attempts':1},connect_timeout=15,read_timeout=140))
NS='http://s3.amazonaws.com/doc/2006-03-01/';ET.register_namespace('',NS)
overrides={};completion_part=None
def signed_headers(params,model,**kwargs):
 params['headers'].update(overrides.get(model.name,{}))
 if model.name=='CompleteMultipartUpload' and completion_part is not None:
  root=ET.fromstring(params['body']);part=root.find('{'+NS+'}Part');assert part is not None
  name,value=completion_part;node=ET.SubElement(part,'{'+NS+'}'+name);node.text=value
  params['body']=ET.tostring(root,encoding='utf-8')
client.meta.events.register('before-call.s3',signed_headers)
def wire(http_response,parsed,model,**kwargs):
 if model.name in ['CompleteMultipartUpload','CopyObject','GetObjectAttributes','ListParts','ListMultipartUploads']:
  parsed['_wire']=ET.fromstring(http_response.content)
client.meta.events.register('after-call.s3',wire)
body=b'123456789';b64=lambda value:base64.b64encode(value).decode()
checks={'SHA256':b64(hashlib.sha256(body).digest()),'SHA1':b64(hashlib.sha1(body).digest()),'CRC32':b64(zlib.crc32(body).to_bytes(4,'big')),'CRC32C':b64(bytes.fromhex('e3069283')),'CRC64NVME':b64(bytes.fromhex('ae8b14860a799888'))}
def composite(algorithm):
 raw=base64.b64decode(checks[algorithm])
 if algorithm=='SHA256':value=hashlib.sha256(raw).digest()
 elif algorithm=='SHA1':value=hashlib.sha1(raw).digest()
 elif algorithm=='CRC32':value=zlib.crc32(raw).to_bytes(4,'big')
 elif algorithm=='CRC32C':
  n=0xffffffff
  for byte in raw:
   n^=byte
   for _ in range(8):n=(n>>1)^ (0x82f63b78 if n&1 else 0)
  value=(n^0xffffffff).to_bytes(4,'big')
 else:raise AssertionError('unsupported composite')
 return b64(value)+'-1'
def head(key,algorithm,kind,value):
 result=client.head_object(Bucket=c['bucket'],Key=key,ChecksumMode='ENABLED');h=result['ResponseMetadata']['HTTPHeaders']
 assert h['x-amz-checksum-type']==kind and h['x-amz-checksum-'+algorithm.lower()]==value
 assert len([name for name in h if name.startswith('x-amz-checksum-') and name!='x-amz-checksum-type'])==1
 return result
def denied(work):
 try:work()
 except ClientError as error:assert error.response['ResponseMetadata']['HTTPStatusCode'] in [400,409];return
 raise AssertionError('invalid declaration accepted')
def run_combination(algorithm,kind):
 global overrides,completion_part
 label=algorithm.lower()+'-'+kind.lower();key='source-'+label;copy='copy-'+label;field='Checksum'+algorithm
 def initiate():
  global overrides
  overrides={'CreateMultipartUpload':{'x-amz-checksum-algorithm':algorithm,'x-amz-checksum-type':kind}}
  result=client.create_multipart_upload(Bucket=c['bucket'],Key=key,ContentType='text/plain');state['uploads'][label]=result['UploadId'];save()
 step(label+':initiate',initiate);upload=state['uploads'][label]
 if algorithm=='SHA256':
  def bad_part():
   global overrides
   overrides={'UploadPart':{'x-amz-sdk-checksum-algorithm':algorithm,'x-amz-checksum-sha256':b64(bytes(32))}}
   denied(lambda:client.upload_part(Bucket=c['bucket'],Key=key,UploadId=upload,PartNumber=1,Body=body))
   overrides={};assert not client.list_parts(Bucket=c['bucket'],Key=key,UploadId=upload).get('Parts',[])
  step(label+':bad-part-no-publication',bad_part)
 def part():
  global overrides
  overrides={'UploadPart':{'x-amz-sdk-checksum-algorithm':algorithm,'x-amz-checksum-'+algorithm.lower():checks[algorithm]}}
  result=client.upload_part(Bucket=c['bucket'],Key=key,UploadId=upload,PartNumber=1,Body=body)
  assert result['ResponseMetadata']['HTTPHeaders']['x-amz-checksum-'+algorithm.lower()]==checks[algorithm]
  state['parts'][label]=result['ETag'];save()
 step(label+':part',part);etag=state['parts'][label]
 def listing():
  global overrides
  overrides={};result=client.list_parts(Bucket=c['bucket'],Key=key,UploadId=upload)
  assert result['_wire'].findtext('{'+NS+'}ChecksumAlgorithm')==algorithm
  assert result['_wire'].findtext('{'+NS+'}ChecksumType')==kind
  assert result['_wire'].findtext('.//{'+NS+'}'+field)==checks[algorithm]
 step(label+':list-parts',listing)
 value=checks[algorithm] if kind=='FULL_OBJECT' else composite(algorithm)
 if algorithm=='SHA256':
  def bad_complete():
   global overrides,completion_part
   overrides={'CompleteMultipartUpload':{'x-amz-checksum-type':'FULL_OBJECT'}};completion_part=(field,checks[algorithm])
   denied(lambda:client.complete_multipart_upload(Bucket=c['bucket'],Key=key,UploadId=upload,MultipartUpload={'Parts':[{'PartNumber':1,'ETag':etag}]}))
   completion_part=None;overrides={}
   try:client.head_object(Bucket=c['bucket'],Key=key)
   except ClientError as error:assert error.response['ResponseMetadata']['HTTPStatusCode']==404
   else:raise AssertionError('invalid completion published key')
  step(label+':bad-completion-no-publication',bad_complete)
 def complete():
  global overrides,completion_part
  overrides={'CompleteMultipartUpload':{'x-amz-checksum-type':kind,'x-amz-checksum-'+algorithm.lower():value,'x-amz-mp-object-size':'9'}};completion_part=(field,checks[algorithm])
  result=client.complete_multipart_upload(Bucket=c['bucket'],Key=key,UploadId=upload,MultipartUpload={'Parts':[{'PartNumber':1,'ETag':etag}]})
  assert result['_wire'].findtext('{'+NS+'}'+field)==value and result['_wire'].findtext('{'+NS+'}ChecksumType')==kind
  completion_part=None
 step(label+':complete',complete)
 def reads():
  global overrides
  overrides={};head(key,algorithm,kind,value)
  # The retained SDK model predates composite body validation. HEAD/attributes
  # prove the negotiated checksum; GET verifies the actual small body separately.
  result=client.get_object(Bucket=c['bucket'],Key=key);assert result['Body'].read()==body;result['Body'].close()
  result=client.get_object_attributes(Bucket=c['bucket'],Key=key,ObjectAttributes=['Checksum','ObjectParts','ObjectSize'])
  assert result['_wire'].findtext('.//{'+NS+'}Checksum/{'+NS+'}'+field)==value
  assert result['_wire'].findtext('.//{'+NS+'}ObjectParts/{'+NS+'}Part/{'+NS+'}'+field)==checks[algorithm]
 step(label+':read-attributes',reads)
 def copying():
  global overrides
  overrides={};result=client.copy_object(Bucket=c['bucket'],Key=copy,CopySource={'Bucket':c['bucket'],'Key':key})
  assert result['_wire'].findtext('{'+NS+'}'+field)==value and result['_wire'].findtext('{'+NS+'}ChecksumType')==kind
  head(copy,algorithm,kind,value)
 step(label+':copy-policy',copying)
 def overwrite():
  global overrides
  overrides={};client.put_object(Bucket=c['bucket'],Key=key,Body=b'')
  result=client.head_object(Bucket=c['bucket'],Key=key,ChecksumMode='ENABLED')
  assert result['ContentLength']==0 and result['ResponseMetadata']['HTTPHeaders']['x-amz-checksum-type']=='FULL_OBJECT'
 step(label+':overwrite',overwrite)
 def replay():
  global overrides
  complete();overrides={};result=client.head_object(Bucket=c['bucket'],Key=key);assert result['ContentLength']==0
 step(label+':original-receipt-replay',replay)
def bad_declarations():
 global overrides
 for algorithm,kind in [('CRC64NVME','COMPOSITE'),('SHA256','FULL_OBJECT'),('SHA1','FULL_OBJECT')]:
  overrides={'CreateMultipartUpload':{'x-amz-checksum-algorithm':algorithm,'x-amz-checksum-type':kind}}
  denied(lambda:client.create_multipart_upload(Bucket=c['bucket'],Key='invalid-negotiation',ContentType='text/plain'))
 overrides={}
try:
 step('unsupported-type-denials',bad_declarations)
 for algorithm,kind in [('CRC32','FULL_OBJECT'),('CRC32','COMPOSITE'),('CRC32C','FULL_OBJECT'),('CRC32C','COMPOSITE'),('CRC64NVME','FULL_OBJECT'),('SHA1','COMPOSITE'),('SHA256','COMPOSITE')]:run_combination(algorithm,kind)
 result={'result':'pass','checks':state['checks'],'combinations':7,'sdk':'boto3 '+boto3.__version__,'botocore':botocore.__version__,'new_objects':14,'retained_visible_bytes':63,'official_SDK_signing':True,'SDK_downloads':False,'five_GiB_bytes_tested':False,'original_checkpoints_retained':True}
except Exception as error:result={'result':'fail','checks':state['checks'],'running':state['running'],'error_type':type(error).__name__}
output.write_text(json.dumps(result,indent=2)+'\n');output.chmod(0o600);print(json.dumps(result));sys.exit(0 if result['result']=='pass' else 1)
`;

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (process.platform !== 'linux' || !process.env.NEON_STORAGE_MULTIPART_CHECKSUM_FIXTURE) throw new Error('Explicit protected Linux fixture required');
  const fixture = resolve(process.env.NEON_STORAGE_MULTIPART_CHECKSUM_FIXTURE);
  if ((await stat(fixture)).mode % 512 !== 0o600) throw new Error('Fixture must be private');
  if (JSON.parse(await readFile(fixture, 'utf8')).owned_fixture !== true) throw new Error('Owned fixture required');
  const temporary = await mkdtemp(join(dirname(fixture), 'probe-source-'));
  await chmod(temporary, 0o700);
  const source = join(temporary, 'probe.py');
  await writeFile(source, probeSource, { mode: 0o600 });
  try {
    const result = await promisify(execFile)('python3', [source, fixture], { timeout: 580000, maxBuffer: 65536,
      env: { ...process.env, NO_PROXY: '*', no_proxy: '*', HTTP_PROXY: '', HTTPS_PROXY: '', ALL_PROXY: '' } });
    process.stdout.write(result.stdout);
  } catch {
    process.stderr.write('Multipart checksum probe failed; inspect the original protected checkpoint\n');
    process.exitCode = 1;
  }
}
