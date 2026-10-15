// Operator-side read-only Linux observer. Keep Kubernetes credentials outside
// browser Jobs; the browser supplies only a bounded stage/owned Endpoint marker.
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import path from 'node:path';
const args=Object.fromEntries(process.argv.slice(2).reduce((pairs,v,i,all)=>i%2?pairs:[...pairs,[v.replace(/^--/,''),all[i+1]]],[]));
if(process.platform!=='linux'||!args.marker||!args.result||!/^prj_[a-f0-9]{16}$/.test(args.project))throw new Error('Linux protected marker/result and project required');
const kubectl=args.kubectl||'kubectl',namespace=args.namespace||'neon';if(!/^[a-z0-9-]+$/.test(namespace))throw new Error('Invalid namespace');
function kube(parts){return execFileSync(kubectl,[...(args.server?['--server',args.server]:[]),'--request-timeout=15s','-n',namespace,...parts],{encoding:'utf8',timeout:20000,stdio:['ignore','pipe','pipe']});}
function cpu(value){return typeof value==='string'&&value.endsWith('m')?Number(value.slice(0,-1)):Number(value)*1000}
function bytes(value){const m=String(value).match(/^(\d+)(Ki|Mi|Gi)?$/);return m?Number(m[1])*({Ki:1024,Mi:1048576,Gi:1073741824}[m[2]]||1):null}
const report={result:'running',read_only:true,project_id:args.project,samples:[],errors:[]};
function save(){const target=path.resolve(args.result);fs.writeFileSync(target+'.partial',JSON.stringify(report)+'\n',{mode:0o600});if(args['result-uid']){if(!/^\d+$/.test(args['result-uid']))throw new Error('Invalid result owner');fs.chownSync(target+'.partial',Number(args['result-uid']),Number(args['result-uid']))}fs.renameSync(target+'.partial',target);}
save();const deadline=Date.now()+1800000;
while(Date.now()<deadline){
 let marker;try{marker=JSON.parse(fs.readFileSync(args.marker,'utf8'))}catch{await new Promise(r=>setTimeout(r,1000));continue}
 if(marker.abort){report.result='interrupted';save();process.exit(1)}
 if(marker.done){if(report.samples.length!==3)throw new Error('Incomplete observation');report.result='pass';save();process.exit(0)}
 if(!/^ep_[a-f0-9]{16}$/.test(marker.endpoint_id)||!['initial','raised','returned'].includes(marker.stage)||![250,500].includes(marker.cpu_milli)||![1024,2048].includes(marker.memory_mib))throw new Error('Invalid observation marker');
 if(report.samples.some(v=>v.stage===marker.stage)){await new Promise(r=>setTimeout(r,1000));continue}
 try{
  const name='cp-'+marker.endpoint_id.slice(3),vm=JSON.parse(kube(['get','virtualmachines.vm.neon.tech',name,'-o','json']));if(vm.metadata.labels['neon-control/project-id']!==args.project||vm.metadata.labels['neon-control/endpoint-id']!==marker.endpoint_id)throw new Error('ownership');
  if(cpu(vm.spec.guest.cpus.use)!==marker.cpu_milli||cpu(vm.status.cpus)!==marker.cpu_milli||bytes(vm.status.memorySize)!==marker.memory_mib*1048576){await new Promise(r=>setTimeout(r,3000));continue}
  const pod=JSON.parse(kube(['get','pod',vm.status.podName,'-o','json']));if(!pod.metadata.ownerReferences.some(v=>v.uid===vm.metadata.uid)||pod.status.phase!=='Running')throw new Error('runner_identity');
  const response=await fetch(`http://${vm.status.podIP}:${vm.spec.runnerPort}/cpu_current`,{signal:AbortSignal.timeout(5000)});if(!response.ok)throw new Error('runner_status');const current=await response.json();if(cpu(current.VCPUs)!==marker.cpu_milli)throw new Error('runner_limit_pending');
  const podName=pod.metadata.name;if(!/^[a-z0-9-]+$/.test(podName))throw new Error('runner_name');const container=pod.spec.containers.find(v=>/runner/.test(v.name))?.name||pod.spec.containers[0].name;
  const command='p=$(find /sys/fs/cgroup -type d -name '+JSON.stringify('neonvm-qemu-'+podName)+' -print -quit); test -n "$p"; cat "$p/cpu.max"; if test -f "$p/memory.current"; then cat "$p/memory.current"; fi';
  const lines=kube(['exec',podName,'-c',container,'--','sh','-ec',command]).trim().split('\n'),[quota,period]=lines[0].trim().split(/\s+/).map(Number);if(!Number.isFinite(quota)||!Number.isFinite(period)||quota/period*1000!==marker.cpu_milli*4)throw new Error('qemu_quota_pending');
  const after=JSON.parse(kube(['get','virtualmachines.vm.neon.tech',name,'-o','json']));if(after.metadata.uid!==vm.metadata.uid||after.status.podName!==podName)throw new Error('runner_changed');
  report.samples.push({stage:marker.stage,at:new Date().toISOString(),endpoint_id:marker.endpoint_id,vm_uid:vm.metadata.uid,pod_uid:pod.metadata.uid,logical_cpu_milli:marker.cpu_milli,observed_memory_mib:marker.memory_mib,runner_cpu_milli:cpu(current.VCPUs),qemu_cpu_quota:quota,qemu_cpu_period:period,qemu_overcommit_factor:4,qemu_memory_current_bytes:Number(lines[1])||null,strict_guest_cpu_quota_verified:false});save();
 }catch(error){const code=error?.message;const safe=['ownership','runner_identity','runner_name','runner_status','runner_limit_pending','qemu_quota_pending','runner_changed'].includes(code)?code:'observation_transport_or_cgroup_unavailable';if(report.errors.at(-1)?.code!==safe){report.errors.push({at:new Date().toISOString(),stage:marker.stage,code:safe});save()}}
 await new Promise(r=>setTimeout(r,3000));
}
report.result='timeout';save();process.exit(1);
