// Package with the pinned pg dependency from examples/functions/postgres.
// This acceptance example uses the explicitly granted app.function_probe table.
// A real application should use its own UNIQUE logical-invocation receipt.
import pg from 'pg';
import {readFileSync} from 'node:fs';
const database = new URL(process.env.DATABASE_URL);
const ca = database.searchParams.get('sslrootcert');
if (!ca || database.searchParams.get('sslmode') !== 'verify-full') throw Error('Verified SQL trust required');
database.searchParams.delete('sslmode'); database.searchParams.delete('sslrootcert');
const pool = new pg.Pool({connectionString:database.toString(),max:1,connectionTimeoutMillis:20000,idleTimeoutMillis:5000,
 ssl:{ca:readFileSync(ca,'utf8'),servername:database.hostname,rejectUnauthorized:true,minVersion:'TLSv1.3'}});
pool.on('error',()=>{});
export default {async fetch(request){
 const url = new URL(request.url);
 if(request.method==='GET' && url.pathname.endsWith('/receipts')) {
  const trigger=url.searchParams.get('trigger');
  if(!/^ftr_[a-f0-9]{24}$/.test(trigger||'')) return new Response('trigger required',{status:422});
  const records=await pool.query('SELECT marker FROM app.function_probe WHERE marker LIKE $1 ORDER BY marker LIMIT 100',['object54:'+trigger+':%']);
  return Response.json({receipts:records.rows.map(row=>row.marker)});
 }
 if(request.method!=='POST' || !url.pathname.endsWith('/uploaded')) return new Response('not found',{status:404});
 const input=await request.json(), id=request.headers.get('x-neon-trigger-invocation-id');
 if(!/^fti_[a-f0-9]{64}$/.test(id||'') || input.version!==1 || input.invocation_id!==id || input.trigger?.type!=='storage_object_created' || !/^ftr_[a-f0-9]{24}$/.test(input.trigger.id) || typeof input.data?.object_key!=='string') return new Response('platform object trigger required',{status:403});
 const marker='object54:'+input.trigger.id+':'+id, client=await pool.connect();
 try {
  await client.query('BEGIN');
  await client.query('SELECT pg_advisory_xact_lock(hashtextextended($1,0))',[marker]);
  const counts=await client.query('SELECT count(*)::integer AS attempts FROM app.function_probe WHERE marker LIKE $1',[marker+':attempt:%']);
  const attempt=counts.rows[0].attempts+1;
  await client.query('INSERT INTO app.function_probe(marker) VALUES($1)',[marker+':attempt:'+attempt]);
  await client.query('INSERT INTO app.function_probe(marker) SELECT $1 WHERE NOT EXISTS(SELECT 1 FROM app.function_probe WHERE marker=$1)',[marker+':effect']);
  const proof=await client.query('SELECT current_user AS role,count(*)::integer AS effects FROM app.function_probe WHERE marker=$1',[marker+':effect']);
  await client.query('COMMIT');
  const required=input.data.object_key.endsWith('/redrive.txt')?4:input.data.object_key.endsWith('/once.txt')?2:1;
  return Response.json({invocation_id:id,attempt,object_key:input.data.object_key,...proof.rows[0]},{status:attempt<required?503:200});
 }catch(error){await client.query('ROLLBACK').catch(()=>{});throw error}finally{client.release()}
}};
