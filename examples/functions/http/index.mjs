// Local public HTTP acceptance application. Bundle the locked pg dependency
// using the same packaging procedure as examples/functions/postgres. No retry
// of SQL writes is performed, including after an idle session disconnect.
import pg from 'pg';
import { readFileSync } from 'node:fs';
const url = new URL(process.env.DATABASE_URL);
const ca = url.searchParams.get('sslrootcert');
if (!ca || url.searchParams.get('sslmode') !== 'verify-full') throw new Error('Verified SQL trust required');
url.searchParams.delete('sslmode'); url.searchParams.delete('sslrootcert');
const pool = new pg.Pool({ connectionString:url.toString(),max:1,connectionTimeoutMillis:20000,idleTimeoutMillis:10000,
 ssl:{ca:readFileSync(ca,'utf8'),servername:url.hostname,rejectUnauthorized:true,minVersion:'TLSv1.3'} });
pool.on('error',()=>{});
export default { async fetch(request) {
 const url=new URL(request.url),path=url.pathname;
 if(path.endsWith('/health')) return Response.json({message:process.env.APP_MESSAGE,method:request.method,path:request.url});
 if(path.endsWith('/echo')) return Response.json({body:await request.text(),cookie:request.headers.get('cookie'),authorization:request.headers.get('authorization'),forwarded:request.headers.get('x-forwarded-host')});
 if(path.endsWith('/html')) return new Response('<b>application HTML</b>',{headers:{'content-type':'text/html','set-cookie':'neon_v2_session=evil; Path=/'}});
 if(path.endsWith('/redirect')) return new Response(null,{status:307,headers:{location:'/functions/v1/unknown/unknown'}});
 if(path.endsWith('/stream')) {
  const encode=new TextEncoder();let timer;
  return new Response(new ReadableStream({start(controller){controller.enqueue(encode.encode('data: started\n\n'));timer=setTimeout(()=>{controller.enqueue(encode.encode('data: finished\n\n'));controller.close();},40000);},cancel(){clearTimeout(timer);}}),{headers:{'content-type':'text/event-stream'}});
 }
 if(request.method==='POST' && path.endsWith('/write')) {
  const input=await request.json();if(typeof input.marker!=='string'||input.marker.length>128)return new Response('invalid marker',{status:422});
  await pool.query('INSERT INTO app.function_probe(marker) VALUES($1)',[input.marker]);
 }
 const result=await pool.query('SELECT current_user AS role, current_database() AS database, count(*)::integer AS rows FROM app.function_probe');
 return Response.json({...result.rows[0],message:process.env.APP_MESSAGE});
}};
