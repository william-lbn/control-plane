// PostgreSQL SSLRequest handshake only: never sends a login, password or SQL.
import net from 'node:net';
import tls from 'node:tls';
import fs from 'node:fs';
import {createHash} from 'node:crypto';
import {parseArgs} from 'node:util';
import {pathToFileURL} from 'node:url';

export async function probePostgresTLS({host,port,serverName,ca,timeout=10000}) {
  return new Promise((resolve,reject)=>{
    let secure;let settled=false;
    const socket=net.createConnection({host,port});
    const finish=(error,value)=>{
      if(settled)return;settled=true;clearTimeout(timer);
      secure?.destroy();socket.destroy();
      if(error)reject(error);else resolve(value);
    };
    const timer=setTimeout(()=>finish(Object.assign(new Error('TLS probe deadline'),{code:'PROBE_TIMEOUT'})),timeout);
    socket.once('error',e=>finish(e));
    socket.once('connect',()=>{
      const request=Buffer.alloc(8);request.writeInt32BE(8,0);request.writeInt32BE(80877103,4);socket.write(request);
    });
    socket.once('data',data=>{
      if(data.length!==1||data[0]!==83){finish(Object.assign(new Error('PostgreSQL did not accept TLS'),{code:'PG_TLS_REJECTED'}));return;}
      secure=tls.connect({socket,servername:serverName,ca,rejectUnauthorized:true});
      secure.once('error',e=>finish(e));
      secure.once('secureConnect',()=>{
        if(!secure.authorized){finish(Object.assign(new Error('TLS identity not authorized'),{code:'PG_TLS_UNAUTHORIZED'}));return;}
        const cert=secure.getPeerCertificate();
        finish(null,{result:'pass',protocol:secure.getProtocol(),cipher:secure.getCipher().name,
          certificate_sha256:createHash('sha256').update(cert.raw).digest('hex'),
          certificate_valid_until:cert.valid_to,sql_sent:false,login_sent:false});
      });
    });
  });
}

export async function verifyPostgresTLS(options,negative=false) {
  const report={result:'running',transport:await probePostgresTLS(options),negative_cases:[],
    complete_system_tls:false,scope:'PostgreSQL SSLRequest transport; no database login or SQL'};
  if(negative)for(const [name,change,codes] of [
    ['wrong_certificate_name',{serverName:'untrusted-identity.invalid'},['ERR_TLS_CERT_ALTNAME_INVALID']],
    ['missing_trust_anchors',{ca:[]},['DEPTH_ZERO_SELF_SIGNED_CERT','SELF_SIGNED_CERT_IN_CHAIN','UNABLE_TO_VERIFY_LEAF_SIGNATURE','UNABLE_TO_GET_ISSUER_CERT_LOCALLY']],
  ]) {
    let code;
    try{await probePostgresTLS({...options,...change});}catch(e){code=e.code;}
    if(!codes.includes(code))throw Object.assign(new Error('TLS rejection gate failed'),{code:'TLS_NEGATIVE_GATE_FAILED'});
    report.negative_cases.push({name,result:'rejected',code});
  }
  report.result='pass';return report;
}

if(process.argv[1]&&pathToFileURL(process.argv[1]).href===import.meta.url) {
  try{
    const {values}=parseArgs({options:{host:{type:'string'},port:{type:'string'},'server-name':{type:'string'},'ca-file':{type:'string'},'verify-negative-cases':{type:'boolean',default:false}},strict:true});
    const port=Number(values.port);
    if(!values.host||!values['server-name']||!Number.isInteger(port)||port<1||port>65535||!values['ca-file'])throw new Error('Explicit host, port, server-name and ca-file are required');
    const stat=fs.statSync(values['ca-file']);if(!stat.isFile()||stat.size===0||stat.size>128*1024)throw new Error('Invalid CA file');
    const ca=fs.readFileSync(values['ca-file']);tls.createSecureContext({ca});
    console.log(JSON.stringify(await verifyPostgresTLS({host:values.host,port,serverName:values['server-name'],ca},values['verify-negative-cases']),null,2));
  }catch(e){console.error(JSON.stringify({result:'fail',code:e.code||'PROBE_CONFIGURATION_ERROR',complete_system_tls:false}));process.exitCode=1;}
}
