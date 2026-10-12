import pg from 'pg';
import { readFileSync } from 'node:fs';

// DATABASE_URL is injected by the trusted supervisor for this exact Function.
// Never log it. Supply explicit CA/hostname/TLS rather than ambient PG settings.
const url = new URL(process.env.DATABASE_URL);
const caFile = url.searchParams.get('sslrootcert');
if (!caFile || url.searchParams.get('sslmode') !== 'verify-full') throw new Error('Verified SQL trust required');
url.searchParams.delete('sslmode'); url.searchParams.delete('sslrootcert');
const pool = new pg.Pool({ connectionString: url.toString(), max: 1,
  connectionTimeoutMillis: 20000, idleTimeoutMillis: 10000,
  ssl: { ca: readFileSync(caFile, 'utf8'), servername: url.hostname, rejectUnauthorized: true, minVersion: 'TLSv1.3' } });

export default { async fetch(request) {
  const path = new URL(request.url).pathname;
  if (path.endsWith('/html')) return new Response('<script>globalThis.functionXSSExecuted=true</script><b>untrusted application HTML</b>', {headers:{'content-type':'text/html'}});
  if (path.endsWith('/error')) return new Response('intentional application error', {status:500});
  if (path.endsWith('/deny')) {
    try { await pool.query('SELECT * FROM private_area.secrets'); return Response.json({denied:false}); }
    catch (error) { return Response.json({denied:error.code==='42501'}); }
  }
  if (request.method==='POST' && path.endsWith('/write')) {
    const input=await request.json();
    if (typeof input.marker!=='string'||input.marker.length>128) return new Response('invalid marker',{status:422});
    await pool.query('INSERT INTO app.function_probe(marker) VALUES($1)',[input.marker]);
  }
  const result=await pool.query('SELECT current_user AS role, current_database() AS database, count(*)::integer AS rows FROM app.function_probe');
  return Response.json({...result.rows[0],message:process.env.APP_MESSAGE||'hello'});
}};
