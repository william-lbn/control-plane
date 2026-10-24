import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import net from "node:net";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import pg from "pg";
import { createAuth } from "../src/auth.ts";
import type { AuthConfig } from "../src/config.ts";

type Jar = Map<string,string>;
test("phone OTP links only verified possession, hashes challenges, commits failed attempts and permits one concurrent login", async () => {
  const dsn=process.env.AUTH_TEST_DATABASE_URL;
  if(!dsn||new URL(dsn).pathname!='/auth_ci'||new URL(dsn).username!=='auth_ci'||!['127.0.0.1','localhost','[::1]'].includes(new URL(dsn).hostname))throw Error('Dedicated local auth_ci required');
  const schema='ba_phone_'+randomBytes(6).toString('hex'), owner=new pg.Pool({connectionString:dsn,max:1});
  await owner.query('CREATE SCHEMA '+schema);
  const db=new pg.Pool({connectionString:dsn,max:4,options:'-c search_path='+schema+',pg_catalog'});
  await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-schema.sql',import.meta.url),'utf8'));
  await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-phone-schema.sql',import.meta.url),'utf8').replaceAll('neon_auth.',schema+'.'));
  const events:any[]=[];let rejectDelivery=false;
  const sockets=new Set<net.Socket>();
  const receiver=http.createServer(async(request,response)=>{
    const chunks:Buffer[]=[];for await(const chunk of request)chunks.push(chunk);const event=JSON.parse(Buffer.concat(chunks).toString());
    events.push(event);if(rejectDelivery)response.writeHead(503).end();else response.end(JSON.stringify({allowed:true}));
  });
  receiver.on('connection',socket=>{sockets.add(socket);socket.on('error',()=>{});socket.on('close',()=>sockets.delete(socket))});
  await new Promise<void>(resolve=>receiver.listen(0,'127.0.0.1',resolve));const port=(receiver.address() as net.AddressInfo).port;
  const branch='br_0123456789abcdef',config:AuthConfig={version:1,branchID:branch,baseURL:'https://auth.example.test/auth/v1/'+branch,databaseURL:dsn,databaseTLS:{caFile:'/run/auth-ca/ca.crt',serverName:'test'},secret:randomBytes(48).toString('base64url'),trustedOrigins:[],allowLabHTTP:true,phoneNumberEnabled:true,phoneNumberExpiresIn:60,phoneNumberSecret:randomBytes(32).toString('base64url'),webhook:{destination:{url:`http://127.0.0.1:${port}/hook`,addresses:['127.0.0.1'],allowLabHTTP:true},events:['send.otp','user.before_create'],timeoutSeconds:1,signingSeed:randomBytes(32).toString('base64url')}};
  const auth=createAuth(config,db);let ip=1;
  async function call(action:string,body?:unknown,jar:Jar=new Map(),engine=auth){
    const response=await engine.handler(new Request(config.baseURL+'/'+action,{method:body===undefined?'GET':'POST',headers:{Origin:'https://auth.example.test','X-Neon-Client-IP':'192.0.2.'+(ip++),Cookie:[...jar].map(([k,v])=>k+'='+v).join('; '),...(body===undefined?{}:{'Content-Type':'application/json'})},body:body===undefined?undefined:JSON.stringify(body)}));
    for(const header of response.headers.getSetCookie()){const item=header.split(';')[0],at=item.indexOf('='),key=item.slice(0,at),value=item.slice(at+1);if(!value||/max-age=0/i.test(header))jar.delete(key);else jar.set(key,value)}
    return {status:response.status,value:await response.json().catch(()=>null)};
  }
  const code=(phone:string)=>events.filter(e=>e.event_type==='send.otp'&&e.user.phone_number===phone).at(-1).event_data.otp_code as string;
  const phone='+15550000001', unknown='+15550000002';
  try{
    const jar:Jar=new Map(),password=randomBytes(24).toString('base64url');
    let result=await call('sign-up/email',{name:'Owned phone user',email:'phone@example.test',password},jar);assert.equal(result.status,200);const userID=result.value.user.id;
    result=await call('sign-up/email',{name:'Forged phone',email:'forged@example.test',password,phoneNumber:phone,phoneNumberVerified:true});assert([200,400].includes(result.status));
    assert.equal((await db.query('SELECT count(*)::int n FROM "user" WHERE "phoneNumber"=$1',[phone])).rows[0].n,0,'Signup reserved an unverified phone');
    result=await call('update-user',{phoneNumber:phone,phoneNumberVerified:true},jar);assert([200,400].includes(result.status));assert.equal((await db.query('SELECT "phoneNumber" FROM "user" WHERE id=$1',[userID])).rows[0].phoneNumber,null);
    assert.equal((await call('phone-number/send-otp',{phoneNumber:'555 invalid'})).status,400);
    assert.equal((await call('phone-number/send-otp',{phoneNumber:phone})).status,200);const first=code(phone);assert(/^\d{6}$/.test(first));
    const row=(await db.query('SELECT digest,attempts FROM control_phone_challenge WHERE phone=$1',[phone])).rows[0];assert(/^[a-f0-9]{64}$/.test(row.digest));assert(!row.digest.includes(first));
    assert.equal((await db.query('SELECT count(*)::int n FROM verification WHERE identifier=$1',[phone])).rows[0].n,0,'Plain SMS code was committed');
    result=await call('phone-number/verify',{phoneNumber:phone,code:first,updatePhoneNumber:true},jar);assert.equal(result.status,200);assert.equal(result.value.user.id,userID);assert.equal(result.value.user.phoneNumberVerified,true);
    assert.equal((await call('phone-number/verify',{phoneNumber:phone,code:first})).status,400);
    assert.equal((await call('sign-out',{},jar)).status,200);
    assert.equal((await call('phone-number/send-otp',{phoneNumber:phone})).status,200);const challenge=code(phone),wrong=challenge==='000000'?'111111':'000000';
    for(let attempt=1;attempt<=2;attempt++){
      assert.equal((await call('phone-number/verify',{phoneNumber:phone,code:wrong})).status,400);
      assert.equal((await db.query('SELECT attempts FROM control_phone_challenge WHERE phone=$1',[phone])).rows[0].attempts,attempt,'HTTP rejection rolled back the attempt counter');
    }
    assert.equal((await call('phone-number/verify',{phoneNumber:phone,code:wrong})).status,400);assert.equal((await db.query('SELECT count(*)::int n FROM control_phone_challenge WHERE phone=$1',[phone])).rows[0].n,0);
    assert.equal((await call('phone-number/verify',{phoneNumber:phone,code:challenge})).status,400);
    assert.equal((await call('phone-number/send-otp',{phoneNumber:phone})).status,200);const oneTime=code(phone),left:Jar=new Map(),right:Jar=new Map();
    const logins=await Promise.all([call('phone-number/verify',{phoneNumber:phone,code:oneTime},left),call('phone-number/verify',{phoneNumber:phone,code:oneTime},right)]);
    assert.deepEqual(logins.map(x=>x.status).sort(),[200,400]);assert.equal(logins.find(x=>x.status===200)!.value.user.id,userID);
    assert.equal((await call('phone-number/send-otp',{phoneNumber:unknown})).status,200);const usersBefore=(await db.query('SELECT count(*)::int n FROM "user"')).rows[0].n;
    assert.equal((await call('phone-number/verify',{phoneNumber:unknown,code:code(unknown)})).status,400);assert.equal((await db.query('SELECT count(*)::int n FROM "user"')).rows[0].n,usersBefore,'Phone-first signup was enabled');
    assert.equal((await call('phone-number/send-otp',{phoneNumber:phone})).status,200);const expiring=code(phone);await db.query("UPDATE control_phone_challenge SET expires_at=now()-interval '1 second' WHERE phone=$1",[phone]);
    assert.equal((await call('phone-number/verify',{phoneNumber:phone,code:expiring})).status,400);assert.equal((await db.query('SELECT count(*)::int n FROM control_phone_challenge WHERE phone=$1',[phone])).rows[0].n,0);
    assert.equal((await call('phone-number/send-otp',{phoneNumber:phone})).status,200);const retained=(await db.query('SELECT digest,expires_at FROM control_phone_challenge WHERE phone=$1',[phone])).rows[0];const retainedCode=code(phone);
    rejectDelivery=true;result=await call('phone-number/send-otp',{phoneNumber:phone});assert(result.status>=500);assert.deepEqual((await db.query('SELECT digest,expires_at FROM control_phone_challenge WHERE phone=$1',[phone])).rows[0],retained,'Failed delivery replaced the previous usable challenge');
    rejectDelivery=false;assert.equal((await call('phone-number/verify',{phoneNumber:phone,code:retainedCode})).status,200);
    const disabled=createAuth({...config,phoneNumberEnabled:false,phoneNumberExpiresIn:undefined,phoneNumberSecret:undefined},db);assert.equal((await call('phone-number/send-otp',{phoneNumber:phone},new Map(),disabled)).status,404);
    assert.equal((await call('phone-number/request-password-reset',{phoneNumber:phone})).status,404);
    assert.equal((await call('.well-known/jwks.json')).status,200);
  }finally{
    await db.end();await owner.end();for(const socket of sockets)socket.destroy();await new Promise<void>(resolve=>receiver.close(()=>resolve()));
  }
});
