import {test} from 'node:test';
import assert from 'node:assert/strict';
import {randomBytes} from 'node:crypto';
import {readFileSync} from 'node:fs';
import pg from 'pg';
import {createAuth} from '../src/auth.ts';
import {parseConfig,type AuthConfig} from '../src/config.ts';
import {adminActions} from '../src/admin.ts';

test('application admin uses maintained PostgreSQL roles bans sessions and bounded impersonation',async()=>{
 const dsn=process.env.AUTH_TEST_DATABASE_URL;
 if(!dsn||new URL(dsn).pathname!='/auth_ci'||new URL(dsn).username!=='auth_ci'||!['localhost','127.0.0.1','[::1]'].includes(new URL(dsn).hostname))throw new Error('Dedicated local auth_ci required');
 const schema='ba_admin_'+randomBytes(6).toString('hex'),owner=new pg.Pool({connectionString:dsn,max:1});await owner.query('CREATE SCHEMA '+schema);
 const db=new pg.Pool({connectionString:dsn,max:2,options:'-c search_path='+schema+',pg_catalog'});
 const branch='br_0123456789abcdef',c:AuthConfig={version:1,branchID:branch,baseURL:'https://auth.example.test/auth/v1/'+branch,secret:randomBytes(48).toString('base64url'),databaseURL:dsn,databaseTLS:{caFile:'/run/auth-ca/ca.crt',serverName:'test'},trustedOrigins:[],allowLabHTTP:false,adminEnabled:true};
 type Jar=Map<string,string>;const privileged:Jar=new Map(),normal:Jar=new Map();let ip=1;
 let auth:ReturnType<typeof createAuth>;
 async function call(action:string,body?:unknown,jar:Jar=privileged,engine?:ReturnType<typeof createAuth>){
  const response=await (engine||auth).handler(new Request(c.baseURL+'/'+action,{method:body===undefined?'GET':'POST',headers:{Origin:'https://auth.example.test','X-Neon-Client-IP':'192.0.2.'+(ip++),...(body===undefined?{}:{'Content-Type':'application/json'}),Cookie:[...jar].map(([k,v])=>k+'='+v).join('; ')},body:body===undefined?undefined:JSON.stringify(body)}));
  for(const item of response.headers.getSetCookie()){const first=item.split(';')[0],at=first.indexOf('='),key=first.slice(0,at),value=first.slice(at+1);if(!value||/max-age=0/i.test(item))jar.delete(key);else jar.set(key,value)}
  const value=await response.json().catch(()=>null);return {status:response.status,value};
 }
 const adminPassword=randomBytes(24).toString('base64url'),userPassword=randomBytes(24).toString('base64url'),changedPassword=randomBytes(24).toString('base64url');
 try{
  await db.query(readFileSync(new URL('../../../api/internal/control/assets/auth-schema.sql',import.meta.url),'utf8'));auth=createAuth(c,db);
  assert.throws(()=>parseConfig({...c,adminEnabled:'true'}));assert.equal(Object.keys(adminActions).length,15);
  let result=await call('sign-up/email',{email:'owner@example.test',name:'Owner',password:adminPassword});assert.equal(result.status,200);const adminID=result.value.user.id;
  result=await call('sign-up/email',{email:'member@example.test',name:'Member',password:userPassword,role:'admin',banned:false},normal);assert.equal(result.status,400,'Signup must reject protected role/ban fields');
  result=await call('sign-up/email',{email:'member@example.test',name:'Member',password:userPassword},normal);assert.equal(result.status,200);const userID=result.value.user.id;
  assert.equal((await db.query('SELECT role FROM "user" WHERE id=$1',[userID])).rows[0].role,'user','Signup must not grant a requested admin role');
  result=await call('admin/list-users',undefined,normal);assert.equal(result.status,403);
  result=await call('update-user',{role:'admin'},normal);assert.notEqual((await db.query('SELECT role FROM "user" WHERE id=$1',[userID])).rows[0].role,'admin');
  await db.query('UPDATE "user" SET role=$1 WHERE id=$2',['admin',adminID]);
  result=await call('admin/list-users?limit=10');assert.equal(result.status,200);assert.equal(result.value.users.length,2);
  result=await call('admin/get-user?id='+encodeURIComponent(userID));assert.equal(result.status,200);
  result=await call('admin/has-permission',{permissions:{user:['set-role']}});assert.equal(result.status,200);assert.equal(result.value.success,true);
  result=await call('admin/create-user',{email:'managed@example.test',name:'Managed',password:changedPassword,role:'user'});assert.equal(result.status,200);const managedID=result.value.user.id;
  result=await call('admin/update-user',{userId:managedID,data:{name:'Updated managed'}});assert.equal(result.status,200);
  result=await call('admin/set-role',{userId:managedID,role:'admin'});assert.equal(result.status,200);
  result=await call('admin/impersonate-user',{userId:managedID});assert.equal(result.status,403,'An admin must not impersonate another admin');
  result=await call('admin/set-role',{userId:managedID,role:'user'});assert.equal(result.status,200);
  result=await call('admin/list-user-sessions',{userId:userID});assert.equal(result.status,200);assert(result.value.sessions.length>0);const oldToken=result.value.sessions[0].token;
  result=await call('admin/revoke-user-session',{sessionToken:oldToken});assert.equal(result.status,200);assert.equal((await call('get-session',undefined,normal)).value,null);
  result=await call('sign-in/email',{email:'member@example.test',password:userPassword},normal);assert.equal(result.status,200);
  result=await call('admin/revoke-user-sessions',{userId:userID});assert.equal(result.status,200);assert.equal((await call('get-session',undefined,normal)).value,null);
  result=await call('admin/set-user-password',{userId:userID,newPassword:changedPassword});assert.equal(result.status,200);
  result=await call('sign-in/email',{email:'member@example.test',password:userPassword},normal);assert.equal(result.status,401);
  result=await call('sign-in/email',{email:'member@example.test',password:changedPassword},normal);assert.equal(result.status,200);
  result=await call('admin/impersonate-user',{userId:userID});assert.equal(result.status,200);assert.equal(result.value.session.impersonatedBy,adminID);assert(new Date(result.value.session.expiresAt).getTime()<=Date.now()+3602000);
  assert([...privileged.keys()].some(k=>k.endsWith('.admin_session')));
  result=await call('get-session');assert.equal(result.value.user.id,userID);
  result=await call('admin/list-users');assert.equal(result.status,403,'Impersonation must have only the target privileges');
  result=await call('admin/stop-impersonating',{});assert.equal(result.status,200);assert.equal(result.value.user.id,adminID);assert(![...privileged.keys()].some(k=>k.endsWith('.admin_session')));
  result=await call('admin/ban-user',{userId:userID,banReason:'Owned functional fixture'});assert.equal(result.status,200);assert.equal((await call('get-session',undefined,normal)).value,null);
  const disabled=createAuth({...c,adminEnabled:false},db);result=await call('admin/list-users',undefined,privileged,disabled);assert.equal(result.status,404);
  result=await call('sign-in/email',{email:'member@example.test',password:changedPassword},normal,disabled);assert.equal(result.status,403,'Disabling administration must preserve persisted bans');
  result=await call('admin/unban-user',{userId:userID});assert.equal(result.status,200);result=await call('sign-in/email',{email:'member@example.test',password:changedPassword},normal);assert.equal(result.status,200);
  result=await call('admin/remove-user',{userId:managedID});assert.equal(result.status,200);assert.equal((await db.query('SELECT count(*)::int AS n FROM "user" WHERE id=$1',[managedID])).rows[0].n,0);
 }finally{await db.end();await owner.end()}
});
