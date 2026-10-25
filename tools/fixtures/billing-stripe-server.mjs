// Synthetic Stripe protocol fixture. No card, commercial merchant or money.
// Private config/state are provisioned outside the source tree on Linux.
import http from 'node:http';
import { randomBytes, createHmac, timingSafeEqual } from 'node:crypto';
import { readFile, writeFile, rename } from 'node:fs/promises';

const file = process.env.NEON_BILLING_FIXTURE_CONFIG;
if (process.platform !== 'linux' || !file?.startsWith('/run/')) throw Error('Protected Linux fixture config required');
const config = JSON.parse(await readFile(file, 'utf8'));
if (!config.private_fixture || config.livemode !== false || !config.api_key || !config.admin_key || !config.webhook_secret || !config.state_file?.startsWith('/run/state/')) throw Error('Explicit synthetic fixture approval required');
let state;
try { state = JSON.parse(await readFile(config.state_file, 'utf8')); }
catch (e) { if (e.code !== 'ENOENT') throw e; state = { sessions: {}, intents: {}, charges: {}, disputes: {}, posts: 0, events: [], deliveries: [], unknown_next: false }; }
const save = async () => { await writeFile(config.state_file + '.tmp', JSON.stringify(state), { mode: 0o600 }); await rename(config.state_file + '.tmp', config.state_file); };
const id = (prefix) => prefix + randomBytes(18).toString('hex');
const equal = (a,b) => typeof a === 'string' && typeof b === 'string' && a.length === b.length && timingSafeEqual(Buffer.from(a),Buffer.from(b));
async function body(req) { const parts=[]; let size=0; for await(const part of req){size+=part.length;if(size>1048576)throw Error('Body limit');parts.push(part);}return Buffer.concat(parts).toString('utf8'); }
const json = (res,code,value) => { res.writeHead(code,{'Content-Type':'application/json','Cache-Control':'no-store'});res.end(JSON.stringify(value)); };
async function emit(type,object) {
  const event={id:id('evt_'),object:'event',type,api_version:config.api_version,created:Math.floor(Date.now()/1000),livemode:false,data:{object:{id:object.id,object:object.object}}};
  state.events.push(event);await save();
  const raw=JSON.stringify(event),stamp=String(Math.floor(Date.now()/1000)),signature=createHmac('sha256',config.webhook_secret).update(stamp+'.'+raw).digest('hex');
  try {
    const response=await fetch(config.webhook_url,{method:'POST',redirect:'error',headers:{'Content-Type':'application/json','Stripe-Signature':'t='+stamp+',v1='+signature},body:raw,signal:AbortSignal.timeout(15000)});
    state.deliveries.push({event_id:event.id,status:response.status});await response.arrayBuffer();await save();
  } catch { state.deliveries.push({event_id:event.id,status:'unknown'});await save(); }
  return event.id;
}
async function handle(req,res){
  const u=new URL(req.url,'http://fixture.invalid'),path=u.pathname;
  if(path==='/healthz'){res.end('ok');return;}
  if(path.startsWith('/v1/')){
    if(!equal(req.headers.authorization,'Bearer '+config.api_key)){json(res,403,{error:'fixture_provider_authority'});return;}
    if(req.headers['stripe-version']!==config.api_version){json(res,400,{error:'version'});return;}
    if(req.method==='GET'&&path==='/v1/account'){json(res,200,{id:config.account_id,object:'account'});return;}
    if(req.method==='POST'&&path==='/v1/checkout/sessions'){
      const f=new URLSearchParams(await body(req)),invoice=f.get('metadata[neon_invoice]'),binding=f.get('metadata[neon_binding]'),amount=Number(f.get('line_items[0][price_data][unit_amount]'));
      if(!/^bil_[a-f0-9]{24}$/.test(invoice)||!/^[a-f0-9]{64}$/.test(binding)||f.get('client_reference_id')!==invoice||f.get('payment_intent_data[metadata][neon_binding]')!==binding||f.get('payment_intent_data[metadata][neon_invoice]')!==invoice||f.get('mode')!=='payment'||f.get('payment_method_types[0]')!=='card'||f.get('line_items[0][quantity]')!=='1'||f.get('line_items[0][price_data][currency]')!=='usd'||!Number.isSafeInteger(amount)||amount<50||amount>99999999||req.headers['idempotency-key']!=='neon-checkout:'+invoice){json(res,400,{error:'binding'});return;}
      const existing=Object.values(state.sessions).find(s=>s.metadata.neon_invoice===invoice);
      if(existing){json(res,409,{error:'fixture_never_recreate_original'});return;}
      const session=id('cs_'),token=randomBytes(24).toString('base64url');
      const value={id:session,object:'checkout.session',mode:'payment',status:'open',payment_status:'unpaid',livemode:false,amount_total:amount,currency:'usd',client_reference_id:invoice,expires_at:Math.floor(Date.now()/1000)+3600,url:config.checkout_origin+'/c/pay/'+session+'#'+token,payment_intent:null,metadata:{neon_invoice:invoice,neon_binding:binding}};
      state.sessions[session]=value;state.posts++;await save();
      if(state.unknown_next){state.unknown_next=false;await save();req.socket.destroy();return;}
      json(res,200,value);return;
    }
    if(req.method==='GET'&&path==='/v1/checkout/sessions'){
      if(!/^pi_[a-f0-9]+$/.test(u.searchParams.get('payment_intent'))||u.searchParams.get('limit')!=='2'){json(res,400,{error:'bounded_original_intent_required'});return;}
      json(res,200,{object:'list',has_more:false,data:Object.values(state.sessions).filter(s=>s.payment_intent===u.searchParams.get('payment_intent'))});return;
    }
    const routes=[['/v1/checkout/sessions/',state.sessions],['/v1/payment_intents/',state.intents],['/v1/charges/',state.charges],['/v1/disputes/',state.disputes]];
    for(const [prefix,table] of routes)if(req.method==='GET'&&path.startsWith(prefix)){const value=table[path.slice(prefix.length)];json(res,value?200:404,value||{error:'missing_original'});return;}
    json(res,404,{error:'unsupported_fixture_operation'});return;
  }
  if(req.method==='GET'&&path.startsWith('/c/pay/')){
    const session=state.sessions[path.slice('/c/pay/'.length)];if(!session){json(res,404,{error:'missing'});return;}
    res.writeHead(200,{'Content-Type':'text/html; charset=utf-8','Cache-Control':'no-store','Referrer-Policy':'no-referrer','Content-Security-Policy':"default-src 'none'; script-src 'unsafe-inline'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'"});
    res.end(`<!doctype html><meta charset="utf-8"><title>合成支付验收</title><h1>本地合成收银台</h1><p>不会收取真实款项。</p><button id="pay">确认合成付款</button><p role="status" id="result"></p><script>document.getElementById('pay').onclick=async()=>{const r=await fetch('/fixture/pay/'+${JSON.stringify(session.id)},{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({token:location.hash.slice(1)})});document.getElementById('result').textContent=r.ok?'合成付款回执已提交':'请核对原订单';};</script>`);return;
  }
  if(req.method==='POST'&&path.startsWith('/fixture/pay/')){
    const s=state.sessions[path.slice('/fixture/pay/'.length)],input=JSON.parse(await body(req));
    if(!s||!equal(input.token,new URL(s.url).hash.slice(1))||s.status!=='open'){json(res,409,{error:'original_checkout_required'});return;}
    const pi=id('pi_'),ch=id('ch_');s.status='complete';s.payment_status='paid';s.payment_intent=pi;s.url=null;
    state.intents[pi]={id:pi,object:'payment_intent',status:'succeeded',livemode:false,amount:s.amount_total,amount_received:s.amount_total,currency:'usd',latest_charge:ch,metadata:s.metadata};
    state.charges[ch]={id:ch,object:'charge',payment_intent:pi,livemode:false,amount:s.amount_total,amount_captured:s.amount_total,amount_refunded:0,currency:'usd',paid:true,captured:true,disputed:false};
    await save();const event=await emit('checkout.session.completed',s);json(res,200,{event_id:event,session_id:s.id});return;
  }
  if(path.startsWith('/fixture/admin/')){
    if(!equal(req.headers.authorization,'Bearer '+config.admin_key)){json(res,403,{error:'fixture_operator_authority'});return;}
    if(req.method==='GET'&&path==='/fixture/admin/state'){json(res,200,state);return;}
    const input=JSON.parse(await body(req));
    if(req.method==='POST'&&path==='/fixture/admin/unknown-next'){state.unknown_next=true;await save();json(res,200,{armed:true});return;}
    const s=state.sessions[input.session_id];if(!s){json(res,404,{error:'original_session_required'});return;}
    if(req.method==='POST'&&path==='/fixture/admin/refund'){
      const charge=state.charges[state.intents[s.payment_intent]?.latest_charge];
      if(!charge||!Number.isSafeInteger(input.amount_minor)||input.amount_minor<charge.amount_refunded||input.amount_minor>charge.amount){json(res,409,{error:'refund_bound'});return;}
      charge.amount_refunded=input.amount_minor;await save();json(res,200,{event_id:await emit('charge.refunded',charge)});return;
    }
    if(req.method==='POST'&&path==='/fixture/admin/dispute'){
      const charge=state.charges[state.intents[s.payment_intent]?.latest_charge];
      if(!charge||!['under_review','won','lost','prevented'].includes(input.status)){json(res,409,{error:'dispute_bound'});return;}
      let d=Object.values(state.disputes).find(v=>v.charge===charge.id);
      if(!d){if(!Number.isSafeInteger(input.amount_minor)||input.amount_minor<1||input.amount_minor>99999999){json(res,409,{error:'dispute_amount'});return;}d={id:id('du_'),object:'dispute',charge:charge.id,payment_intent:null,amount:input.amount_minor,currency:'usd',livemode:false};state.disputes[d.id]=d;}
      d.status=input.status;charge.disputed=input.status==='under_review';await save();json(res,200,{dispute_id:d.id,event_id:await emit('charge.dispute.updated',d)});return;
    }
    if(req.method==='POST'&&path==='/fixture/admin/replay'){
      const original=state.events.find(v=>v.id===input.event_id);if(!original){json(res,404,{error:'original_event_required'});return;}
      const raw=JSON.stringify(original),stamp=String(Math.floor(Date.now()/1000)),signature=createHmac('sha256',config.webhook_secret).update(stamp+'.'+raw).digest('hex');
      const response=await fetch(config.webhook_url,{method:'POST',redirect:'error',headers:{'Content-Type':'application/json','Stripe-Signature':'t='+stamp+',v1='+signature},body:raw,signal:AbortSignal.timeout(15000)});await response.arrayBuffer();json(res,200,{status:response.status});return;
    }
    json(res,404,{error:'unsupported_fixture_admin_operation'});return;
  }
  json(res,404,{error:'missing'});
}
// Serialize the tiny fixture's mutations; no retry of an unknown payment POST.
let queue=Promise.resolve();
const server=http.createServer((req,res)=>{queue=queue.then(()=>handle(req,res)).catch(()=>{if(!res.headersSent)json(res,500,{error:'synthetic_fixture_failure'});else res.destroy();});});
await save();server.listen(config.port,'0.0.0.0');
