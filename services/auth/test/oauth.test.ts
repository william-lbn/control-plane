import {test} from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import net from 'node:net';
import {randomBytes,randomUUID,createHash} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {generateKeyPair,exportJWK,SignJWT} from 'jose';
import pg from 'pg';
import {createAuth} from '../src/auth.ts';
import type {AuthConfig} from '../src/config.ts';
import type {AuthOAuthProvider} from '../src/oauth.ts';

type Jar=Map<string,string>;
test('OAuth uses native state and PKCE, verified subjects, explicit linking and branch-isolated cookies without retaining provider tokens',async()=>{
 const dsn=process.env.AUTH_TEST_DATABASE_URL;
 if(!dsn||new URL(dsn).pathname!='/auth_ci'||new URL(dsn).username!=='auth_ci'||!['127.0.0.1','localhost','[::1]'].includes(new URL(dsn).hostname))throw Error('Dedicated local auth_ci required');
 const schema='ba_oauth_'+randomBytes(6).toString('hex'),owner=new pg.Pool({connectionString:dsn,max:1});await owner.query('CREATE SCHEMA '+schema);
 const db=new pg.Pool({connectionString:dsn,max:4,options:'-c search_path='+schema+',pg_catalog'});
 for(const asset of ['auth-schema.sql','auth-oauth-schema.sql'])await db.query(readFileSync(new URL('../../../api/internal/control/assets/'+asset,import.meta.url),'utf8').replaceAll('neon_auth',schema));
 const key=await generateKeyPair('EdDSA'),jwk={...await exportJWK(key.publicKey),kid:'fixture-key',alg:'EdDSA',use:'sig'},forged=await generateKeyPair('EdDSA');
 const codes=new Map<string,{id:string;challenge:string;nonce:string;redirect:string;mode:string;email:string;sub:string}>(),access=new Map<string,{email:string;sub:string;verified:boolean}>();let mode='valid',email='social@example.test',sub='social-subject',exchanges=0;
 const sockets=new Set<net.Socket>();let origin='';
 const provider=http.createServer(async(req,res)=>{
  const url=new URL(req.url!,'http://fixture');const [kind,id]=url.pathname.slice(1).split('/');
  if(kind==='authorize'){
   const code=randomUUID();codes.set(code,{id,challenge:url.searchParams.get('code_challenge')!,nonce:url.searchParams.get('nonce')!,redirect:url.searchParams.get('redirect_uri')!,mode,email,sub});
   assert.equal(url.searchParams.get('code_challenge_method'),'S256');const target=new URL(url.searchParams.get('redirect_uri')!);target.searchParams.set('state',url.searchParams.get('state')!);target.searchParams.set('code',code);if(id==='google')target.searchParams.set('iss',origin);res.writeHead(302,{Location:target.href}).end();return;
  }
  res.setHeader('Content-Type','application/json');
  if(kind==='jwks'){res.end(JSON.stringify({keys:[jwk]}));return}
  if(kind==='token'){
   exchanges++;const chunks:Buffer[]=[];for await(const c of req)chunks.push(c);const body=new URLSearchParams(Buffer.concat(chunks).toString()),record=codes.get(body.get('code')!);codes.delete(body.get('code')!);
   if(!record||record.id!==id||record.redirect!==body.get('redirect_uri')||body.get('client_id')!==id+'-client'||body.get('client_secret')!=='fixture-private-'+id||createHash('sha256').update(body.get('code_verifier')||'').digest('base64url')!==record.challenge){res.writeHead(400).end('{}');return}
   const token=randomBytes(24).toString('base64url');access.set(token,{email:record.email,sub:record.sub,verified:record.mode!=='unverified'});
   const now=Math.floor(Date.now()/1000);const claims={sub:record.sub,email:record.email,email_verified:record.mode!=='unverified',name:'Social identity',nonce:record.mode==='nonce'?'wrong':record.nonce};
   const jwt=await new SignJWT(claims).setProtectedHeader({alg:'EdDSA',kid:'fixture-key'}).setIssuer(record.mode==='issuer'?origin+'/wrong':origin).setAudience(record.mode==='audience'?'other-client':id+'-client').setIssuedAt(now).setExpirationTime(record.mode==='expired'?now-30:now+300).sign(record.mode==='signature'?forged.privateKey:key.privateKey);
   res.end(JSON.stringify({access_token:token,refresh_token:'never-persist-this-refresh-token',token_type:'Bearer',expires_in:300,...(id==='google'?{id_token:jwt}:{})}));return;
  }
  const profile=access.get((req.headers.authorization||'').replace(/^Bearer /,''));if(!profile){res.writeHead(401).end('{}');return}
  if(kind==='emails'){res.end(JSON.stringify([{email:profile.email,primary:true,verified:profile.verified}]));return}
  if(kind==='userinfo'){res.end(JSON.stringify(id==='github'?{id:4201,email:'untrusted-profile@example.test',login:'github-user'}:{sub:profile.sub,email:profile.email,email_verified:profile.verified,name:'Vercel identity'}));return}
  res.writeHead(404).end('{}');
 });provider.on('connection',s=>{sockets.add(s);s.on('error',()=>{});s.on('close',()=>sockets.delete(s))});await new Promise<void>(r=>provider.listen(0,'127.0.0.1',r));origin='http://127.0.0.1:'+(provider.address() as net.AddressInfo).port;
 const destination=(path:string)=>({url:origin+path,addresses:['127.0.0.1'],allowLabHTTP:true});
 const providers:AuthOAuthProvider[]=['google','github','vercel'].map(id=>({id,kind:id==='google'?'oidc':id as 'github'|'vercel',clientID:id+'-client',clientSecret:'fixture-private-'+id,authorizationURL:origin+'/authorize/'+id,token:destination('/token/'+id),scopes:id==='github'?['read:user','user:email']:['openid','email','profile'],...(id==='google'?{issuer:origin,jwks:destination('/jwks/google')}:{userInfo:destination('/userinfo/'+id),...(id==='github'?{emails:destination('/emails/github')}:{})})}));
 const branch='br_0123456789abcdef',base='https://auth.example.test/auth/v1/'+branch,c:AuthConfig={version:1,branchID:branch,baseURL:base,databaseURL:dsn,databaseTLS:{caFile:'/run/auth-ca/ca.crt',serverName:'test'},secret:randomBytes(48).toString('base64url'),trustedOrigins:[],allowLabHTTP:true,oauthProviders:providers};
 const auth=createAuth(c,db);let ip=1;
 async function call(path:string,body?:unknown,jar:Jar=new Map(),engine=auth){const res=await engine.handler(new Request(base+'/'+path,{method:body===undefined?'GET':'POST',headers:{Origin:'https://auth.example.test','X-Neon-Client-IP':'192.0.2.'+(ip++),Cookie:[...jar].map(([k,v])=>k+'='+v).join('; '),...(body===undefined?{}:{'Content-Type':'application/json'})},body:body===undefined?undefined:JSON.stringify(body)}));for(const line of res.headers.getSetCookie()){const first=line.split(';')[0],at=first.indexOf('='),k=first.slice(0,at),v=first.slice(at+1);if(!v||/max-age=0/i.test(line))jar.delete(k);else jar.set(k,v)}const raw=await res.text();let value:any;try{value=JSON.parse(raw)}catch{}return{status:res.status,location:res.headers.get('Location'),value}}
 async function begin(id:string,jar:Jar,link=false){const r=await call(link?'link-social':'sign-in/social',{provider:id,callbackURL:'https://auth.example.test/app',newUserCallbackURL:'https://auth.example.test/welcome',errorCallbackURL:'https://auth.example.test/failed',disableRedirect:true},jar);assert.equal(r.status,200);const redirect=await fetch(r.value.url,{redirect:'manual'});assert.equal(redirect.status,302);return new URL(redirect.headers.get('Location')!)}
 const callback=(url:URL,jar:Jar)=>call(url.pathname.slice(new URL(base).pathname.length+1)+url.search,undefined,jar);
 try{
  const jar:Jar=new Map();let url=await begin('google',jar);assert([...jar.keys()].some(k=>k.endsWith('.state')));let r=await callback(url,jar);assert.equal(r.status,302);assert.equal(r.location,'https://auth.example.test/welcome');
  r=await call('get-session',undefined,jar);assert.equal(r.status,200);assert.equal(r.value.user.email,email);const id=r.value.user.id;
  const binding=(await db.query('SELECT * FROM account WHERE "userId"=$1',[id])).rows[0];assert.equal(binding.providerId,'google');assert.equal(binding.accountId,sub);assert.equal(binding.accessToken,null);assert.equal(binding.refreshToken,null);assert.equal(binding.idToken,null);
  assert.equal((await callback(url,jar)).status,400);assert.equal((await call('unlink-account',{accountId:binding.id},jar)).status,400,'Last login was removed');
  assert.equal((await call('sign-out',{},jar)).status,200);url=await begin('google',jar);r=await callback(url,jar);assert.equal(r.status,302,'Repeat callback outcome '+JSON.stringify({code:r.value?.code,message:r.value?.message}));assert.equal(r.location,'https://auth.example.test/app');assert.equal((await call('get-session',undefined,jar)).value.user.id,id);
  assert.equal((await call('sign-in/social',{provider:'google',idToken:{token:'forged'}})).status,400);assert.equal((await call('sign-in/social',{provider:'google',additionalData:{neonAuthProvider:'github'}})).status,400);
  r=await call('sign-in/social',{provider:'google',callbackURL:'https://evil.example.test/app'});assert(r.status>=400);
  r=await call('sign-in/social',{provider:'google',scopes:['administrator'],callbackURL:'https://auth.example.test/app'});assert(r.status>=400);
  for(const invalid of ['nonce','issuer','audience','signature','expired','unverified']){
   mode=invalid;email=invalid+'@example.test';sub='subject-'+invalid;const bad:Jar=new Map();url=await begin('google',bad);r=await callback(url,bad);assert.equal(r.status,302);assert(new URL(r.location!).searchParams.has('error'));assert.equal((await db.query('SELECT count(*)::int n FROM "user" WHERE email=$1',[email])).rows[0].n,0);assert.equal((await call('get-session',undefined,bad)).value,null);
  }
  mode='valid';email='social@example.test';sub='social-subject';const stolen:Jar=new Map();url=await begin('google',stolen);const before=exchanges;assert.equal((await callback(url,new Map())).status,302);assert.equal(exchanges,before,'Missing browser state reached token endpoint');
  const swapped=new URL(url);swapped.pathname=swapped.pathname.replace('/google','/github');assert.equal((await callback(swapped,stolen)).status,400);assert.equal(exchanges,before,'Foreign provider consumed native state');assert.equal((await callback(url,stolen)).status,302);
  email='social@example.test';sub='github-subject';const implicit:Jar=new Map();url=await begin('github',implicit);r=await callback(url,implicit);assert(new URL(r.location!).searchParams.has('error'),'OAuth silently linked a matching email');assert.equal((await db.query('SELECT count(*)::int n FROM account WHERE "userId"=$1',[id])).rows[0].n,1);
  url=await begin('github',jar,true);assert.equal((await callback(url,jar)).location,'https://auth.example.test/app');const accounts=await call('list-accounts',undefined,jar);assert.equal(accounts.status,200);assert.equal(accounts.value.length,2);assert(!JSON.stringify(accounts.value).includes('fixture-private'));assert(!JSON.stringify(accounts.value).includes('accessToken'));
  const linked=(await db.query('SELECT id FROM account WHERE "userId"=$1 AND "providerId"=$2',[id,'github'])).rows[0].id;assert.equal((await call('unlink-account',{accountId:linked},jar)).status,200);
  email='vercel@example.test';sub='vercel-subject';const vercel:Jar=new Map();url=await begin('vercel',vercel);assert.equal((await callback(url,vercel)).status,302);assert.equal((await call('get-session',undefined,vercel)).value.user.email,email);
  // An authenticated explicit link proves both accounts. The first subsequent
  // provider login must consume its verified email proof immediately, without
  // rejecting the callback against the old in-memory local verification flag.
  email='unverified-local@example.test';sub='bound-unverified-subject';const local:Jar=new Map();
  r=await call('sign-up/email',{name:'Unverified local profile',email,password:randomBytes(24).toString('base64url')},local);assert.equal(r.status,200);const localID=r.value.user.id;
  assert.equal((await db.query('SELECT "emailVerified" FROM "user" WHERE id=$1',[localID])).rows[0].emailVerified,false);
  url=await begin('google',local,true);assert.equal((await callback(url,local)).location,'https://auth.example.test/app');
  assert.equal((await db.query('SELECT "emailVerified" FROM "user" WHERE id=$1',[localID])).rows[0].emailVerified,false);
  assert.equal((await call('sign-out',{},local)).status,200);url=await begin('google',local);r=await callback(url,local);assert.equal(r.location,'https://auth.example.test/app');
  r=await call('get-session',undefined,local);assert.equal(r.value.user.id,localID);assert.equal(r.value.user.emailVerified,true);
  // Existing native bans are still enforced for social sign-in.
  await db.query('UPDATE "user" SET banned=true WHERE id=$1',[id]);email='social@example.test';sub='social-subject';const banned:Jar=new Map();url=await begin('google',banned);r=await callback(url,banned);assert(new URL(r.location!).searchParams.has('error'));assert.equal((await call('get-session',undefined,banned)).value,null);
  const disabled=createAuth({...c,oauthProviders:undefined},db);assert.equal((await call('sign-in/social',{provider:'google'},new Map(),disabled)).status,404);
  assert.equal((await call('get-access-token',{providerId:'google'},jar)).status,404,'Provider token management became public');
 }finally{await db.end();await owner.end();for(const s of sockets)s.destroy();await new Promise<void>(r=>provider.close(()=>r()))}
});
