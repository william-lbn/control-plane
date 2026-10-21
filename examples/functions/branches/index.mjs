// Rebundle with the already pinned pg dependency from the PostgreSQL example.
import pg from 'pg';
import { readFileSync } from 'node:fs';
const database = new URL(process.env.DATABASE_URL);
const ca = database.searchParams.get('sslrootcert');
if (!ca || database.searchParams.get('sslmode') !== 'verify-full') throw Error('Verified SQL trust required');
database.searchParams.delete('sslmode'); database.searchParams.delete('sslrootcert');
const pool = new pg.Pool({connectionString:database.toString(),max:1,connectionTimeoutMillis:20000,idleTimeoutMillis:5000,
 ssl:{ca:readFileSync(ca,'utf8'),servername:database.hostname,rejectUnauthorized:true,minVersion:'TLSv1.3'}});
pool.on('error',()=>{});
export default {async fetch(request){
 if(request.method==='POST') {
  const input=await request.json();
  if(typeof input.value!=='string'||input.value.length>100) return new Response('invalid value',{status:422});
  await pool.query('UPDATE app.function_branch_probe SET value=$1 WHERE id=1',[input.value]);
 }
 const result=await pool.query('SELECT current_user AS role,current_database() AS database,value FROM app.function_branch_probe WHERE id=1');
 return Response.json({...result.rows[0],code:'branch-copy-v1',environment:process.env.BRANCH_MARKER});
}};
