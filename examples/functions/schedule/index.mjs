// Acceptance example, using only the already authorized app.function_probe
// table. Package with the locked pg dependency as in examples/functions/http.
// Stable invocation IDs are not secrets; trust comes from the supervisor's
// removal of caller X-Neon-* headers and insertion of its signed trigger field.
import pg from 'pg';
import {readFileSync} from 'node:fs';
const database = new URL(process.env.DATABASE_URL);
const ca = database.searchParams.get('sslrootcert');
if (!ca || database.searchParams.get('sslmode') !== 'verify-full') throw Error('Explicit SQL trust required');
database.searchParams.delete('sslmode'); database.searchParams.delete('sslrootcert');
const pool = new pg.Pool({connectionString:database.toString(),max:1,connectionTimeoutMillis:20000,idleTimeoutMillis:5000,
 ssl:{ca:readFileSync(ca,'utf8'),servername:database.hostname,rejectUnauthorized:true,minVersion:'TLSv1.3'}});
pool.on('error',()=>{});
export default {async fetch(request){
 const url=new URL(request.url);
 if(request.method==='GET'&&url.pathname.endsWith('/receipts')){
  const trigger=url.searchParams.get('trigger');if(!/^ftr_[a-f0-9]{24}$/.test(trigger||''))return new Response('trigger required',{status:422});
  const data=await pool.query('SELECT marker FROM app.function_probe WHERE marker LIKE $1 ORDER BY marker LIMIT 20',[trigger+':fti_%']);
  return Response.json({receipts:data.rows.map(row=>row.marker)});
 }
 if(request.method!=='POST'||!url.pathname.endsWith('/scheduled'))return new Response('not found',{status:404});
 const input=await request.json(),id=request.headers.get('x-neon-trigger-invocation-id');
 if(!/^fti_[a-f0-9]{64}$/.test(id||'')||input.version!==1||input.invocation_id!==id||input.trigger?.type!=='schedule'||!/^ftr_[a-f0-9]{24}$/.test(input.trigger.id))return new Response('platform trigger required',{status:403});
 const marker=input.trigger.id+':'+id,client=await pool.connect();
 try{
  await client.query('BEGIN');
  // All copies of this application use one transaction lock for this ID. A
  // production application should normally use its own unique receipt table.
  await client.query('SELECT pg_advisory_xact_lock(hashtextextended($1,0))',[marker]);
  const inserted=await client.query('INSERT INTO app.function_probe(marker) SELECT $1 WHERE NOT EXISTS(SELECT 1 FROM app.function_probe WHERE marker=$1) RETURNING marker',[marker]);
  const proof=await client.query('SELECT current_user AS role,count(*)::integer AS copies FROM app.function_probe WHERE marker=$1',[marker]);
  await client.query('COMMIT');
  return Response.json({invocation_id:id,trigger_id:input.trigger.id,inserted:inserted.rowCount===1,...proof.rows[0]});
 }catch(error){await client.query('ROLLBACK').catch(()=>{});throw error}finally{client.release()}
}};
