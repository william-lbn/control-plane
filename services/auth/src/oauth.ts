import type { BetterAuthPlugin } from "better-auth";
import { createAuthorizationURL, type OAuthProvider, type OAuth2Tokens } from "@better-auth/core/oauth2";
import { createLocalJWKSet, jwtVerify } from "jose";
import { createAuthTransport, validateAuthDestination, type ApprovedAuthDestination } from "./transport.ts";

export interface AuthOAuthProvider {
 id:string;kind:'oidc'|'github'|'vercel';clientID:string;clientSecret:string;authorizationURL:string;
 token:ApprovedAuthDestination;userInfo?:ApprovedAuthDestination;emails?:ApprovedAuthDestination;
 jwks?:ApprovedAuthDestination;issuer?:string;scopes:string[];
}
const unavailable=()=>new Error('Approved OAuth provider unavailable');
const bounded=(v:unknown,max:number):v is string=>typeof v==='string'&&v.length>0&&v.length<=max&&!/[\u0000-\u001f]/.test(v);
export function validateAuthOAuthProviders(providers:AuthOAuthProvider[],allowLabHTTP:boolean){
 if(!Array.isArray(providers)||providers.length<1||providers.length>8||new Set(providers.map(p=>p?.id)).size!==providers.length)throw unavailable();
 for(const p of providers){
  if(!p||Object.keys(p).some(k=>!['id','kind','clientID','clientSecret','authorizationURL','token','userInfo','emails','jwks','issuer','scopes'].includes(k))||!bounded(p.id,48)||!(/^[a-z][a-z0-9_-]*$/).test(p.id)||!['oidc','github','vercel'].includes(p.kind)||!bounded(p.clientID,512)||!bounded(p.clientSecret,4096)||!Array.isArray(p.scopes)||p.scopes.length<1||p.scopes.length>8||new Set(p.scopes).size!==p.scopes.length||p.scopes.some(s=>typeof s!=='string'||!(/^[a-zA-Z0-9:_./-]{1,64}$/).test(s)))throw unavailable();
  for(const d of [p.token,p.userInfo,p.emails,p.jwks])if(d){validateAuthDestination(d);if(d.allowLabHTTP&&!allowLabHTTP)throw unavailable()}
  const url=new URL(p.authorizationURL);
  if(url.href!==p.authorizationURL||url.username||url.password||url.hash||url.search||(url.protocol!=='https:'&&!(allowLabHTTP&&p.token.allowLabHTTP&&url.protocol==='http:')))throw unavailable();
  if(p.kind==='oidc'){
   if(!p.jwks||!p.issuer||!p.scopes.includes('openid'))throw unavailable();const issuer=new URL(p.issuer);
   if(issuer.username||issuer.password||issuer.search||issuer.hash||(issuer.protocol!=='https:'&&!(allowLabHTTP&&p.token.allowLabHTTP&&issuer.protocol==='http:')))throw unavailable();
  }else if(!p.userInfo||p.kind==='github'&&!p.emails)throw unavailable();
 }
}

// The core owns state, PKCE, callback/session handling and account linking.
// Every server-side provider request uses the same bounded, pinned transport.
export function createAuthOAuth(providers:AuthOAuthProvider[],allowLabHTTP:boolean,transport=createAuthTransport()):BetterAuthPlugin{
 validateAuthOAuthProviders(providers,allowLabHTTP);
 async function json(destination:ApprovedAuthDestination,method:'GET'|'POST',body:Buffer,headers:Record<string,string>){
  const result=await transport.request(destination,method,body,{Accept:'application/json',...headers},10000,65536);
  if(result.status!==200)throw unavailable();try{return JSON.parse(result.body.toString())}catch{throw unavailable()}
 }
 const registered:OAuthProvider<Record<string,unknown>>[]=providers.map(p=>({
  id:p.id,name:p.id,issuer:p.issuer,requiresIdTokenNonce:p.kind==='oidc',
  // getUserInfo already requires a verified provider email. The maintained
  // linker upgrades a matching bound local email, but its additional provider
  // verification option reads the pre-upgrade user on the first callback.
  accountSubject:({profile})=>String(profile.sub||''),options:{clientId:p.clientID,clientSecret:p.clientSecret},
  createAuthorizationURL:async data=>{
   if(!data.codeVerifier||p.kind==='oidc'&&!data.idTokenNonce||data.scopes?.some(s=>!p.scopes.includes(s))||data.additionalParams&&Object.keys(data.additionalParams).length)throw unavailable();
   return createAuthorizationURL({id:p.id,options:{clientId:p.clientID,clientSecret:p.clientSecret},authorizationEndpoint:p.authorizationURL,state:data.state,codeVerifier:data.codeVerifier,redirectURI:data.redirectURI,scopes:p.scopes,nonce:data.idTokenNonce,loginHint:data.loginHint});
  },
  validateAuthorizationCode:async data=>{
   if(!bounded(data.code,4096)||!bounded(data.codeVerifier,128))throw unavailable();
   const form=new URLSearchParams({grant_type:'authorization_code',client_id:p.clientID,client_secret:p.clientSecret,code:data.code,code_verifier:data.codeVerifier,redirect_uri:data.redirectURI});
   const token=await json(p.token,'POST',Buffer.from(form.toString()),{'Content-Type':'application/x-www-form-urlencoded'});
   if(!token||!bounded(token.access_token,8192)||typeof token.token_type!=='string'||token.token_type.toLowerCase()!=='bearer'||token.id_token!==undefined&&!bounded(token.id_token,16384)||token.expires_in!==undefined&&(!Number.isInteger(token.expires_in)||token.expires_in<1||token.expires_in>31536000))throw unavailable();
   return {accessToken:token.access_token,idToken:token.id_token,tokenType:'Bearer',accessTokenExpiresAt:new Date(Date.now()+1000*(token.expires_in||3600)),scopes:p.scopes};
  },
  getUserInfo:async(tokens:OAuth2Tokens&{expectedIdTokenNonce?:string})=>{
   try{
    let profile:Record<string,unknown>;
    if(p.kind==='oidc'){
     if(!tokens.idToken||!tokens.expectedIdTokenNonce)throw unavailable();
     const keys=await json(p.jwks!,'GET',Buffer.alloc(0),{});
     if(!keys||!Array.isArray(keys.keys)||keys.keys.length<1||keys.keys.length>32||keys.keys.some((k:any)=>!k||['d','k','p','q'].some(field=>field in k)))throw unavailable();
     const verified=await jwtVerify(tokens.idToken,createLocalJWKSet(keys),{issuer:p.issuer,audience:p.clientID,algorithms:['RS256','ES256','EdDSA'],requiredClaims:['sub','exp','iat','nonce'],maxTokenAge:600,clockTolerance:5});
     profile=verified.payload;
     if(profile.nonce!==tokens.expectedIdTokenNonce||profile.azp!==undefined&&profile.azp!==p.clientID||Array.isArray(profile.aud)&&profile.aud.length>1&&profile.azp!==p.clientID)throw unavailable();
     if(p.userInfo){const info=await json(p.userInfo,'GET',Buffer.alloc(0),{Authorization:'Bearer '+tokens.accessToken});if(info?.sub!==profile.sub)throw unavailable()}
    }else{
     const raw=await json(p.userInfo!,'GET',Buffer.alloc(0),{Authorization:'Bearer '+tokens.accessToken,'User-Agent':'neon-managed-auth'});
     if(p.kind==='github'){
      const emails=await json(p.emails!,'GET',Buffer.alloc(0),{Authorization:'Bearer '+tokens.accessToken,'User-Agent':'neon-managed-auth'});
      if(!Array.isArray(emails)||emails.length>100)throw unavailable();const primary=emails.find(e=>e?.primary===true&&e.verified===true);
      if(!primary||!(typeof raw.id==='string'&&bounded(raw.id,255)||Number.isSafeInteger(raw.id)&&raw.id>0))throw unavailable();
      profile={sub:String(raw.id),email:primary.email,email_verified:true,name:raw.name||raw.login,picture:raw.avatar_url};
     }else profile=raw;
    }
    if(!bounded(profile.sub,255)||!bounded(profile.email,320)||!(/^[^\s@]+@[^\s@]+\.[^\s@]+$/).test(profile.email)||profile.email_verified!==true)throw unavailable();
    return {user:{email:profile.email.toLowerCase(),emailVerified:true,name:bounded(profile.name,500)?profile.name:profile.email,image:bounded(profile.picture,2048)?profile.picture:undefined},data:{sub:profile.sub}};
   }catch{return null}
  },
 }));
 return {id:'approved-auth-oauth',init:async context=>({context:{socialProviders:[...registered,...context.socialProviders]}})};
}

// OAuth is used for identity, not for retaining access to provider APIs. Only
// provider/subject/user bindings survive into native storage and branch copies.
export function omitOAuthTokens(account:Record<string,unknown>){
 // The maintained adapter merges hook data into its input. Explicit NULLs
 // prevent original token fields from surviving that merge.
 const value={...account};for(const field of ['accessToken','refreshToken','idToken','accessTokenExpiresAt','refreshTokenExpiresAt'])value[field]=null;return {data:value};
}
