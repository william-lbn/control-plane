import {test} from 'node:test';
import assert from 'node:assert/strict';
import net from 'node:net';
import tls from 'node:tls';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {verifyPostgresTLS} from '../verify-postgres-tls.mjs';

test('real PostgreSQL STARTTLS verifies the CA and DNS identity without login or SQL',async()=>{
  const directory=fs.mkdtempSync(path.join(os.tmpdir(),'neon-tls-ci-'));fs.chmodSync(directory,0o700);
  const caFile=path.join(directory,'ca.pem');const keyFile=path.join(directory,'key.pem');
  let listener;const connections=new Set();const requests=[];
  try{
    const r=spawnSync('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=proxy.example.test','-addext','subjectAltName=DNS:proxy.example.test','-keyout',keyFile,'-out',caFile],{timeout:15000,stdio:'pipe'});
    assert.equal(r.status,0,'OpenSSL is required on Linux for native TLS verification');
    const ca=fs.readFileSync(caFile);const context=tls.createSecureContext({key:fs.readFileSync(keyFile),cert:ca});
    listener=net.createServer(socket=>{
      connections.add(socket);socket.on('error',()=>{});socket.on('close',()=>connections.delete(socket));
      socket.once('data',data=>{
        requests.push(Buffer.from(data));socket.write('S');
        const encrypted=new tls.TLSSocket(socket,{isServer:true,secureContext:context});
        encrypted.on('error',()=>{});encrypted.on('data',()=>assert.fail('TLS diagnostic sent a database login or SQL'));
      });
    });
    await new Promise((resolve,reject)=>{listener.once('error',reject);listener.listen(0,'127.0.0.1',resolve);});
    const report=await verifyPostgresTLS({host:'127.0.0.1',port:listener.address().port,serverName:'proxy.example.test',ca,timeout:10000},true);
    assert.equal(report.result,'pass');assert.equal(report.transport.sql_sent,false);
    assert.equal(report.negative_cases.length,2);assert.equal(report.complete_system_tls,false);
    assert.equal(requests.length,3);
    for(const request of requests){assert.equal(request.length,8);assert.equal(request.readInt32BE(4),80877103);}
  }finally{
    for(const socket of connections)socket.destroy();
    if(listener)await new Promise(resolve=>listener.close(resolve));
    fs.rmSync(directory,{recursive:true,force:true});
  }
});
