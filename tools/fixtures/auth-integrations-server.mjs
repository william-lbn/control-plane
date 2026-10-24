// Explicit laboratory protocol fixture. It has automatic synthetic identity
// selection and private receipt inspection; never expose it as a real IdP/SMS.
import http from 'node:http';
import {readFileSync} from 'node:fs';
import {randomBytes,randomUUID,createHash,createPublicKey,verify,timingSafeEqual} from 'node:crypto';
import {generateKeyPair,exportJWK,SignJWT} from 'jose';

const config=JSON.parse(readFileSync(process.env.AUTH_LAB_CONFIG_FILE||'/run/lab/fixture.json','utf8'));
if(config.version!==1||config.laboratoryAcknowledged!==true||!/^prj_[a-f0-9]{16}$/.test(config.projectID)||!Array.isArray(config.clients)||config.clients.length<1||config.clients.length>8||typeof config.receiptToken!=='string'||config.receiptToken.length<32)throw Error('Explicit bounded laboratory configuration required');
const origin=new URL(config.origin),control=new URL(config.controlOrigin);
if(origin.protocol!=='http:'||control.protocol!=='http:'||[origin,control].some(u=>u.username||u.password||u.pathname!=='/'||u.search||u.hash))throw Error('Fixed laboratory origins required');
const clients=new Map(config.clients.map(c=>[c.id,c]));
if(clients.size!==config.clients.length||config.clients.some(c=>!(/^[a-z][a-z0-9_-]{0,47}$/).test(c.id)||!['oidc','github','vercel'].includes(c.kind)||typeof c.clientID!=='string'||typeof c.clientSecret!=='string'||c.clientSecret.length<32))throw Error('Independent laboratory clients required');
const key=await generateKeyPair('EdDSA'),jwk={...await exportJWK(key.publicKey),kid:'lab-'+randomBytes(8).toString('hex'),alg:'EdDSA',use:'sig'};
const codes=new Map(),tokens=new Map(),events=new Map(),publicKeys=new Map();
let identity={subject:'lab-subject',email:'lab-identity@example.test',name:'Laboratory identity'};
const secretEquals=(left,right)=>typeof left==='string'&&Buffer.byteLength(left)===Buffer.byteLength(right)&&timingSafeEqual(Buffer.from(left),Buffer.from(right));
const admin=req=>secretEquals((req.headers.authorization||'').replace(/^Bearer /,''),config.receiptToken);
function boundedMap(map,key,value,max){map.set(key,value);if(map.size>max)map.delete(map.keys().next().value)}
async function body(req,max=32768){const chunks=[];let length=0;for await(const c of req){length+=c.length;if(length>max)throw Error('Body limit');chunks.push(c)}return Buffer.concat(chunks)}
function send(res,status,value){res.writeHead(status,{'Content-Type':'application/json','Cache-Control':'no-store'}).end(JSON.stringify(value))}
function callbackAllowed(raw,id){try{const u=new URL(raw);return u.origin===control.origin&&u.pathname.startsWith('/auth/v1/br_')&&/^\/auth\/v1\/br_[a-f0-9]{16}\/callback\/[a-z][a-z0-9_-]{0,47}$/.test(u.pathname)&&u.pathname.endsWith('/'+id)&&!u.username&&!u.password&&!u.search&&!u.hash}catch{return false}}
async function acceptEvent(raw,headers){
 const value=JSON.parse(raw.toString()),branch=value.context?.branch_id;
 if(value.context?.project_id!==config.projectID||!/^br_[a-f0-9]{16}$/.test(branch)||!/^ep_[a-f0-9]{16}$/.test(value.context?.endpoint_id)||['password','accessToken','refreshToken','idToken'].some(field=>Object.hasOwn(value.user||{},field))||headers['x-neon-event-id']!==value.event_id||headers['x-neon-event-type']!==value.event_type)throw Error('Event scope');
 const stamp=String(headers['x-neon-timestamp']||'');if(!/^\d{13}$/.test(stamp)||Math.abs(Date.now()-Number(stamp))>90000)throw Error('Event timestamp');
 let keys=publicKeys.get(branch);if(!keys||keys.expires<Date.now()){
  const response=await fetch(control.origin+'/auth/v1/'+branch+'/.well-known/jwks.json',{signal:AbortSignal.timeout(8000)});if(!response.ok)throw Error('Branch public key unavailable');const result=await response.json();if(!Array.isArray(result.keys)||result.keys.length!==1||result.keys[0].d)throw Error('Branch public key invalid');keys={expires:Date.now()+300000,value:result.keys};publicKeys.set(branch,keys);
 }
 const [header,empty,signature]=String(headers['x-neon-signature']||'').split('.'),protectedHeader=JSON.parse(Buffer.from(header,'base64url').toString());
 const publicKey=keys.value.find(k=>k.kid===protectedHeader.kid);if(!publicKey||protectedHeader.alg!=='EdDSA'||empty!==''||headers['x-neon-signature-kid']!==publicKey.kid)throw Error('Event signature header');
 const signed=header+'.'+Buffer.from(stamp+'.'+raw.toString('base64url')).toString('base64url');if(!verify(null,Buffer.from(signed),createPublicKey({key:publicKey,format:'jwk'}),Buffer.from(signature,'base64url')))throw Error('Event signature');
 const digest=createHash('sha256').update(raw).digest('hex'),prior=events.get(value.event_id);
 if(prior&&prior.digest!==digest)throw Error('Event identity changed');
 if(!prior)boundedMap(events,value.event_id,{event:value,digest,verified_signature:true,attempts:[]},1000);
 const record=events.get(value.event_id);record.attempts.push(Number(headers['x-neon-delivery-attempt']));
 return value;
}
const server=http.createServer(async(req,res)=>{
 try{
  const url=new URL(req.url,'http://lab'),parts=url.pathname.split('/').filter(Boolean),[action,id]=parts;
  if(url.pathname==='/readyz'){res.end('ok');return}
  if(action==='fixture'){
   if(!admin(req)){send(res,403,{});return}
   if(id==='events'&&req.method==='GET'){const branch=url.searchParams.get('branch');if(!/^br_[a-f0-9]{16}$/.test(branch)){send(res,400,{});return}send(res,200,{items:[...events.values()].filter(e=>e.event.context.branch_id===branch)});return}
   if(id==='identity'&&req.method==='POST'){const value=JSON.parse((await body(req,2048)).toString());if(typeof value.subject!=='string'||!(/^[a-zA-Z0-9_-]{1,128}$/).test(value.subject)||typeof value.email!=='string'||!(/^[a-z0-9._+-]+@example\.test$/).test(value.email)||typeof value.name!=='string'||value.name.length>100){send(res,400,{});return}identity={subject:value.subject,email:value.email,name:value.name};send(res,200,{updated:true});return}
   send(res,404,{});return
  }
  if(action==='hook'&&req.method==='POST'){const event=await acceptEvent(await body(req),req.headers);send(res,200,event.event_type==='user.before_create'?{allowed:!event.user.email?.startsWith('deny-'),error_code:'LAB_SIGNUP_DENIED',error_message:'Laboratory signup policy rejected this identity'}:{});return}
  const client=clients.get(id);if(!client){send(res,404,{});return}
  if(action==='jwks'&&req.method==='GET'){send(res,200,{keys:[jwk]});return}
  if(action==='authorize'&&req.method==='GET'){
   const redirect=url.searchParams.get('redirect_uri'),state=url.searchParams.get('state'),challenge=url.searchParams.get('code_challenge'),nonce=url.searchParams.get('nonce');
   if(url.searchParams.get('client_id')!==client.clientID||url.searchParams.get('response_type')!=='code'||!callbackAllowed(redirect,id)||!(/^[a-zA-Z0-9_-]{32}$/).test(state||'')||!(/^[a-zA-Z0-9_-]{43}$/).test(challenge||'')||url.searchParams.get('code_challenge_method')!=='S256'||client.kind==='oidc'&&!(/^[a-zA-Z0-9_-]{32}$/).test(nonce||'')){send(res,400,{});return}
   const code=randomUUID();boundedMap(codes,code,{client,redirect,challenge,nonce,identity:{...identity},expires:Date.now()+120000},256);const target=new URL(redirect);target.searchParams.set('state',state);target.searchParams.set('code',code);if(client.kind==='oidc')target.searchParams.set('iss',origin.origin);res.writeHead(302,{Location:target.href,'Cache-Control':'no-store'}).end();return
  }
  if(action==='token'&&req.method==='POST'){
   const form=new URLSearchParams((await body(req,16384)).toString()),record=codes.get(form.get('code'));codes.delete(form.get('code'));
   if(!record||record.expires<Date.now()||record.client.id!==id||form.get('grant_type')!=='authorization_code'||form.get('client_id')!==client.clientID||!secretEquals(form.get('client_secret'),client.clientSecret)||form.get('redirect_uri')!==record.redirect||createHash('sha256').update(form.get('code_verifier')||'').digest('base64url')!==record.challenge){send(res,400,{});return}
   const token=randomBytes(32).toString('base64url');boundedMap(tokens,token,{...record.identity,expires:Date.now()+300000,client:id},256);
   const jwt=client.kind==='oidc'?await new SignJWT({sub:record.identity.subject,email:record.identity.email,email_verified:true,name:record.identity.name,nonce:record.nonce}).setProtectedHeader({alg:'EdDSA',kid:jwk.kid}).setIssuer(origin.origin).setAudience(client.clientID).setIssuedAt().setExpirationTime('5m').sign(key.privateKey):undefined;
   send(res,200,{access_token:token,token_type:'Bearer',expires_in:300,...(jwt?{id_token:jwt}:{})});return
  }
  const profile=tokens.get((req.headers.authorization||'').replace(/^Bearer /,''));if(!profile||profile.client!==id||profile.expires<Date.now()){send(res,401,{});return}
  if(action==='emails'&&client.kind==='github'){send(res,200,[{email:profile.email,primary:true,verified:true}]);return}
  if(action==='userinfo'){send(res,200,client.kind==='github'?{id:profile.subject,login:profile.name}:{sub:profile.subject,email:profile.email,email_verified:true,name:profile.name});return}
  send(res,404,{});
 }catch{if(!res.headersSent)send(res,400,{code:'LAB_PROTOCOL_REJECTED'});else res.destroy()}
});
server.requestTimeout=15000;server.headersTimeout=5000;server.listen(9083,'0.0.0.0');
process.on('SIGTERM',()=>server.close(()=>process.exit(0)));
