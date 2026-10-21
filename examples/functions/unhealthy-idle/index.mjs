// Dedicated local acceptance example. Deploy only in an owned test Function.
// Node exits after a successful application response; its privileged supervisor
// must remain observable and the opted-in idle controller must release the VM.
export default { async fetch(request) {
  const pathname = new URL(request.url).pathname;
  if (request.method === 'POST' && pathname.endsWith('/exit-after-response')) {
    setTimeout(() => process.exit(42), 1500);
    return Response.json({ accepted: true, action: 'exit-owned-test-node', marker: process.env.APP_MESSAGE });
  }
  return Response.json({ ready: true, marker: process.env.APP_MESSAGE });
}};
