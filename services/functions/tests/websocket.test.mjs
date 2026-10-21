import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { once } from 'node:events';
import test from 'node:test';
import WebSocket from 'ws';
import { startRuntime } from '../runtime/server.mjs';
import { upgradeWebSocket } from '../runtime/websocket.mjs';

async function fixture(t) {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'function-ws-'));
  const sdk = pathToFileURL(path.resolve('runtime/websocket.mjs')).href;
  const entry = path.join(directory, 'index.mjs');
  await fs.writeFile(entry, `import {upgradeWebSocket} from '${sdk}';export default req=>{
    const path=new URL(req.url).pathname;
    if(path.endsWith('/deny'))return new Response('private',{status:401});
    const {socket,response}=upgradeWebSocket(req,{protocol:'echo.v1'});
    if(path.endsWith('/twice'))upgradeWebSocket(req);
    if(path.endsWith('/clone'))return response.clone();
    socket.onopen=()=>socket.send('opened');
    socket.onmessage=e=>{if(e.data==='server-close')socket.close(4001,'done');else socket.send(e.data);};
    return response;
  };`);
  const runtime = await startRuntime({ entry, port: 0 });
  const sockets = [];
  t.after(async () => { for (const socket of sockets) socket.terminate(); await runtime.close(); await fs.rm(directory, { recursive: true }); });
  const connect = (suffix = '') => { const socket = new WebSocket(`ws://127.0.0.1:${runtime.server.address().port}/`, ['echo.v1'], { headers: { 'x-neon-request-url': 'http://functions.test/functions/v1/br_0000000000000000/echo' + suffix }, perMessageDeflate: false }); sockets.push(socket);return socket; };
  return {runtime,connect};
}

test('WebSocket real upgrade preserves text binary protocol activity and close', async t => {
  const {runtime,connect} = await fixture(t);
  const socket = connect(), welcome = once(socket,'message');
  await once(socket,'open');assert.equal(socket.protocol,'echo.v1');assert.equal((await welcome)[0].toString(),'opened');
  assert.equal(runtime.status().active,1);
  let next=once(socket,'message');socket.send('中文');assert.equal((await next)[0].toString(),'中文');
  next=once(socket,'message');socket.send(Buffer.from([0,255,5]));const [value,binary]=await next;assert.equal(binary,true);assert.deepEqual([...value],[0,255,5]);
  next=once(socket,'message');socket.send(new Blob([new Uint8Array([1,2,3])]));assert.deepEqual([...(await next)[0]],[1,2,3]);
  const closed=once(socket,'close');socket.send('server-close');const [code,reason]=await closed;assert.equal(code,4001);assert.equal(reason.toString(),'done');
  await new Promise(r=>setTimeout(r,20));assert.equal(runtime.status().active,0);
});

test('WebSocket rejects off runtime duplicate upgrade lost response and application denial', async t => {
  assert.throws(()=>upgradeWebSocket(new Request('http://invalid')),TypeError);
  const {connect}=await fixture(t);
  for (const [suffix,status] of [['/deny',401],['/clone',500],['/twice',502]]) {
    const socket=connect(suffix);
    const observed=await new Promise((resolve,reject)=>{socket.once('unexpected-response',(_request,response)=>{response.resume();socket.terminate();resolve(response.statusCode);});socket.on('error',()=>{});setTimeout(()=>reject(new Error('Missing rejection')),2000).unref();});
    assert.equal(observed,status);
  }
});

test('WebSocket oversized message closes 1009 and runtime drain closes original connection', async t => {
  const {runtime,connect}=await fixture(t);
  const first=connect();await once(first,'open');const closed=once(first,'close');first.send(Buffer.alloc(1024*1024+1));assert.equal((await closed)[0],1009);
  const second=connect();await once(second,'open');const stopped=once(second,'close');await runtime.close();assert.equal((await stopped)[0],1006);
});
