#!/usr/bin/env node
// Linux developer delivery: private immutable snapshot -> serial BuildKit OCI
// archives -> checksum-verified containerd imports. Never pushes a registry.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { copyFileSync, existsSync, lstatSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';

const components = ['api','web','gateway','dataapi','postgrest','adapter','auth','ai'];
const ignored = new Set(['.git','.local','node_modules','dist','artifacts','test-results','playwright-report']);
const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const args = process.argv.slice(2), command = args.shift();
function option(name, fallback) { const index=args.indexOf(name); if(index<0)return fallback; const value=args[index+1]; if(!value || value.startsWith('--'))throw new Error(`Missing ${name}`); args.splice(index,2); return value; }
const probeIndex=args.indexOf('--sdk-probe');const buildProbe=probeIndex>=0 || command==='probe';if(buildProbe)args.splice(probeIndex,1);
const output=path.resolve(option('--output','artifacts/local-images'));
const buildctl=option('--buildctl','buildctl'), addr=option('--buildkit-address','unix:///run/buildkit/buildkitd.sock');
const source=path.resolve(option('--source','.'));
const requested=option('--components','api,web,auth');const selected=command==='probe' ? [] : requested.split(',');
const ctr=option('--ctr','ctr'), containerd=option('--containerd-address','/run/k3s/containerd/containerd.sock');
const parentRevision=option('--parent-revision','');
const importAttempt=option('--import-attempt','initial');
if(!/^[a-z0-9-]{1,40}$/.test(importAttempt))throw new Error('Invalid import attempt');
const goProxy=option('--go-proxy','');
if(goProxy && !/^https:\/\/[a-z0-9.-]+(?:,direct)?$/.test(goProxy))throw new Error('Explicit HTTPS Go module mirror required');
if(args.length || !['build','import','probe'].includes(command) || process.platform!=='linux') throw new Error('Use Linux: local-images.mjs build|import --output <new directory> [--source <source> --components api,web,auth --buildctl <binary> --buildkit-address unix:///...]');
if((!selected.length && command!=='probe') || new Set(selected).size!==selected.length || selected.some(x=>!components.includes(x)))throw new Error('Unknown or duplicate component');
if(parentRevision && !/^[a-f0-9]{40}$/.test(parentRevision))throw new Error('Invalid parent Git revision');
if(!addr.startsWith('unix:///') || !path.isAbsolute(containerd))throw new Error('Local Unix sockets required');
function run(binary,argv,log) {
 const child=spawnSync(binary,argv,{encoding:'utf8',maxBuffer:32*1024*1024,timeout:1800000,env:{...process.env,GOMAXPROCS:'2'}});
 if(log)writeFileSync(log,(child.stdout??'')+(child.stderr??''),{mode:0o600,flag:'wx'});
 if(child.error || child.status!==0)throw new Error(`Local ${path.basename(binary)} failed; inspect retained private log`);
 return child.stdout;
}
function walk(directory,prefix='') {
 let files=[];
 for(const name of readdirSync(directory).sort()) {
  if(ignored.has(name))continue;
  const full=path.join(directory,name), relative=path.posix.join(prefix,name), stat=lstatSync(full);
  if(stat.isSymbolicLink())throw new Error('Symlinks require an explicit reviewed build context');
  if(stat.isDirectory())files.push(...walk(full,relative));
  else if(stat.isFile()) {
   if(/^\.env/.test(name) || /\.(?:pem|key|dump|db|etl|py|ps1)$/i.test(name))throw new Error(`Private file refused: ${relative}`);
   files.push({path:relative,sha256:digest(readFileSync(full)),size:stat.size});
  }
 }
 return files;
}
if(command==='build' || command==='probe') {
 if(output===source || output.startsWith(source+path.sep))throw new Error('Output must be outside the source snapshot');
 if(existsSync(output))throw new Error('Exclusive output directory already exists');
 mkdirSync(output,{recursive:true,mode:0o700});
 const files=walk(source), sourceSHA=digest(Buffer.from(JSON.stringify(files)));
 const snapshot=path.join(output,'source');mkdirSync(snapshot,{mode:0o700});
 for(const file of files) {const target=path.join(snapshot,file.path);mkdirSync(path.dirname(target),{recursive:true});copyFileSync(path.join(source,file.path),target);if(digest(readFileSync(target))!==file.sha256)throw new Error('Source changed during copy');}
 const lock=JSON.parse(readFileSync(path.join(snapshot,'containers/builders.lock.json'),'utf8'));
 for(const image of Object.values(lock.images))if(!/@sha256:[a-f0-9]{64}$/.test(image))throw new Error('Builder digest lock required');
 const report={result:'running',linux_only:true,registry_pushed:false,source_kind:'immutable local file snapshot',parent_revision:parentRevision,source_sha256:sourceSHA,go_proxy:goProxy || 'Dockerfile default',source_files:files,images:{},started_at:new Date().toISOString()};
 const save=()=>writeFileSync(path.join(output,'result.json'),JSON.stringify(report,null,2)+'\n',{mode:0o600});save();
 try {
  for(const component of selected) {
   const name=`neon.local/control-${component}:snapshot-${sourceSHA.slice(0,16)}`;
   const archive=path.join(output,component+'.oci.tar'),meta=path.join(output,component+'.metadata.json');
   const build=['--addr',addr,'build','--progress=plain','--frontend=dockerfile.v0','--local','context='+snapshot,'--local','dockerfile='+snapshot,'--opt','filename=Dockerfile.'+component,'--opt','platform=linux/amd64'];
   const buildArgs={GO_BUILDER_IMAGE:lock.images.go,NODE_BUILDER_IMAGE:lock.images.node,WEB_RUNTIME_IMAGE:lock.images.web,PLAYWRIGHT_IMAGE:lock.images.playwright};
   if(goProxy)buildArgs.GOPROXY=goProxy;
   for(const [key,value] of Object.entries(buildArgs))build.push('--opt',`build-arg:${key}=${value}`);
   for(const key of ['HTTP_PROXY','HTTPS_PROXY','NO_PROXY'])if(process.env[key])build.push('--opt',`build-arg:${key}=${process.env[key]}`);
   build.push('--opt','label:org.opencontainers.image.source=https://github.com/william-lbn/control-plane','--opt',`label:io.neon.control.source-sha256=${sourceSHA}`,'--output',`type=oci,name=${name},dest=${archive}`,'--metadata-file',meta);
   run(buildctl,build,path.join(output,component+'.build.private.log'));
   const imageDigest=JSON.parse(readFileSync(meta,'utf8'))['containerimage.digest'];
   if(!/^sha256:[a-f0-9]{64}$/.test(imageDigest))throw new Error('OCI output digest missing');
   report.images[component]={name,reference:name.split(':')[0]+'@'+imageDigest,digest:imageDigest,archive,archive_sha256:digest(readFileSync(archive)),source_sha256:sourceSHA};save();
   console.log(JSON.stringify({component,digest:imageDigest,registry_pushed:false}));
  }
  if(buildProbe) {
   const argv=['--addr',addr,'build','--progress=plain','--frontend=dockerfile.v0','--local','context='+snapshot,'--local','dockerfile='+snapshot,'--opt','filename=Dockerfile.sdk-probe','--opt','platform=linux/amd64','--opt','build-arg:GO_BUILDER_IMAGE='+lock.images.go,'--output','type=local,dest='+path.join(output,'probe')];
   if(goProxy)argv.push('--opt','build-arg:GOPROXY='+goProxy);
   run(buildctl,argv,path.join(output,'probe.build.private.log'));
   const file=path.join(output,'probe/storage-sdk-probe');
   report.sdk_probe={sha256:digest(readFileSync(file)),source_sha256:sourceSHA,platform:'linux/amd64',runtime_service:false};save();
  }
  report.result='pass';
 } catch(error) {report.result='fail';report.error='Build failed; private log retained';throw error;}
 finally {report.finished_at=new Date().toISOString();save();}
} else {
 const report=JSON.parse(readFileSync(path.join(output,'result.json'),'utf8'));
 if(report.result!=='pass' || !report.linux_only || report.registry_pushed!==false)throw new Error('Successful local build receipt required');
 const receipt=path.join(output,`import-${importAttempt}-${digest(Buffer.from(containerd)).slice(0,8)}.json`);
 if(existsSync(receipt))throw new Error('Import receipt exists; inspect it before retry');
 const imported=[];
 for(const [component,image] of Object.entries(report.images)) {
  if(!components.includes(component) || path.resolve(image.archive)!==path.join(output,component+'.oci.tar') || digest(readFileSync(image.archive))!==image.archive_sha256)throw new Error('Archive checksum or path mismatch');
  // The transfer importer can register the archive wrapper as a runtime image.
  // Synthetic unqualified digest aliases then become unusable CRI references
  // after a daemon restart. Import named images directly and add the verified
  // fully qualified digest reference below.
  run(ctr,['--address',containerd,'--namespace','k8s.io','images','import','--local','--digests','--skip-digest-for-named','--platform','linux/amd64',image.archive],path.join(output,component+'.'+importAttempt+'.import.private.log'));
  // OCI exporters may publish an index plus one platform manifest. containerd
  // preserves the index on its tag without creating a repository@digest alias.
  // Check the exact tag target before creating that immutable alias.
  const rows=run(ctr,['--address',containerd,'--namespace','k8s.io','images','list']).split('\n').map(line=>line.trim().split(/\s+/));
  if(!rows.some(row=>row[0]===image.name && row[2]===image.digest))throw new Error('Imported tag target differs from built OCI digest');
  const existing=rows.find(row=>row[0]===image.reference);
  if(existing && existing[2]!==image.digest)throw new Error('Existing digest alias points elsewhere');
  if(!existing)run(ctr,['--address',containerd,'--namespace','k8s.io','images','tag',image.name,image.reference]);
  const listed=run(ctr,['--address',containerd,'--namespace','k8s.io','images','list','--quiet']);
  if(!listed.split('\n').includes(image.reference))throw new Error('Digest reference missing after import');
  imported.push({component,reference:image.reference});
  writeFileSync(receipt,JSON.stringify({result:'running',imported},null,2)+'\n',{mode:0o600});
 }
 writeFileSync(receipt,JSON.stringify({result:'pass',imported,registry_pushed:false},null,2)+'\n',{mode:0o600});
 console.log(JSON.stringify({result:'pass',imported:imported.length,registry_pushed:false}));
}
