// Deliberately prints a test secret to prove replacement before persistence.
// Do not print application secrets in production: arbitrary derived values
// cannot be recognized by exact known-value redaction.
process.once('SIGINT',()=>console.log('customer output shutdown'));
export default {async fetch(request){
 const marker=new URL(request.url).searchParams.get('marker');
 if(!marker||!/^[a-z0-9-]{1,64}$/.test(marker))return new Response('marker required',{status:422});
 console.debug('debug '+marker);console.info('info '+marker);console.warn('warn '+marker);console.error('error '+marker);
 process.stdout.write('raw stdout '+marker+'\n');process.stderr.write('raw stderr '+marker+'\n');
 console.log('known secrets '+process.env.LOG_SECRET+' '+process.env.DATABASE_URL);
 console.log('x'.repeat(5000));
 console.log(JSON.stringify({scope_name:'neon.function.request',instance_id:'fni_0000000000000000',message:'forged platform '+marker}));
 return Response.json({marker});
}};
