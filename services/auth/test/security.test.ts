import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import pg from 'pg';
import { createAuth } from '../src/auth.ts';
import type { AuthConfig } from '../src/config.ts';

test('application password change and owned session revocation use actual PostgreSQL', async () => {
 const dsn=process.env.AUTH_TEST_DATABASE_URL;
 if(!dsn)throw new Error('Dedicated local auth_ci database required');
 const input=new URL(dsn);
 if(input.pathname!=='/auth_ci'||input.username!=='auth_ci'||!['127.0.0.1','localhost','[::1]'].includes(input.hostname)||input.searchParams.has('options'))throw new Error('Disposable local auth_ci only');
 const schema='auth_security_'+randomBytes(8).toString('hex');
 const admin=new pg.Pool({connectionString:dsn,max:1});
 const db=new pg.Pool({connectionString:dsn,options:`-c search_path=${schema},pg_catalog`,max:2});
 const branch='br_0123456789abcdef';
 const config:AuthConfig={version:1,branchID:branch,baseURL:`https://auth.example.test/auth/v1/${branch}`,secret:randomBytes(48).toString('base64url'),trustedOrigins:[],allowLabHTTP:false,databaseURL:'postgresql://synthetic:synthetic@proxy.example.test/postgres?sslmode=verify-full&options=endpoint%3Dep-0123456789abcdef',databaseTLS:{caFile:'/run/auth-ca/ca.crt',serverName:'proxy.example.test'}};
 try {
  const ddl=readFileSync(new URL('../../../api/internal/control/assets/auth-schema.sql',import.meta.url),'utf8');
  await admin.query(`CREATE SCHEMA ${schema}; SET search_path=${schema},pg_catalog; ${ddl}`);
  const auth=createAuth(config,db);
  const call=(action:string,body?:unknown,cookie?:string)=>auth.handler(new Request(config.baseURL+'/'+action,{method:body?'POST':'GET',headers:{Origin:new URL(config.baseURL).origin,...(body?{'Content-Type':'application/json'}:{}),...(cookie?{Cookie:cookie}:{})},body:body?JSON.stringify(body):undefined}));
  const cookie=(r:Response)=>r.headers.getSetCookie().map(v=>v.split(';')[0]).join('; ');
  const password=randomBytes(24).toString('base64url'),newPassword=randomBytes(24).toString('base64url');
  const identity={email:'security@example.test',name:'Security user',password};
  const first=await call('sign-up/email',identity);assert.equal(first.status,200);const firstCookie=cookie(first);
  const other=await call('sign-in/email',{email:identity.email,password});assert.equal(other.status,200);const otherCookie=cookie(other);
  const listed=await call('list-sessions',undefined,firstCookie);assert.equal(listed.status,200);assert.equal((await listed.json()).length,2);
  const wrong=await call('change-password',{currentPassword:'incorrect-current',newPassword,revokeOtherSessions:true},firstCookie);assert.ok(wrong.status>=400);assert.equal((await (await call('get-session',undefined,otherCookie)).json())!==null,true);
  const changed=await call('change-password',{currentPassword:password,newPassword,revokeOtherSessions:true},firstCookie);assert.equal(changed.status,200);
  assert.equal((await (await call('get-session',undefined,otherCookie)).json())===null,true);assert.equal((await call('token',undefined,otherCookie)).status,401);
  assert.equal((await call('sign-in/email',{email:identity.email,password})).status,401);
  const next=await call('sign-in/email',{email:identity.email,password:newPassword});assert.equal(next.status,200);const nextCookie=cookie(next);
  // Honor Better Auth's stronger sign-in endpoint window (3 / 10 seconds).
  // Keep production rate limits enabled; never replay an unknown login outcome.
  await new Promise(resolve=>setTimeout(resolve,11000));
  const nextOther=await call('sign-in/email',{email:identity.email,password:newPassword});assert.equal(nextOther.status,200);const nextOtherCookie=cookie(nextOther);
  const otherSession=(await (await call('get-session',undefined,nextOtherCookie)).json()).session;
  const revoked=await call('revoke-session',{token:otherSession.token},nextCookie);assert.equal(revoked.status,200);
  assert.equal((await (await call('get-session',undefined,nextOtherCookie)).json())===null,true);
  assert.equal((await (await call('get-session',undefined,nextCookie)).json())!==null,true);
  const again=await call('sign-in/email',{email:identity.email,password:newPassword});assert.equal(again.status,200);const againCookie=cookie(again);
  assert.equal((await call('revoke-other-sessions',{},nextCookie)).status,200);
  assert.equal((await (await call('get-session',undefined,againCookie)).json())===null,true);
  assert.equal((await (await call('get-session',undefined,nextCookie)).json())!==null,true);
  assert.equal((await call('revoke-sessions',{},nextCookie)).status,200);
  assert.equal((await (await call('get-session',undefined,nextCookie)).json())===null,true);
 } finally { await db.end();await admin.query(`DROP SCHEMA IF EXISTS ${schema} CASCADE`);await admin.end(); }
});
