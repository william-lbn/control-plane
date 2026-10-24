import {test} from 'node:test';
import assert from 'node:assert/strict';
import {randomBytes} from 'node:crypto';
import {readFileSync} from 'node:fs';
import pg from 'pg';
import {createServer} from 'node:net';
import {createAuth} from '../src/auth.ts';
import {parseConfig,type AuthConfig} from '../src/config.ts';

type Jar=Map<string,string>;
type QueryGate=(sql:string)=>Promise<void>;
async function fixture(userLimit=2,memberLimit=3,gate?:QueryGate){
 const dsn=process.env.AUTH_TEST_DATABASE_URL;
 if(!dsn||new URL(dsn).pathname!='/auth_ci'||new URL(dsn).username!=='auth_ci'||!['127.0.0.1','localhost','[::1]'].includes(new URL(dsn).hostname))throw Error('Dedicated local auth_ci required');
 const schema='ba_org_'+randomBytes(6).toString('hex'),owner=new pg.Pool({connectionString:dsn,max:1});await owner.query('CREATE SCHEMA '+schema);
 const db=new pg.Pool({connectionString:dsn,max:4,options:'-c search_path='+schema+',pg_catalog'});
 await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-schema.sql',import.meta.url),'utf8'));
 await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-organization-schema.sql',import.meta.url),'utf8').replaceAll('neon_auth.',schema+'.'));
 const branch='br_0123456789abcdef',c:AuthConfig={version:1,branchID:branch,baseURL:'https://auth.example.test/auth/v1/'+branch,databaseURL:dsn,databaseTLS:{caFile:'/run/auth-ca/ca.crt',serverName:'test'},secret:randomBytes(48).toString('base64url'),trustedOrigins:[],allowLabHTTP:false,adminEnabled:true,organizationEnabled:true,organizationLimit:userLimit,organizationMembershipLimit:memberLimit};
 await db.query('INSERT INTO control_organization_policy(singleton,enabled,user_memberships,organization_members,branch_id,generation) VALUES(true,true,$1,$2,$3,1)',[userLimit,memberLimit,branch]);
 const adapter=gate?new Proxy(db,{get(target,key){
  if(key!=='connect'){const value=Reflect.get(target,key,target);return typeof value==='function'?value.bind(target):value}
  return async()=>{const client=await target.connect();return new Proxy(client,{get(connection,property){
   if(property!=='query'){const value=Reflect.get(connection,property,connection);return typeof value==='function'?value.bind(connection):value}
   return async(...args:any[])=>{await gate(typeof args[0]==='string'?args[0]:args[0].text);return (connection.query as any)(...args)};
  }})};
 }}):db;
 const auth=createAuth(c,adapter);let ip=1;
 async function call(action:string,body?:unknown,jar:Jar=new Map(),engine=auth,fixedIP?:string){
  const response=await engine.handler(new Request(c.baseURL+'/'+action,{method:body===undefined?'GET':'POST',headers:{Origin:'https://auth.example.test','X-Neon-Client-IP':fixedIP||'192.0.2.'+(ip++),Cookie:[...jar].map(([key,value])=>key+'='+value).join('; '),...(body===undefined?{}:{'Content-Type':'application/json'})},body:body===undefined?undefined:JSON.stringify(body)}));
  for(const item of response.headers.getSetCookie()){const first=item.split(';')[0],at=first.indexOf('='),key=first.slice(0,at),value=first.slice(at+1);if(!value||/max-age=0/i.test(item))jar.delete(key);else jar.set(key,value)}
  return {status:response.status,value:await response.json().catch(()=>null)};
 }
 async function user(name:string,verified=true){const jar:Jar=new Map(),email=name+'@example.test',password=randomBytes(24).toString('base64url');const value=await call('sign-up/email',{name,email,password},jar);assert.equal(value.status,200);const id=value.value.user.id as string;if(verified)await db.query('UPDATE "user" SET "emailVerified"=true WHERE id=$1',[id]);return {id,email,password,jar}}
 return {db,owner,schema,c,auth,call,user,async close(){await db.end();await owner.end()}};
}
const claims=(token:string)=>JSON.parse(Buffer.from(token.split('.')[1],'base64url').toString());

test('organizations enforce current membership, verified invitations, scoped roles and owner-safe user deletion',async()=>{
 const f=await fixture();const {db,c,call,user}=f;
 try{
  const owner=await user('owner'),member=await user('member'),other=await user('other',false),support=await user('support'),protectedUser=await user('protected');
  await db.query('UPDATE "user" SET role=$1 WHERE id=$2',['admin',support.id]);
  let r=await call('organization/create',{name:'Alpha',slug:'alpha'},owner.jar);assert.equal(r.status,200);const alpha=r.value.id;
  r=await call('organization/create',{name:'Beta',slug:'beta'},owner.jar);assert.equal(r.status,200);const beta=r.value.id;
  r=await call('organization/create',{name:'Over quota',slug:'over'},owner.jar);assert.equal(r.status,403);
  assert.equal((await call('organization/get-full-organization?organizationId='+alpha,undefined,support.jar)).status,403,'Global application admin must not become an organization member');
  assert.equal((await call('organization/get-organization?organizationId='+beta,undefined,member.jar)).status,403);
  assert.equal((await call('organization/check-slug',{slug:'alpha'},owner.jar)).status,400);
  assert.equal((await call('organization/check-slug',{slug:'available'},owner.jar)).status,200);
  assert.equal((await call('organization/update',{organizationId:alpha,data:{name:'Updated Alpha'}},owner.jar)).status,200);
  assert.equal((await call('organization/list',undefined,owner.jar)).value.length,2);
  r=await call('organization/invite-member',{organizationId:alpha,email:other.email,role:'member'},owner.jar);assert.equal(r.status,200);const unverified=r.value.id;
  assert.equal((await call('organization/get-invitation?id='+unverified,undefined,other.jar)).status,403);
  assert.equal((await call('organization/accept-invitation',{invitationId:unverified},other.jar)).status,403);
  assert.equal((await call('organization/accept-invitation',{invitationId:unverified},member.jar)).status,403);
  assert.equal((await db.query('SELECT status FROM invitation WHERE id=$1',[unverified])).rows[0].status,'pending');
  await db.query('UPDATE "user" SET "emailVerified"=true WHERE id=$1',[other.id]);
  assert.equal((await call('organization/get-invitation?id='+unverified,undefined,other.jar)).status,200);
  assert.equal((await call('organization/reject-invitation',{invitationId:unverified},other.jar)).status,200);
  r=await call('organization/invite-member',{organizationId:alpha,email:member.email,role:'member'},owner.jar);assert.equal(r.status,200);const invitation=r.value.id;
  r=await call('organization/accept-invitation',{invitationId:invitation},member.jar);assert.equal(r.status,200);const memberID=r.value.member.id;
  assert.equal((await call('organization/accept-invitation',{invitationId:invitation},member.jar)).status,400);
  assert.equal((await call('organization/get-active-member-role',undefined,member.jar)).value.role,'member');
  const firstToken=await call('token',undefined,member.jar);assert.equal(firstToken.status,200);assert.equal(claims(firstToken.value.token).organization_id,alpha);assert.equal(claims(firstToken.value.token).organization_role,'member');
  assert.equal((await call('organization/update',{organizationId:alpha,data:{name:'Forbidden'}},member.jar)).status,403);
  r=await call('organization/create',{name:'Member own workspace',slug:'gamma'},member.jar);assert.equal(r.status,200);const gamma=r.value.id;
  r=await call('organization/has-permission',{organizationId:alpha,permissions:{organization:['update']}},member.jar);assert.equal(r.status,200);assert.equal(r.value.success,false,'Owning another organization must not elevate this membership');
  assert.equal((await call('organization/update',{organizationId:gamma,data:{name:'Own workspace'}},member.jar)).status,200);
  r=await call('organization/invite-member',{organizationId:alpha,email:'cancel@example.test',role:'member'},owner.jar);assert.equal(r.status,200);assert.equal((await call('organization/cancel-invitation',{invitationId:r.value.id},owner.jar)).status,200);
  r=await call('organization/invite-member',{organizationId:alpha,email:other.email,role:'member'},owner.jar);assert.equal(r.status,200);const expired=r.value.id;await db.query('UPDATE invitation SET "expiresAt"=now()-interval \'1 second\' WHERE id=$1',[expired]);assert.equal((await call('organization/accept-invitation',{invitationId:expired},other.jar)).status,400);
  assert.equal((await call('organization/list-invitations?organizationId='+alpha,undefined,owner.jar)).status,200);
  r=await call('organization/list-user-invitations?email='+encodeURIComponent(owner.email),undefined,other.jar);assert(r.status===400||r.status===403||(r.status===200&&r.value.every((v:any)=>v.email===other.email)),'Server-only email selector leaked another recipient invitations');
  assert.equal((await call('organization/list-members?organizationId='+alpha,undefined,member.jar)).status,200);
  assert.equal((await call('organization/update-member-role',{organizationId:alpha,memberId:memberID,role:'admin'},owner.jar)).status,200);
  assert.equal((await call('organization/delete',{organizationId:alpha},member.jar)).status,403,'Organization admin cannot delete an organization');
  assert.equal((await call('organization/update',{organizationId:alpha,data:{name:'Admin updated'}},member.jar)).status,200);
  assert.equal((await call('organization/set-active',{organizationId:alpha},member.jar)).status,200);
  assert.equal((await call('organization/get-active-member',undefined,member.jar)).value.role,'admin');
  assert.equal((await call('organization/remove-member',{organizationId:alpha,memberIdOrEmail:memberID},owner.jar)).status,200);
  const removedToken=await call('token',undefined,member.jar);assert.equal(removedToken.status,200);assert.equal(claims(removedToken.value.token).organization_id,undefined,'Removed membership remained in a newly issued token');
  const full=await call('organization/get-full-organization?organizationId='+alpha,undefined,owner.jar);assert.equal(full.status,200);const ownerMember=full.value.members.find((v:any)=>v.userId===owner.id);
  assert((await call('organization/remove-member',{organizationId:alpha,memberIdOrEmail:ownerMember.id},owner.jar)).status>=400);assert((await call('organization/leave',{organizationId:alpha},owner.jar)).status>=400);
  r=await call('organization/create',{name:'Protected owner',slug:'protected'},protectedUser.jar);assert.equal(r.status,200);const protectedOrg=r.value.id;
  r=await call('admin/remove-user',{userId:protectedUser.id},support.jar);assert.equal(r.status,409,'Removing a sole owner must fail atomically');
  assert.equal((await db.query('SELECT count(*)::int n FROM "user" WHERE id=$1',[protectedUser.id])).rows[0].n,1);assert.equal((await db.query('SELECT count(*)::int n FROM account WHERE "userId"=$1',[protectedUser.id])).rows[0].n,1);
  assert.equal((await call('organization/delete',{organizationId:protectedOrg},protectedUser.jar)).status,200);assert.equal((await call('admin/remove-user',{userId:protectedUser.id},support.jar)).status,200);
  const lesser=createAuth({...c,organizationCreatorRole:'admin'},db);r=await call('organization/create',{name:'Admin creator',slug:'lesser'},other.jar,lesser);assert.equal(r.status,200);const lesserID=r.value.id;assert.equal((await call('organization/get-active-member',undefined,other.jar,lesser)).value.role,'admin');assert.equal((await call('organization/delete',{organizationId:lesserID},other.jar,lesser)).status,403);
  assert.equal((await call('admin/remove-user',{userId:other.id},support.jar)).status,409,'Ownerless creator-admin organization lost its last manager');
  r=await call('organization/invite-member',{organizationId:lesserID,email:member.email,role:'member'},other.jar,lesser);assert.equal(r.status,200);r=await call('organization/accept-invitation',{invitationId:r.value.id},member.jar);assert.equal(r.status,200);assert.equal((await call('organization/update-member-role',{organizationId:lesserID,memberId:r.value.member.id,role:'admin'},other.jar,lesser)).status,200);assert.equal((await call('organization/leave',{organizationId:lesserID},other.jar,lesser)).status,200);
  assert.equal((await call('organization/add-member',{organizationId:alpha,userId:support.id,role:'owner'},owner.jar)).status,404,'Server-only membership bypass exposed');
  assert.equal((await call('organization/create-team',{organizationId:alpha,name:'Not enabled'},owner.jar)).status,404);
  const disabled=createAuth({...c,organizationEnabled:false},db);assert.equal((await call('organization/list',undefined,owner.jar,disabled)).status,404);
  assert.equal((await call('organization/delete',{organizationId:beta},owner.jar)).status,200);assert.equal((await call('organization/delete',{organizationId:alpha},owner.jar)).status,200);
 }finally{await f.close()}
});

test('organization native transactions serialize quota admissions and retain one owner without consuming rejected invitations',async()=>{
 let pattern:RegExp|undefined,arrivals=0,release:()=>void=()=>{},wait:Promise<void>=Promise.resolve(),timer:ReturnType<typeof setTimeout>|undefined;
 function arm(value:RegExp){pattern=value;arrivals=0;wait=new Promise<void>((resolve,reject)=>{release=resolve;timer=setTimeout(()=>reject(new Error('Both controlled admissions must reach the SQL gate')),15000)})}
 const gate:QueryGate=async sql=>{if(!pattern?.test(sql.trim()))return;arrivals++;if(arrivals===2){pattern=undefined;if(timer)clearTimeout(timer);release()}await wait};
 const f=await fixture(1,2,gate),{db,auth,call,user}=f;
 try{
  const owner=await user('race-owner'),left=await user('race-left'),right=await user('race-right');
  arm(/^insert into (?:"[^"]+"\.)?"organization"/i);
  const creates=await Promise.all(['left','right'].map(name=>call('organization/create',{name,slug:name},owner.jar)));
  assert.deepEqual(creates.map(v=>v.status).sort(),[200,403]);assert.equal(arrivals,2);
  assert.equal((await db.query('SELECT count(*)::int n FROM organization')).rows[0].n,1,'Rejected create left an orphan organization');
  assert.equal((await db.query('SELECT count(*)::int n FROM member')).rows[0].n,1);
  const org=creates.find(v=>v.status===200)!.value.id;
  const inviteLeft=await call('organization/invite-member',{organizationId:org,email:left.email,role:'member'},owner.jar),inviteRight=await call('organization/invite-member',{organizationId:org,email:right.email,role:'member'},owner.jar);assert.equal(inviteLeft.status,200);assert.equal(inviteRight.status,200);
  arm(/^insert into (?:"[^"]+"\.)?"member"/i);
  const joins=await Promise.all([call('organization/accept-invitation',{invitationId:inviteLeft.value.id},left.jar),call('organization/accept-invitation',{invitationId:inviteRight.value.id},right.jar)]);
  assert.deepEqual(joins.map(v=>v.status).sort(),[200,403]);assert.equal(arrivals,2);
  assert.equal((await db.query('SELECT count(*)::int n FROM member WHERE "organizationId"=$1',[org])).rows[0].n,2);
  assert.deepEqual((await db.query('SELECT status FROM invitation ORDER BY status')).rows.map(v=>v.status),['accepted','pending'],'Rejected admission consumed the invitation');
  const winner=joins[0].status===200?left:right,winnerMember=joins.find(v=>v.status===200)!.value.member;
  assert.equal((await call('organization/update-member-role',{organizationId:org,memberId:winnerMember.id,role:'owner'},owner.jar)).status,200);
  const ownerMember=(await db.query('SELECT id FROM member WHERE "organizationId"=$1 AND "userId"=$2',[org,owner.id])).rows[0].id;
  arm(/^update (?:"[^"]+"\.)?"member"/i);
  const demotions=await Promise.all([call('organization/update-member-role',{organizationId:org,memberId:ownerMember,role:'member'},owner.jar),call('organization/update-member-role',{organizationId:org,memberId:winnerMember.id,role:'member'},winner.jar)]);
  assert.deepEqual(demotions.map(v=>v.status).sort(),[200,409]);assert.equal(arrivals,2);
  assert.equal((await db.query('SELECT count(*)::int n FROM member WHERE "organizationId"=$1 AND role=$2',[org,'owner'])).rows[0].n,1,'Concurrent demotion removed the last owner');
  const before=Number((await db.query('SELECT coalesce(sum(count),0) n FROM "rateLimit"')).rows[0].n);
  const rejected=await call('organization/update',{organizationId:org,data:{name:'Forbidden'}},joins[0].status===200?right.jar:left.jar,auth,'192.0.2.240');assert([400,403].includes(rejected.status));
  assert.equal(Number((await db.query('SELECT coalesce(sum(count),0) n FROM "rateLimit"')).rows[0].n),before+1,'Rejected request rolled back its rate-limit attempt');
 }finally{if(timer)clearTimeout(timer);await f.close()}
});

test('organization invitation mail uses a fixed trusted application URL and rejects failed delivery atomically',async()=>{
 const messages:string[]=[];let rejectData=false;
 const server=createServer(socket=>{
  let buffer='',data=false,lines:string[]=[];socket.setTimeout(10000,()=>socket.destroy());socket.write('220 isolated SMTP fixture\r\n');
  socket.on('data',chunk=>{buffer+=chunk.toString();let at:number;while((at=buffer.indexOf('\r\n'))>=0){const line=buffer.slice(0,at);buffer=buffer.slice(at+2);
   if(data){if(line==='.') {data=false;if(rejectData)socket.write('451 controlled delivery rejected\r\n');else{messages.push(lines.join('\r\n'));socket.write('250 accepted\r\n')}lines=[]}else lines.push(line.startsWith('..')?line.slice(1):line);continue}
   if(/^(EHLO|HELO)/i.test(line))socket.write('250-localhost\r\n250 PIPELINING\r\n');
   else if(/^DATA$/i.test(line)){data=true;lines=[];socket.write('354 continue\r\n')}
   else if(/^QUIT$/i.test(line))socket.end('221 bye\r\n');else socket.write('250 ok\r\n');
  }})
 });await new Promise<void>(resolve=>server.listen(0,'127.0.0.1',resolve));const address=server.address();if(!address||typeof address==='string')throw Error('Local SMTP address required');
 const f=await fixture(),{db,c,call,user}=f;
 try{
  const owner=await user('mail-owner'),recipient=await user('mail-recipient');
  const settings:AuthConfig={...c,requireEmailVerification:true,organizationInvitationEmail:true,organizationInvitationURL:'https://app.example.test/auth/accept-invitation',trustedOrigins:['https://app.example.test'],mail:{version:1,host:'127.0.0.1',port:address.port,from:'auth@example.test',secure:false,labCleartext:true}};
  const guarded={...settings,databaseURL:'postgresql://synthetic:synthetic@proxy.example.test/postgres?sslmode=verify-full&options=endpoint%3Dep-0123456789abcdef',databaseTLS:{caFile:'/run/auth-ca/ca.crt' as const,serverName:'proxy.example.test'}};
  assert.equal(parseConfig(guarded).organizationEnabled,true);
  for(const change of [{organizationInvitationURL:'https://untrusted.example.test/invite'},{organizationInvitationURL:'https://app.example.test/invite?to=elsewhere'},{organizationInvitationURL:'https://user:secret@app.example.test/invite'},{requireEmailVerification:false},{organizationLimit:101},{organizationMembershipLimit:0},{organizationCreatorRole:'owner,admin'},{organizationEnabled:false}])assert.throws(()=>parseConfig({...guarded,...change}));
  assert.throws(()=>createAuth({...settings,mail:undefined},db));
  const auth=createAuth(settings,db);let r=await call('organization/create',{name:'Mail workspace',slug:'mail-workspace'},owner.jar,auth);assert.equal(r.status,200);const org=r.value.id;
  r=await call('organization/invite-member',{organizationId:org,email:recipient.email,role:'member'},owner.jar,auth);assert.equal(r.status,200);assert.equal(messages.length,1);
  const raw=messages[0];assert(/X-Neon-Auth-Event: organization-invitation/i.test(raw));assert(raw.includes(c.branchID));
  const decoded=raw.replace(/=\r?\n/g,'').replace(/=([0-9a-f]{2})/gi,(_,hex:string)=>String.fromCharCode(parseInt(hex,16)));const match=decoded.match(/https:\/\/app\.example\.test\/auth\/accept-invitation\?[^\s]+/);assert(match,'Fixed invitation URL missing');const link=new URL(match[0]);assert.equal(link.searchParams.get('invitationId'),r.value.id);assert.equal(link.searchParams.get('branchId'),c.branchID);assert.equal(link.origin,'https://app.example.test');
  rejectData=true;r=await call('organization/invite-member',{organizationId:org,email:'rejected@example.test',role:'member'},owner.jar,auth);assert(r.status>=500);assert.equal((await db.query('SELECT count(*)::int n FROM invitation WHERE email=$1',['rejected@example.test'])).rows[0].n,0,'Failed mail committed an unusable invitation');
 }finally{await f.close();await new Promise<void>((resolve,reject)=>server.close(error=>error?reject(error):resolve()))}
});
