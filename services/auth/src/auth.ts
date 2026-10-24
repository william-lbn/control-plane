import { betterAuth } from "better-auth";
import { admin, organization, jwt, emailOTP, magicLink, twoFactor } from "better-auth/plugins";
import type { Pool } from "pg";
import type { AuthConfig } from "./config.ts";
import { authLinkDeliveryEnabled, authOTPDeliveryEnabled } from "./config.ts";
import { APIError } from "better-auth/api";
import { createMailer } from "./mail.ts";
import { transactionalAuthDatabase } from "./transactions.ts";
import { organizationActions } from "./organization.ts";
import { adminActions } from "./admin.ts";
import { createPhonePlugin, phoneActions } from "./phone.ts";
import { createAuthWebhooks, AuthWebhookRejected } from "./webhooks.ts";
import { createAuthOutbox } from "./outbox.ts";
import { createAuthOAuth, omitOAuthTokens } from "./oauth.ts";

export function authOptions(c: AuthConfig, database: Pool, trackOrganizationMail?: (task:Promise<void>)=>Promise<void>, integrations?: {
  webhooks?: ReturnType<typeof createAuthWebhooks>;
  markPhoneRejectedAttempt: () => void;
}) {
  if (
    ((c.requireEmailVerification || c.magicLinkEnabled) && !authLinkDeliveryEnabled(c)) ||
    (c.emailOTPEnabled && !authOTPDeliveryEnabled(c)) || (c.organizationInvitationEmail && !c.mail)
  )
    throw new Error("Email verification requires a mail transport");
  const mail = c.mail ? createMailer(c.mail, c.branchID, c.baseURL) : undefined;
  const hooks = integrations?.webhooks || (c.webhook ? createAuthWebhooks(c.webhook, { branch_id:c.branchID,project_id:c.projectID,endpoint_id:c.endpointID }) : undefined);
  async function sendLink(kind: "password-reset" | "email-verification" | "magic-link", user: { email:string }, url: string) {
    if (hooks?.enabled("send.magic_link")) {
      const base=new URL(c.baseURL), target=new URL(url);
      if(url.length>4096||target.origin!==base.origin||!target.pathname.startsWith(base.pathname+'/')||target.username||target.password||target.hash)throw new Error('Invalid authentication link');
      const seconds=kind==='password-reset'?900:kind==='magic-link'?(c.magicLinkExpiresIn||300):3600;
      const task=hooks.deliver(hooks.envelope('send.magic_link',user,{url,type:kind,expires_at:new Date(Date.now()+seconds*1000).toISOString()}));
      await (trackOrganizationMail?trackOrganizationMail(task):task);
    } else if(mail) await mail.send(kind,user,url);
    else throw new Error('Authentication link delivery unavailable');
  }
  return {
    database,
    secret: c.secret,
    baseURL: c.baseURL,
    basePath: `/auth/v1/${c.branchID}`,
    trustedOrigins: [
      ...new Set([new URL(c.baseURL).origin, ...c.trustedOrigins]),
    ],
    emailAndPassword: {
      enabled: true,
      minPasswordLength: 12,
      maxPasswordLength: 128,
      requireEmailVerification: c.requireEmailVerification ?? false,
      ...(authLinkDeliveryEnabled(c)
        ? {
            revokeSessionsOnPasswordReset: true,
            resetPasswordTokenExpiresIn: 900,
            sendResetPassword: async ({
              user,
              url,
            }: {
              user: { email: string };
              url: string;
            }) => {
              await sendLink("password-reset", user, url);
            },
          }
        : {}),
    },
    ...(authLinkDeliveryEnabled(c)
      ? {
          emailVerification: {
            expiresIn: 3600,
            sendOnSignUp: c.requireEmailVerification ?? false,
            autoSignInAfterVerification: false,
            sendVerificationEmail: async ({
              user,
              url,
            }: {
              user: { email: string };
              url: string;
            }) => {
              await sendLink("email-verification", user, url);
            },
          },
        }
      : {}),
    ...((hooks?.enabled('user.before_create')||c.oauthProviders?.length) ? {databaseHooks:{
      ...(c.oauthProviders?.length?{account:{create:{before:async(account:Record<string,unknown>)=>omitOAuthTokens(account)},update:{before:async(account:Record<string,unknown>)=>omitOAuthTokens(account)}}}:{}),
      ...(hooks?.enabled('user.before_create')?{user:{create:{before:async(user:Record<string,unknown>)=>{
      try { await hooks.deliver(hooks.envelope('user.before_create',user,{})); }
      catch(error){
        if(error instanceof AuthWebhookRejected && error.code!=='AUTH_DELIVERY_UNAVAILABLE' && error.code!=='SIGNUP_VALIDATION_UNAVAILABLE')throw new APIError('FORBIDDEN',{code:error.code,message:error.message});
        throw new APIError('SERVICE_UNAVAILABLE',{code:'SIGNUP_VALIDATION_UNAVAILABLE',message:'Signup validation could not be confirmed'});
      }
      return {data:user};
    }}}}:{}),
    }} : {}),
    ...(c.oauthProviders?.length?{account:{storeStateStrategy:'database' as const,updateAccountOnSignIn:false,storeAccountCookie:false,accountLinking:{enabled:true,disableImplicitLinking:true,allowDifferentEmails:false,requireLocalEmailVerified:true,allowUnlinkingAll:false,trustedProviders:[]}}}:{}),
    session: {
      expiresIn: 86400,
      updateAge: 3600,
      cookieCache: { enabled: false },
    },
    advanced: {
      cookiePrefix: `neon_app_${c.branchID}`,
      // The explicit laboratory HTTP exception must never weaken HTTPS cookies.
      useSecureCookies: new URL(c.baseURL).protocol === "https:",
      // HTTPS applications may use an exact trusted origin on another site.
      // HTTP lab mode remains first-party; SameSite=None requires Secure.
      defaultCookieAttributes: {
        httpOnly: true,
        sameSite:
          new URL(c.baseURL).protocol === "https:"
            ? ("none" as const)
            : ("lax" as const),
        path: `/auth/v1/${c.branchID}`,
      },
      crossSubDomainCookies: { enabled: false },
      // Only the authenticated Go relay supplies this header, never the caller.
      ipAddress: { ipAddressHeaders: ["x-neon-client-ip"] },
    },
    rateLimit: {
      enabled: true,
      window: 60,
      max: 30,
      storage: "database" as const,
    },
    plugins: [
      ...(c.oauthProviders?.length?[createAuthOAuth(c.oauthProviders,c.allowLabHTTP)]:[]),
      // Keep persisted bans enforced even when management endpoints are disabled.
      // Only the explicitly enabled branch exposes the administrative protocol.
      admin({defaultRole:"user",adminRoles:["admin"],impersonationSessionDuration:3600,allowImpersonatingAdmins:false}),
      ...(c.phoneNumberEnabled && hooks ? [createPhonePlugin(database,{secret:c.phoneNumberSecret!,branchID:c.branchID,expiresIn:c.phoneNumberExpiresIn!,webhooks:hooks,markRejectedAttempt:integrations?.markPhoneRejectedAttempt || (()=>{throw new Error('Phone request scope required')})})] : []),
      ...(c.organizationEnabled ? [organization({
        organizationLimit:c.organizationLimit||10,membershipLimit:c.organizationMembershipLimit||100,
        creatorRole:c.organizationCreatorRole||"owner",teams:{enabled:false},dynamicAccessControl:{enabled:false},
        requireEmailVerificationOnInvitation:true,invitationExpiresIn:172800,invitationLimit:100,
        organizationHooks:{beforeDeleteOrganization:async({organization}:{organization:{id:string}})=>{
          // The maintained endpoint has already checked this organization's
          // delete permission. Its adapter removes members before the parent.
          // The Neon extension reserves neon.* for its server settings.
          await database.query("SELECT set_config('neon_control.auth_deleting_organization',$1,true)",[organization.id]);
        }},
        ...(c.organizationInvitationEmail&&mail ? {sendInvitationEmail:async(data:{id:string;email:string})=>{
          const task=mail.sendOrganizationInvitation(data.email,data.id,c.organizationInvitationURL!);
          await (trackOrganizationMail?trackOrganizationMail(task):task);
        }} : {}),
      })] : []),
      ...(c.twoFactorEnabled ? [twoFactor({issuer:"Neon Self Hosted",skipVerificationOnEnable:false,twoFactorCookieMaxAge:300,backupCodeOptions:{storeBackupCodes:"encrypted"}})] : []),
      ...(authLinkDeliveryEnabled(c) && c.magicLinkEnabled
        ? [
            magicLink({
              expiresIn: c.magicLinkExpiresIn || 300,
              disableSignUp: !c.magicLinkAllowRegistration,
              storeToken: "hashed",
              sendMagicLink: async ({ email, url }) => {
                await sendLink("magic-link", { email }, url);
              },
            }),
          ]
        : []),
      ...(authOTPDeliveryEnabled(c) && c.emailOTPEnabled
        ? [
            emailOTP({
              otpLength: 6,
              expiresIn: 300,
              allowedAttempts: 3,
              storeOTP: "hashed",
              disableSignUp: false,
              sendVerificationOnSignUp: false,
              sendVerificationOTP: async ({ email, otp, type }) => {
                if(hooks?.enabled('send.otp')) {
                  const task=hooks.deliver(hooks.envelope('send.otp',{email},{otp_code:otp,otp_type:type,delivery_preference:'email',expires_at:new Date(Date.now()+300000).toISOString()}));
                  await (trackOrganizationMail?trackOrganizationMail(task):task);
                } else if(mail) await mail.sendOTP(type, email, otp);
              },
            }),
          ]
        : []),
      jwt({
        jwt: {
          issuer: c.baseURL,
          audience: c.branchID,
          expirationTime: "5m",
          definePayload: async ({ user,session }: { user: { id: string };session?:{id?:string;activeOrganizationId?:string|null} }) => {
            const claims:Record<string,string>={branch_id:c.branchID,sub:user.id};
            if(c.organizationEnabled&&session?.activeOrganizationId){
              const member=await database.query('SELECT m.role FROM member m JOIN organization o ON o.id=m."organizationId" WHERE m."userId"=$1 AND m."organizationId"=$2',[user.id,session.activeOrganizationId]);
              if(member.rows.length===1){claims.organization_id=session.activeOrganizationId;claims.organization_role=member.rows[0].role}
            }
            return claims;
          },
        },
        jwks: {
          keyPairConfig: { alg: "EdDSA" as const, crv: "Ed25519" as const },
        },
      }),
    ],
    // Library errors must not emit passwords, DSNs or private signing material.
    logger: { disabled: true },
  };
}
export function createAuth(c: AuthConfig, database: Pool) {
  const transactions=transactionalAuthDatabase(database);
  const hooks=c.webhook?createAuthWebhooks(c.webhook,{branch_id:c.branchID,endpoint_id:c.endpointID,project_id:c.projectID}):undefined;
  const outbox=hooks?createAuthOutbox(transactions.database,c.branchID,hooks,c.webhook!.events):undefined;
  const auth = betterAuth(authOptions(c, transactions.database,transactions.trackOrganizationMail,{webhooks:hooks,markPhoneRejectedAttempt:transactions.markPhoneRejectedAttempt}));
  const handler = auth.handler;
  auth.handler = async (request: Request) => {
    const action=new URL(request.url).pathname.slice(`/auth/v1/${c.branchID}/`.length);
    if(['get-access-token','refresh-token','account-info'].includes(action))return new Response(null,{status:404});
    if(action==='sign-in/social'||action==='link-social'){
      if(!c.oauthProviders?.length||request.method!=='POST')return new Response(null,{status:404});
      let body:any;try{body=await request.clone().json()}catch{return Response.json({code:'INVALID_REQUEST'},{status:400})}
      if(!body||!c.oauthProviders.some(p=>p.id===body.provider)||body.idToken!==undefined||body.additionalData!==undefined)return Response.json({code:'INVALID_OAUTH_REQUEST'},{status:400});
      request=new Request(request,{body:JSON.stringify({...body,additionalData:{neonAuthProvider:body.provider}})});
    }
    if(action.startsWith('callback/')&&(!c.oauthProviders?.some(p=>action==='callback/'+p.id)||request.method!=='GET'))return new Response(null,{status:404});
    if(action.startsWith('callback/')){
      const states=new URL(request.url).searchParams.getAll('state');
      if(states.length!==1||!(/^[a-zA-Z0-9_-]{32}$/).test(states[0]))return Response.json({code:'INVALID_OAUTH_STATE'},{status:400});
      const saved=await database.query('SELECT value FROM verification WHERE identifier=$1 AND "expiresAt">now()',['auth-state:'+states[0]]);
      if(saved.rows.length!==1)return Response.json({code:'INVALID_OAUTH_STATE'},{status:400});
      let state:any;try{state=JSON.parse(saved.rows[0].value)}catch{return Response.json({code:'INVALID_OAUTH_STATE'},{status:400})}
      if(state.neonAuthProvider!==action.slice('callback/'.length))return Response.json({code:'INVALID_OAUTH_PROVIDER'},{status:400});
    }
    if(['list-accounts','unlink-account'].includes(action)&&!c.oauthProviders?.length)return new Response(null,{status:404});
    if (action.startsWith('phone-number/') && (!c.phoneNumberEnabled || !Object.hasOwn(phoneActions,action) || (request.method!=='POST'&&request.method!=='OPTIONS')))
      return new Response(null,{status:404,headers:{"Cache-Control":"no-store"}});
    if(action==='.well-known/jwks.json'&&hooks&&request.method==='GET')return Response.json(hooks.jwks(),{headers:{'Cache-Control':'no-store'}});
    if(action.startsWith('webhooks/')){
      if(!hooks||!outbox||!c.adminEnabled||!['webhooks/events','webhooks/drain'].includes(action)||
        (action==='webhooks/events'?request.method!=='GET':request.method!=='POST'))return new Response(null,{status:404});
      const session=await auth.api.getSession({headers:request.headers});
      if(!session?.user||!String((session.user as {role?:string}).role||'').split(',').includes('admin'))return Response.json({code:'FORBIDDEN',message:'Application administrator session required'},{status:403});
      return Response.json(action==='webhooks/events'?await outbox.list():await outbox.drain(3),{headers:{'Cache-Control':'no-store'}});
    }
    if (action.startsWith('admin/')&&(!c.adminEnabled||!Object.hasOwn(adminActions,action)||(request.method!==adminActions[action]&&request.method!=='OPTIONS')))
      return new Response(null,{status:404,headers:{"Cache-Control":"no-store"}});
    if (action.startsWith('organization/')&&(!c.organizationEnabled||!Object.hasOwn(organizationActions,action)||(request.method!==organizationActions[action]&&request.method!=='OPTIONS')))
      return new Response(null,{status:404,headers:{"Cache-Control":"no-store"}});
    const response=await transactions.handle(request,handler);
    if((request.method==='POST'||action.startsWith('callback/'))&&response.status<400&&outbox?.enabled){
      // Only activity-triggered work: never poll or wake an idle Compute.
      // The native mutation and its queue row have already committed.
      void outbox.schedule().catch(()=>{console.error('{"event":"auth_event_drain_unavailable"}')});
    }
    return response;
  };
  return auth;
}
