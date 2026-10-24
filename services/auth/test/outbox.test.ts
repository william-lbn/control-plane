import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import net from "node:net";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import pg from "pg";
import { createAuth } from "../src/auth.ts";
import { createAuthWebhooks } from "../src/webhooks.ts";
import { createAuthOutbox } from "../src/outbox.ts";
import type { AuthConfig } from "../src/config.ts";

type Jar=Map<string,string>;
test('native webhook outbox follows committed users, phone verification and accepted organization memberships',async()=>{
 const dsn=process.env.AUTH_TEST_DATABASE_URL;
 if(!dsn||new URL(dsn).pathname!='/auth_ci'||new URL(dsn).username!=='auth_ci'||!['127.0.0.1','localhost','[::1]'].includes(new URL(dsn).hostname))throw Error('Dedicated local auth_ci required');
 const schema='ba_hooks_'+randomBytes(6).toString('hex'),owner=new pg.Pool({connectionString:dsn,max:1});await owner.query('CREATE SCHEMA '+schema);
 const db=new pg.Pool({connectionString:dsn,max:6,options:'-c search_path='+schema+',pg_catalog'});
 for(const asset of ['auth-schema.sql','auth-phone-schema.sql','auth-organization-schema.sql','auth-webhook-schema.sql'])await db.query(readFileSync(new URL('../../../api/internal/control/assets/'+asset,import.meta.url),'utf8').replaceAll('neon_auth',schema));
 const branch='br_0123456789abcdef',project='prj_0123456789abcdef',endpoint='ep_0123456789abcdef';
 await db.query('INSERT INTO control_organization_policy VALUES(true,true,10,2,$1,1)',[branch]);
 const types=['user.created','phone_number.verified','organization.invitation.created','organization.invitation.accepted'];
 await db.query('INSERT INTO control_webhook_policy(singleton,enabled,branch_id,project_id,endpoint_id,generation,events) VALUES(true,true,$1,$2,$3,1,$4)',[branch,project,endpoint,types]);
 const received:any[]=[];let receiverStatus=200,decision:unknown={allowed:true};const sockets=new Set<net.Socket>();
 const receiver=http.createServer(async(req,res)=>{const chunks:Buffer[]=[];for await(const c of req)chunks.push(c);const event=JSON.parse(Buffer.concat(chunks).toString());received.push(event);res.writeHead(event.event_type==='user.before_create'?200:receiverStatus).end(JSON.stringify(event.event_type==='user.before_create'?decision:{}))});
 receiver.on('connection',s=>{sockets.add(s);s.on('error',()=>{});s.on('close',()=>sockets.delete(s))});await new Promise<void>(r=>receiver.listen(0,'127.0.0.1',r));const port=(receiver.address() as net.AddressInfo).port;
 const c:AuthConfig={version:1,branchID:branch,projectID:project,endpointID:endpoint,baseURL:'https://auth.example.test/auth/v1/'+branch,databaseURL:dsn,databaseTLS:{caFile:'/run/auth-ca/ca.crt',serverName:'test'},secret:randomBytes(48).toString('base64url'),trustedOrigins:[],allowLabHTTP:true,adminEnabled:true,organizationEnabled:true,organizationLimit:10,organizationMembershipLimit:2,phoneNumberEnabled:true,phoneNumberExpiresIn:60,phoneNumberSecret:randomBytes(32).toString('base64url'),emailOTPEnabled:true,magicLinkEnabled:true,webhook:{destination:{url:`http://127.0.0.1:${port}/hook`,addresses:['127.0.0.1'],allowLabHTTP:true},events:['send.otp','send.magic_link','user.before_create',...types] as any,timeoutSeconds:1,signingSeed:randomBytes(32).toString('base64url')}};
 const auth=createAuth(c,db),hooks=createAuthWebhooks(c.webhook!,{branch_id:branch,project_id:project,endpoint_id:endpoint}),queue=createAuthOutbox(db,branch,hooks,types);let ip=1;
 async function call(action:string,body?:unknown,jar:Jar=new Map()){
  const res=await auth.handler(new Request(c.baseURL+'/'+action,{method:body===undefined?'GET':'POST',headers:{Origin:'https://auth.example.test','X-Neon-Client-IP':'192.0.2.'+(ip++),Cookie:[...jar].map(([k,v])=>k+'='+v).join('; '),...(body===undefined?{}:{'Content-Type':'application/json'})},body:body===undefined?undefined:JSON.stringify(body)}));
  for(const line of res.headers.getSetCookie()){const first=line.split(';')[0],at=first.indexOf('='),k=first.slice(0,at),v=first.slice(at+1);if(!v||/max-age=0/i.test(line))jar.delete(k);else jar.set(k,v)}return {status:res.status,value:await res.json().catch(()=>null)};
 }
 async function user(name:string){const jar:Jar=new Map(),email=name+'@example.test',r=await call('sign-up/email',{name,email,password:randomBytes(24).toString('base64url')},jar);assert.equal(r.status,200);await db.query('UPDATE "user" SET "emailVerified"=true WHERE id=$1',[r.value.user.id]);return {jar,email,id:r.value.user.id as string}}
 async function settle(){for(let i=0;i<8;i++){await queue.drain(3);if(!(await db.query("SELECT 1 FROM control_webhook_event WHERE state IN ('pending','delivering')")).rowCount)return;await new Promise(r=>setTimeout(r,20))}throw Error('Outbox remained active')}
 try{
  const admin=await user('owner'),member=await user('member'),overflow=await user('overflow');await settle();
  assert.equal((await call('webhooks/events',undefined,admin.jar)).status,403);assert.equal((await call('webhooks/drain',{},new Map([['neon_console','untrusted']]))).status,403);
  await db.query('UPDATE "user" SET role=$1 WHERE id=$2',['admin',admin.id]);
  let r=await call('webhooks/events',undefined,admin.jar);assert.equal(r.status,200);assert.equal(r.value.items.length,3);assert(!JSON.stringify(r.value).includes(admin.email));assert(!JSON.stringify(r.value).includes('payload'));
  const before=(await db.query('SELECT count(*)::int n FROM "user"')).rows[0].n;decision={allowed:false,error_code:'INVITE_REQUIRED',error_message:'Invitation required'};
  r=await call('sign-up/email',{name:'Denied',email:'denied@example.test',password:randomBytes(24).toString('base64url')});assert.equal(r.status,403);assert.equal(r.value.code,'INVITE_REQUIRED');assert.equal((await db.query('SELECT count(*)::int n FROM "user"')).rows[0].n,before);
  decision={allowed:'yes'};r=await call('sign-up/email',{name:'Invalid',email:'invalid@example.test',password:randomBytes(24).toString('base64url')});assert.equal(r.status,503);decision={allowed:true};
  assert.equal((await call('email-otp/send-verification-otp',{email:member.email,type:'sign-in'})).status,200);assert.equal(received.at(-1).event_data.delivery_preference,'email');assert(/^\d{6}$/.test(received.at(-1).event_data.otp_code));
  assert.equal((await call('sign-in/magic-link',{email:member.email,callbackURL:c.baseURL})).status,200);assert.equal(received.at(-1).event_type,'send.magic_link');assert(new URL(received.at(-1).event_data.url).pathname.startsWith(new URL(c.baseURL).pathname+'/'));
  receiverStatus=503;assert.equal((await call('email-otp/send-verification-otp',{email:member.email,type:'sign-in'})).status,503);receiverStatus=200;
  const phone='+15550000009';assert.equal((await call('phone-number/send-otp',{phoneNumber:phone})).status,200);const code=received.filter(e=>e.event_type==='send.otp'&&e.user.phone_number===phone).at(-1).event_data.otp_code;
  assert.equal((await call('phone-number/verify',{phoneNumber:phone,code,updatePhoneNumber:true},member.jar)).status,200);await settle();
  assert.equal(received.filter(e=>e.event_type==='phone_number.verified').length,1);assert.equal(received.find(e=>e.event_type==='phone_number.verified').user.id,member.id);
  r=await call('organization/create',{name:'Events organization',slug:'events'},admin.jar);assert.equal(r.status,200);const org=r.value.id;
  r=await call('organization/invite-member',{organizationId:org,email:member.email,role:'member'},admin.jar);assert.equal(r.status,200);const invitation=r.value.id;
  r=await call('organization/accept-invitation',{invitationId:invitation},member.jar);assert.equal(r.status,200);const membership=r.value.member.id;await settle();
  const accepted=received.find(e=>e.event_type==='organization.invitation.accepted');assert.equal(accepted.event_data.member_id,membership);assert.equal(accepted.user.id,member.id);assert(received.some(e=>e.event_type==='organization.invitation.created'&&e.event_data.invitation_id===invitation));
  r=await call('organization/invite-member',{organizationId:org,email:overflow.email,role:'member'},admin.jar);assert.equal(r.status,200);const denied=r.value.id;
  assert.equal((await call('organization/accept-invitation',{invitationId:denied},overflow.jar)).status,403);assert.equal((await db.query('SELECT status FROM invitation WHERE id=$1',[denied])).rows[0].status,'pending');assert.equal((await db.query("SELECT count(*)::int n FROM control_webhook_event WHERE event_type='organization.invitation.accepted'")).rows[0].n,1);
  await settle();const client=await db.connect();try{
   await client.query('BEGIN');await client.query('INSERT INTO "user"(id,name,email,"emailVerified","createdAt","updatedAt") VALUES($1,$2,$3,false,now(),now())',['rolled-back','Rollback','rollback@example.test']);
   assert.equal((await db.query("SELECT count(*)::int n FROM control_webhook_event WHERE payload->'user'->>'id'='rolled-back'")).rows[0].n,0);await client.query('ROLLBACK');
  }finally{client.release()}
  receiverStatus=503;const fail=await user('receiver-failed');await settle();r=await call('webhooks/events',undefined,admin.jar);assert(r.value.items.some((e:any)=>e.event_type==='user.created'&&e.state==='failed'));assert.equal((await db.query('SELECT count(*)::int n FROM "user" WHERE id=$1',[fail.id])).rows[0].n,1,'Nonblocking receiver failure undid signup');receiverStatus=200;
  await db.query("UPDATE control_webhook_event SET state='pending',delivery_batches=0 WHERE payload->'user'->>'id'=$1 AND event_type='user.created'",[fail.id]);
  const second=createAuthOutbox(db,branch,hooks,types),start=received.length;await Promise.all([queue.drain(3),second.drain(3)]);assert.equal(received.slice(start).filter(e=>e.user.id===fail.id).length,1,'Concurrent dispatch duplicated a claimed event');
  await db.query("UPDATE control_webhook_event SET state='delivering',delivery_batches=3,lease_until=now()-interval '1 second' WHERE payload->'user'->>'id'=$1 AND event_type='user.created'",[fail.id]);await queue.drain();assert.equal((await db.query("SELECT state,last_error FROM control_webhook_event WHERE payload->'user'->>'id'=$1 AND event_type='user.created'",[fail.id])).rows[0].last_error,'delivery_lease_exhausted');
  // A copied or obsolete generation is never sent by this generation's worker.
  await db.query("UPDATE control_webhook_event SET state='pending',generation=2,delivery_batches=0 WHERE payload->'user'->>'id'=$1 AND event_type='user.created'",[fail.id]);assert.deepEqual(await queue.drain(),{delivered:0,failed:0});
  await Promise.all(Array.from({length:5},(_,i)=>user('coalesced-'+i)));
  for(let attempt=0;attempt<100;attempt++){
   const done=(await db.query("SELECT count(*)::int n FROM control_webhook_event WHERE event_type='user.created' AND payload->'user'->>'email' LIKE 'coalesced-%' AND state='delivered'")).rows[0].n;
   if(done===5)break;if(attempt===99)assert.fail('Activity arriving during delivery was not dispatched');await new Promise(r=>setTimeout(r,30));
  }
  await db.query('UPDATE control_webhook_policy SET enabled=false WHERE singleton');const oldCount=(await db.query('SELECT count(*)::int n FROM control_webhook_event')).rows[0].n;await user('disabled-policy');assert.equal((await db.query('SELECT count(*)::int n FROM control_webhook_event')).rows[0].n,oldCount);
 }finally{await db.end();await owner.end();for(const s of sockets)s.destroy();await new Promise<void>(r=>receiver.close(()=>r()))}
});
